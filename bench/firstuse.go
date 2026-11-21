package main

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"time"
)

// The first-use measurements: the time a service takes to answer its first use
// on a target that has just started, for the work a service does once per
// process. SNS makes its RSA-2048 signing key on the first CreateTopic and signs
// the first delivery with it, which no micro benchmark can time: it runs once a
// process, so a benchmark that tried would pay a key generation every iteration
// and never reach the 50 iterations the micro-benchmark gate asks of one
// (docs/standards.md, "Micro benchmarks").
//
// A run of a first-use workload is one container, started, used once and
// removed: the loadgen times the use itself (`loadgen first`) and the
// orchestrator takes -first-use-runs of them on each side, alternating the
// sides as startup does. The runs are kept whole in the result file, because the
// compare gate judges this metric on them (compare.go, "rankSumZ").

const (
	// defaultFirstUseRuns is how many containers a side starts to time a first
	// use. A key generation takes 15 to 300 ms, so no fewer than about 30 runs
	// tell a doubled one from chance: docs/standards.md has the numbers.
	defaultFirstUseRuns = 30
	// firstUseTimeout is the longest `loadgen first` may take.
	firstUseTimeout = 2 * time.Minute
)

// firstUseWorkload is a service's first use of what it makes once per process.
type firstUseWorkload interface {
	Service() string
	// FirstUse does the use on a target that has just started, and returns the time
	// from the start of the first call that needs the work to the answer that
	// proves it done.
	FirstUse(ctx context.Context, c client) (time.Duration, error)
}

// firstUseWorkloads is what this program can time.
var firstUseWorkloads = []firstUseWorkload{publishWorkload{}}

//nolint:ireturn // the registry holds workloads of different types.
func findFirstUse(service string) (firstUseWorkload, error) {
	for _, w := range firstUseWorkloads {
		if w.Service() == service {
			return w, nil
		}
	}

	return nil, fmt.Errorf("%w: %s first use", errNoWorkload, service)
}

// firstUseRun is what `loadgen first` prints.
type firstUseRun struct {
	Micros int64 `json:"micros"`
}

// firstUseCell is the id compare and Problems give the first use of a service.
func firstUseCell(service string) string { return "first-use/" + service }

func firstUseCommand(ctx context.Context, args []string) (firstUseRun, error) {
	flags := flag.NewFlagSet("loadgen first", flag.ContinueOnError)
	endpoint := flags.String("endpoint", defaultEndpoint, "target endpoint")
	service := flags.String("service", serviceSNS, "the service whose first use to time")

	err := flags.Parse(args)
	if err != nil {
		return firstUseRun{}, fmt.Errorf("parse flags: %w", err)
	}

	wl, err := findFirstUse(*service)
	if err != nil {
		return firstUseRun{}, err
	}

	took, err := wl.FirstUse(ctx, client{endpoint: *endpoint})
	if err != nil {
		return firstUseRun{}, fmt.Errorf("first use of %s: %w", *service, err)
	}

	return firstUseRun{Micros: took.Microseconds()}, nil
}

// firstUseAll times the first use of each service on every side into res. A side
// stops at its first failure, as startup does, and keeps the failure in Problems.
func (r *runner) firstUseAll(ctx context.Context, sides []target, res []targetResult) {
	for _, w := range firstUseWorkloads {
		if len(r.opts.services) > 0 && !slices.Contains(r.opts.services, w.Service()) {
			continue
		}

		runs, errs := r.firstUse(ctx, sides, w)

		for s := range sides {
			res[s].problem("first use ("+w.Service()+")", errs[s])

			if errs[s] != nil {
				continue
			}

			if res[s].FirstUse == nil {
				res[s].FirstUse = map[string]startupResult{}
			}

			res[s].FirstUse[w.Service()] = runs[s]
		}
	}
}

// firstUse starts firstUseRuns containers on each side, uses each once and
// removes it.
func (r *runner) firstUse(ctx context.Context, sides []target, w firstUseWorkload) ([]startupResult, []error) {
	res := make([]startupResult, len(sides))
	errs := make([]error, len(sides))

	alternate(r.opts.firstUseRuns, len(sides), func(i, s int) {
		if errs[s] != nil {
			return
		}

		took, err := r.firstUseRun(ctx, sides[s], w)
		if err != nil {
			errs[s] = fmt.Errorf("run %d: %w", i+1, err)

			return
		}

		r.logf("%s: first %s use in %s (run %d of %d)", sides[s].label(), w.Service(), took.Round(time.Millisecond),
			i+1, r.opts.firstUseRuns)
		res[s].SamplesMS = append(res[s].SamplesMS, float64(took.Microseconds())/microsPerMilli)
	})

	for s := range sides {
		if errs[s] == nil {
			res[s].MS = spreadOf(res[s].SamplesMS)
		}
	}

	return res, errs
}

// firstUseRun is one container's first use of a service.
func (r *runner) firstUseRun(ctx context.Context, t target, w firstUseWorkload) (time.Duration, error) {
	inst, _, err := r.start(ctx, t)
	if err != nil {
		return 0, err
	}

	defer inst.stop(ctx)

	var run firstUseRun

	err = r.loadgen(ctx, inst.name, firstUseTimeout, &run, "first", "-service", w.Service())
	if err != nil {
		return 0, err
	}

	err = inst.alive(ctx)
	if err != nil {
		return 0, err
	}

	return time.Duration(run.Micros) * time.Microsecond, nil
}
