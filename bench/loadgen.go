package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

const (
	defaultEndpoint     = "http://127.0.0.1:4566"
	loadRegion          = "us-east-1"
	messagesPerCycle    = 10
	maxReceiveRounds    = 100
	maxWorkerErrors     = 50
	dialTimeout         = 5 * time.Second
	idleConnTimeout     = 90 * time.Second
	percentMedian       = 50
	percentTail         = 99
	percentScale        = 100
	microsPerMilli      = 1000
	defaultLoadDuration = 2 * time.Second
	defaultCycles       = 200
	defaultCycleConc    = 16
	idleConnsPerWork    = 2
)

// opStats is the latency summary of one operation.
type opStats struct {
	Count  int   `json:"count"`
	Errors int   `json:"errors"`
	P50us  int64 `json:"p50_us"`
	P99us  int64 `json:"p99_us"`
	MeanUS int64 `json:"mean_us"`
}

// usage is what the orchestrator reads from the target's cgroup around a run.
type usage struct {
	BaselineRSSBytes uint64  `json:"baseline_rss_bytes"`
	EndRSSBytes      uint64  `json:"end_rss_bytes"`
	PeakRSSBytes     uint64  `json:"peak_rss_bytes"`
	MedianRSSBytes   uint64  `json:"median_rss_bytes"`
	CPUPercent       float64 `json:"cpu_percent"`
}

// agenticRun is the create, use and delete workload.
type agenticRun struct {
	Cycles      int     `json:"cycles"`
	Concurrency int     `json:"concurrency"`
	Seconds     float64 `json:"seconds"`
	Errors      int     `json:"errors"`
	CycleP50ms  float64 `json:"cycle_p50_ms"`
	CycleP99ms  float64 `json:"cycle_p99_ms"`
	Usage       usage   `json:"usage,omitzero"`
}

// loadgenMain runs inside the load generator container.
func loadgenMain(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bench loadgen cell|agentic [flags]")
	}

	var (
		result any
		err    error
	)

	switch args[0] {
	case "cell":
		result, err = cellCommand(ctx, args[1:])
	case "agentic":
		result, err = agenticCommand(ctx, args[1:])
	default:
		return fmt.Errorf("unknown workload %q, want cell or agentic", args[0])
	}

	if err != nil {
		return err
	}

	err = json.NewEncoder(stdout).Encode(result)
	if err != nil {
		return fmt.Errorf("write result: %w", err)
	}

	return nil
}

func cellCommand(ctx context.Context, args []string) (cellRun, error) {
	flags := flag.NewFlagSet("loadgen cell", flag.ContinueOnError)
	endpoint := flags.String("endpoint", defaultEndpoint, "target endpoint")
	duration := flags.Duration("duration", defaultLoadDuration, "how long to run")

	var spec cellSpec

	flags.IntVar(&spec.Payload, "payload", payloadSmall, "message body bytes")
	flags.StringVar(&spec.Queue, "queue", queueStandard, "standard or fifo")
	flags.StringVar(&spec.Shape, "shape", shapeSingle, "single, batch or longpoll")
	flags.StringVar(&spec.Proto, "proto", protoJSON, "json (through the SDK) or query (a form post)")
	flags.IntVar(&spec.Concurrency, "concurrency", 1, "concurrent workers")

	err := flags.Parse(args)
	if err != nil {
		return cellRun{}, fmt.Errorf("parse flags: %w", err)
	}

	return runCell(ctx, *endpoint, spec, *duration)
}

func agenticCommand(ctx context.Context, args []string) (agenticRun, error) {
	flags := flag.NewFlagSet("loadgen agentic", flag.ContinueOnError)
	endpoint := flags.String("endpoint", defaultEndpoint, "target endpoint")
	cycles := flags.Int("cycles", defaultCycles, "create, use, delete cycles")
	concurrency := flags.Int("concurrency", defaultCycleConc, "concurrent workers")

	err := flags.Parse(args)
	if err != nil {
		return agenticRun{}, fmt.Errorf("parse flags: %w", err)
	}

	return runAgentic(ctx, newSQSClient(*endpoint, *concurrency), *cycles, *concurrency)
}

// newTransport returns a connection pool sized for the workers.
func newTransport(workers int) *http.Transport {
	return &http.Transport{
		DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
		MaxIdleConns:        workers * idleConnsPerWork,
		MaxIdleConnsPerHost: workers * idleConnsPerWork,
		IdleConnTimeout:     idleConnTimeout,
	}
}

// newSQSClient returns an SDK client with retries off, so a failure is counted
// and not hidden, and a connection pool sized for the workers.
func newSQSClient(endpoint string, workers int) *sqs.Client {
	return sqs.New(sqs.Options{
		Region:           loadRegion,
		Credentials:      credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint:     &endpoint,
		HTTPClient:       &http.Client{Transport: readOnlyBodies{newTransport(workers)}},
		RetryMaxAttempts: 1,
	})
}

