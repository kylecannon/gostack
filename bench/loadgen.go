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
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

const (
	defaultEndpoint     = "http://127.0.0.1:4566"
	loadRegion          = "us-east-1"
	messageBytes        = 256
	messagesPerCycle    = 10
	maxReceiveRounds    = 100
	maxWorkerErrors     = 50
	dialTimeout         = 5 * time.Second
	idleConnTimeout     = 90 * time.Second
	percentMedian       = 50
	percentTail         = 99
	percentScale        = 100
	microsPerMilli      = 1000
	defaultLoadDuration = 30 * time.Second
	defaultCycles       = 200
	defaultCycleConc    = 16
	idleConnsPerWork    = 2
)

// errTooManyErrors stops a worker whose calls keep failing, so a dead target
// ends a run quickly instead of spinning for the whole duration.
var errTooManyErrors = errors.New("too many failed calls")

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
	CPUPercent       float64 `json:"cpu_percent"`
}

// latencyRun is one concurrency level of the SQS latency workload.
type latencyRun struct {
	Concurrency   int     `json:"concurrency"`
	Seconds       float64 `json:"seconds"`
	CyclesPerSec  float64 `json:"cycles_per_sec"`
	EmptyReceives int     `json:"empty_receives"`
	Send          opStats `json:"send"`
	Receive       opStats `json:"receive"`
	Delete        opStats `json:"delete"`
	Usage         usage   `json:"usage,omitzero"`
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
		return errors.New("usage: bench loadgen sqs|agentic [flags]")
	}

	var (
		result any
		err    error
	)

	switch args[0] {
	case "sqs":
		result, err = latencyCommand(ctx, args[1:])
	case "agentic":
		result, err = agenticCommand(ctx, args[1:])
	default:
		return fmt.Errorf("unknown workload %q, want sqs or agentic", args[0])
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

func latencyCommand(ctx context.Context, args []string) (latencyRun, error) {
	flags := flag.NewFlagSet("loadgen sqs", flag.ContinueOnError)
	endpoint := flags.String("endpoint", defaultEndpoint, "target endpoint")
	concurrency := flags.Int("concurrency", 1, "concurrent workers")
	duration := flags.Duration("duration", defaultLoadDuration, "how long to run")

	err := flags.Parse(args)
	if err != nil {
		return latencyRun{}, fmt.Errorf("parse flags: %w", err)
	}

	return runLatency(ctx, newSQSClient(*endpoint, *concurrency), *concurrency, *duration)
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

// newSQSClient returns an SDK client with retries off, so a failure is counted
// and not hidden, and a connection pool sized for the workers.
func newSQSClient(endpoint string, workers int) *sqs.Client {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
		MaxIdleConns:        workers * idleConnsPerWork,
		MaxIdleConnsPerHost: workers * idleConnsPerWork,
		IdleConnTimeout:     idleConnTimeout,
	}

	return sqs.New(sqs.Options{
		Region:           loadRegion,
		Credentials:      credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint:     &endpoint,
		HTTPClient:       &http.Client{Transport: transport},
		RetryMaxAttempts: 1,
	})
}

// samples are the timings one worker collected.
type samples struct {
	send, receive, del []time.Duration
	failed             [3]int
	empty              int
}

// runLatency runs workers that each loop send, receive one message, delete it,
// against one queue for the duration. A call in flight at the deadline
// finishes and counts.
func runLatency(ctx context.Context, client *sqs.Client, workers int, duration time.Duration) (latencyRun, error) {
	name := fmt.Sprintf("bench-latency-%x", time.Now().UnixNano())

	created, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: &name})
	if err != nil {
		return latencyRun{}, fmt.Errorf("create queue: %w", err)
	}

	defer func() {
		_, _ = client.DeleteQueue(context.WithoutCancel(ctx), &sqs.DeleteQueueInput{QueueUrl: created.QueueUrl})
	}()

	body := strings.Repeat("x", messageBytes)
	results := make([]samples, workers)
	errs := make([]error, workers)
	started := time.Now()
	end := started.Add(duration)

	var wg sync.WaitGroup

	for i := range workers {
		wg.Go(func() { errs[i] = latencyWorker(ctx, client, *created.QueueUrl, body, end, &results[i]) })
	}

	wg.Wait()

	elapsed := time.Since(started)

	err = errors.Join(errs...)
	if err != nil {
		return latencyRun{}, err
	}

	return summarize(results, workers, elapsed), nil
}

func latencyWorker(
	ctx context.Context, client *sqs.Client, queueURL, body string, end time.Time, out *samples,
) error {
	for time.Now().Before(end) {
		err := ctx.Err()
		if err != nil {
			return fmt.Errorf("stopped: %w", err)
		}

		if out.failed[0]+out.failed[1]+out.failed[2] > maxWorkerErrors {
			return errTooManyErrors
		}

		began := time.Now()
		_, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: &body})
		took := time.Since(began)

		if err != nil {
			out.failed[0]++

			continue
		}

		out.send = append(out.send, took)

		began = time.Now()
		got, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 1})
		took = time.Since(began)

		if err != nil {
			out.failed[1]++

			continue
		}

		if len(got.Messages) == 0 {
			out.empty++

			continue
		}

		out.receive = append(out.receive, took)

		began = time.Now()
		_, err = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl: &queueURL, ReceiptHandle: got.Messages[0].ReceiptHandle,
		})
		took = time.Since(began)

		if err != nil {
			out.failed[2]++

			continue
		}

		out.del = append(out.del, took)
	}

	return nil
}

func summarize(results []samples, workers int, elapsed time.Duration) latencyRun {
	var send, receive, del []time.Duration

	run := latencyRun{Concurrency: workers, Seconds: elapsed.Seconds()}

	for _, s := range results {
		send = append(send, s.send...)
		receive = append(receive, s.receive...)
		del = append(del, s.del...)
		run.EmptyReceives += s.empty
		run.Send.Errors += s.failed[0]
		run.Receive.Errors += s.failed[1]
		run.Delete.Errors += s.failed[2]
	}

	run.Send = statsOf(send, run.Send.Errors)
	run.Receive = statsOf(receive, run.Receive.Errors)
	run.Delete = statsOf(del, run.Delete.Errors)
	run.CyclesPerSec = float64(run.Delete.Count) / elapsed.Seconds()

	return run
}

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
