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
	"time"
)

const (
	defaultEndpoint     = "http://127.0.0.1:4566"
	loadRegion          = "us-east-1"
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

// runResult is what a workload returns for one run of a cell: the operations it
// timed, by name, and the whole cycles it completed. Counters are the workload's
// own tallies, such as receives that came back empty.
type runResult struct {
	Seconds      float64            `json:"seconds"`
	Cycles       int                `json:"cycles"`
	CyclesPerSec float64            `json:"cycles_per_sec"`
	Counters     map[string]int     `json:"counters,omitempty"`
	Ops          map[string]opStats `json:"ops"`
	Usage        usage              `json:"usage,omitzero"`
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
		return errors.New("usage: bench loadgen cell|agentic|first -service NAME [flags]")
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
	case loadgenFirst:
		result, err = firstUseCommand(ctx, args[1:])
	default:
		return fmt.Errorf("unknown workload %q, want cell, agentic or first", args[0])
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

// optionFlags collects the repeated -opt key=value flags of a cell.
type optionFlags []option

func (o *optionFlags) String() string {
	parts := make([]string, 0, len(*o))
	for _, opt := range *o {
		parts = append(parts, opt.Key+"="+opt.Value)
	}

	return strings.Join(parts, ",")
}

var errBadOption = errors.New("want -opt key=value")

func (o *optionFlags) Set(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if !ok || key == "" {
		return fmt.Errorf("%w, got %q", errBadOption, v)
	}

	*o = append(*o, option{Key: key, Value: value})

	return nil
}

// parseCell reads the flags of `loadgen cell`: the endpoint and duration of the
// run, and the cell, whose persistence the orchestrator has already set on the
// container.
func parseCell(args []string) (cellRequest, error) {
	flags := flag.NewFlagSet("loadgen cell", flag.ContinueOnError)
	req := cellRequest{cell: cellSpec{Service: serviceSQS, Workload: workloadSQS}}
	spec := &req.cell

	var options optionFlags

	flags.StringVar(&req.endpoint, "endpoint", defaultEndpoint, "target endpoint")
	flags.DurationVar(&req.duration, "duration", defaultLoadDuration, "how long to run")
	flags.StringVar(&spec.Service, "service", spec.Service, "the service to load")
	flags.StringVar(&spec.Workload, "workload", spec.Workload, "the service's workload")
	flags.IntVar(&spec.Payload, "payload", payloadSmall, "body bytes")
	flags.IntVar(&spec.Concurrency, "concurrency", 1, "concurrent workers")
	flags.Var(&options, "opt", "an axis of the workload, key=value; repeat for each (sqs: queue, shape, proto)")

	err := flags.Parse(args)
	if err != nil {
		return cellRequest{}, fmt.Errorf("parse flags: %w", err)
	}

	spec.Options = options

	return req, nil
}

// cellRequest is what `loadgen cell` was asked to run.
type cellRequest struct {
	cell     cellSpec
	endpoint string
	duration time.Duration
}

func cellCommand(ctx context.Context, args []string) (runResult, error) {
	req, err := parseCell(args)
	if err != nil {
		return runResult{}, err
	}

	wl, err := findWorkload(req.cell.Service, req.cell.Workload)
	if err != nil {
		return runResult{}, err
	}

	res, err := wl.Run(ctx, client{endpoint: req.endpoint}, req.cell, req.duration)
	if err != nil {
		return res, fmt.Errorf("run %s: %w", req.cell.id(), err)
	}

	return res, nil
}

func agenticCommand(ctx context.Context, args []string) (agenticRun, error) {
	flags := flag.NewFlagSet("loadgen agentic", flag.ContinueOnError)
	endpoint := flags.String("endpoint", defaultEndpoint, "target endpoint")
	service := flags.String("service", serviceSQS, "the service to load")
	cycles := flags.Int("cycles", defaultCycles, "create, use, delete cycles")
	concurrency := flags.Int("concurrency", defaultCycleConc, "concurrent workers")

	err := flags.Parse(args)
	if err != nil {
		return agenticRun{}, fmt.Errorf("parse flags: %w", err)
	}

	wl, err := findAgentic(*service)
	if err != nil {
		return agenticRun{}, err
	}

	run, err := wl.Agentic(ctx, client{endpoint: *endpoint}, *cycles, *concurrency)
	if err != nil {
		return run, fmt.Errorf("agentic %s: %w", *service, err)
	}

	return run, nil
}

// summarizeAgentic folds the cycle timings of the workers of an agentic run.
func summarizeAgentic(all []time.Duration, failures []int, cycles, workers int,
	took time.Duration,
) (agenticRun, error) {
	run := agenticRun{Cycles: cycles, Concurrency: workers, Seconds: took.Seconds()}

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

// newTransport returns a connection pool sized for the workers.
func newTransport(workers int) *http.Transport {
	return &http.Transport{
		DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
		MaxIdleConns:        workers * idleConnsPerWork,
		MaxIdleConnsPerHost: workers * idleConnsPerWork,
		IdleConnTimeout:     idleConnTimeout,
	}
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