// readOnlyBodies hands net/http a request body with only Read and Close.
//
// The SDK closes the request body as soon as the response headers arrive.
// net/http keeps writing in another goroutine: after the Content-Length bytes it
// reads the body once more to check for extra data, and that read goes through
// WriteTo when the body has one. The SDK's body (smithy-go
// transport/http/internal/io.safeWriteToReadCloser) returns io.EOF from WriteTo
// once it is closed, which io.Copy passes on as an error, so net/http counts the
// request write as failed and closes the connection under the response body
// still being read: "use of closed network connection". A server that answers
// within microseconds of the request body, as gostack does, loses that race on
// receives whose answer outgrows the transport's 4 KiB read buffer. On a FIFO
// queue the failed receive leaves the worker's group in flight, so every later
// receive of the run comes back empty. Read after Close returns a plain io.EOF,
// which ends the check cleanly.
type readOnlyBodies struct{ next http.RoundTripper }

func (t readOnlyBodies) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.Body != http.NoBody {
		// A shallow copy: a RoundTripper must not change the caller's request.
		req = req.WithContext(req.Context())
		req.Body = readCloser{req.Body}
	}

	return t.next.RoundTrip(req) //nolint:wrapcheck // a RoundTripper returns its transport's error as is.
}

// readCloser hides every method of a body but Read and Close.
type readCloser struct{ io.ReadCloser }

// statsOf summarizes timings. It sorts its argument.
func statsOf(timings []time.Duration, failed int) opStats {
	stats := opStats{Count: len(timings), Errors: failed}
	if len(timings) == 0 {
		return stats
	}

	slices.Sort(timings)

	var sum time.Duration
	for _, t := range timings {
		sum += t
	}

	stats.P50us = percentile(timings, percentMedian).Microseconds()
	stats.P99us = percentile(timings, percentTail).Microseconds()
	stats.MeanUS = (sum / time.Duration(len(timings))).Microseconds()

	return stats
}

// percentile is the nearest-rank percentile of sorted timings.
func percentile(sorted []time.Duration, pct int) time.Duration {
	rank := int(math.Ceil(float64(pct) / percentScale * float64(len(sorted))))

	return sorted[min(max(rank, 1), len(sorted))-1]
}

// runAgentic runs cycles of create queue, send 10, receive 10, delete queue,
// spread over the workers.
func runAgentic(ctx context.Context, client *sqs.Client, cycles, workers int) (agenticRun, error) {
	prefix := fmt.Sprintf("bench-agentic-%x", time.Now().UnixNano())
	jobs := make(chan int)
	timings := make([][]time.Duration, workers)
	failures := make([]int, workers)
	started := time.Now()

	var wg sync.WaitGroup

	for i := range workers {
		wg.Go(func() {
			for n := range jobs {
				began := time.Now()
				err := agenticCycle(ctx, client, fmt.Sprintf("%s-%d", prefix, n))
				if err != nil {
					failures[i]++

					continue
				}

				timings[i] = append(timings[i], time.Since(began))
			}
		})
	}

	for n := range cycles {
		jobs <- n
	}

	close(jobs)
	wg.Wait()

	run := agenticRun{Cycles: cycles, Concurrency: workers, Seconds: time.Since(started).Seconds()}
	all := slices.Concat(timings...)

	for _, f := range failures {
		run.Errors += f
	}

	if len(all) == 0 {
		return run, errors.New("every cycle failed")
	}

	slices.SortFunc(all, cmp.Compare[time.Duration])

	run.CycleP50ms = float64(percentile(all, percentMedian).Microseconds()) / microsPerMilli
	run.CycleP99ms = float64(percentile(all, percentTail).Microseconds()) / microsPerMilli

	return run, nil
}

func agenticCycle(ctx context.Context, client *sqs.Client, name string) error {
	created, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: &name})
	if err != nil {
		return fmt.Errorf("create queue: %w", err)
	}

	for i := range messagesPerCycle {
		body := fmt.Sprintf("message %d of %s", i, name)
		_, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: created.QueueUrl, MessageBody: &body})
		if err != nil {
			return fmt.Errorf("send message: %w", err)
		}
	}

	received := 0
	for round := 0; received < messagesPerCycle; round++ {
		if round >= maxReceiveRounds {
			return fmt.Errorf("received %d of %d messages in %d rounds", received, messagesPerCycle, round)
		}

		var got *sqs.ReceiveMessageOutput

		got, err = client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: created.QueueUrl, MaxNumberOfMessages: messagesPerCycle,
		})
		if err != nil {
			return fmt.Errorf("receive message: %w", err)
		}

		received += len(got.Messages)
	}

	_, err = client.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: created.QueueUrl})
	if err != nil {
		return fmt.Errorf("delete queue: %w", err)
	}

	return nil
}
