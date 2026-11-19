package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The cells of the system matrix (docs/standards.md, "Benchmarks"). A cell is
// one combination of a workload's axes, measured by running the same cycle at a
// concurrency for a fixed time, several times, each in a container of its own.
//
// A workload is one service's load shape. It names the service and the
// operations it times, lists its own cells, and runs one cell against an
// endpoint. Everything else in this program (the orchestrator, the result file,
// the report, the compare gate) reads the operations from the result, so a
// service is added by registering a workload and nothing else.

const (
	profileFull  = "full"
	profileQuick = "quick"

	levelLow  = 1
	levelMid  = 16
	levelHigh = 64

	bytesPerKiB = 1 << 10

	// idFixedParts are the segments of a cell id besides the workload's options:
	// service, workload, persist, payload and concurrency.
	idFixedParts = 5
)

// workload is one service's load shape.
type workload interface {
	// Service is the first segment of the cell id: the service the cells load.
	Service() string
	// Name is the second segment: the shape of load, for a service with more than
	// one.
	Name() string
	// Ops lists the operations a run times, in the order a report shows them.
	Ops() []string
	// Cells lists the cells of a profile, persistence on and off.
	Cells(profile string) []cellSpec
	// Run runs one cell for the duration against the endpoint, on cell.Concurrency
	// workers, and returns what it timed.
	Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error)
}

// agenticWorkload is a service's create, use and delete cycle.
type agenticWorkload interface {
	Service() string
	Agentic(ctx context.Context, c client, cycles, workers int) (agenticRun, error)
}

// client is how a workload reaches the target: the endpoint a service answers
// on, from inside the target's network namespace.
type client struct{ endpoint string }

// workloads and agenticWorkloads are what this program can run.
var (
	workloads        = []workload{sqsWorkload{}, objectsWorkload{}, listingWorkload{}, multipartWorkload{}}
	agenticWorkloads = []agenticWorkload{sqsWorkload{}, objectsWorkload{}}
)

var errNoWorkload = errors.New("no such workload")

//nolint:ireturn // the registry holds workloads of different types.
func findWorkload(service, name string) (workload, error) {
	for _, w := range workloads {
		if w.Service() == service && w.Name() == name {
			return w, nil
		}
	}

	return nil, fmt.Errorf("%w: %s/%s", errNoWorkload, service, name)
}

//nolint:ireturn // the registry holds workloads of different types.
func findAgentic(service string) (agenticWorkload, error) {
	for _, w := range agenticWorkloads {
		if w.Service() == service {
			return w, nil
		}
	}

	return nil, fmt.Errorf("%w: %s agentic", errNoWorkload, service)
}

// option is one axis of a cell the workload defines, such as queue=fifo.
type option struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// cellSpec is one point of a workload's matrix.
type cellSpec struct {
	Service  string `json:"service"`
	Workload string `json:"workload"`
	Payload  int    `json:"payload_bytes"`
	// Options are the workload's own axes, in the order the id carries them.
	Options []option `json:"options,omitempty"`
	// PersistEnv is the environment variable the cell sets to 1 to turn the
	// service's persistence on. Empty is persistence off.
	PersistEnv  string `json:"persist_env,omitempty"`
	Concurrency int    `json:"concurrency"`
}

func (c cellSpec) persists() bool { return c.PersistEnv != "" }

// option returns the value of an axis, or "" when the cell has no such axis.
func (c cellSpec) option(key string) string {
	for _, o := range c.Options {
		if o.Key == key {
			return o.Value
		}
	}

	return ""
}

// payloadName writes a size the way the cell id carries it.
func payloadName(n int) string {
	switch {
	case n >= bytesPerMiB && n%bytesPerMiB == 0:
		return strconv.Itoa(n/bytesPerMiB) + "MiB"
	case n >= bytesPerKiB && n%bytesPerKiB == 0:
		return strconv.Itoa(n/bytesPerKiB) + "KiB"
	default:
		return strconv.Itoa(n) + "B"
	}
}

// id is the name a cell has in the result files, and the key compare matches
// cells by: <service>/<workload>/<options...>. It is the same in every profile.
// No segment holds a slash.
func (c cellSpec) id() string {
	persist := "off"
	if c.persists() {
		persist = "on"
	}

	parts := make([]string, 0, idFixedParts+len(c.Options))
	parts = append(parts, c.Service, c.Workload, "persist="+persist)

	// A workload with no body of its own, such as a listing, has no payload axis.
	if c.Payload > 0 {
		parts = append(parts, "payload="+payloadName(c.Payload))
	}

	for _, o := range c.Options {
		parts = append(parts, o.Key+"="+o.Value)
	}

	parts = append(parts, "c="+strconv.Itoa(c.Concurrency))

	return strings.Join(parts, "/")
}

// cellsFor lists the cells of a profile that a target runs, for the services
// named, or for every service when none is. A target that does not persist
// leaves the persistence-on cells out.
func cellsFor(profile string, t target, services ...string) []cellSpec {
	var out []cellSpec

	for _, w := range workloads {
		if len(services) > 0 && !slices.Contains(services, w.Service()) {
			continue
		}

		for _, c := range w.Cells(profile) {
			if c.persists() && !t.persistable {
				continue
			}

			out = append(out, c)
		}
	}

	return out
}

// orderedOps lists the operations the cells time. The workload's own order comes
// first, so a report reads the way the cycle runs; operations it does not know,
// from a result of another build, follow in name order.
func orderedOps(service, name string, cells []cellResult) []string {
	present := map[string]bool{}

	for _, c := range cells {
		for op := range c.Ops {
			present[op] = true
		}
	}

	var ops []string

	known, err := findWorkload(service, name)
	if err == nil {
		for _, op := range known.Ops() {
			if present[op] {
				ops = append(ops, op)
				delete(present, op)
			}
		}
	}

	rest := make([]string, 0, len(present))
	for op := range present {
		rest = append(rest, op)
	}

	slices.Sort(rest)

	return append(ops, rest...)
}
