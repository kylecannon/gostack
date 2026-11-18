// Command bench is the gostack system benchmark and its regression gate. It
// runs one workload against the gostack images and the LocalStack image the
// parity harness builds, and writes the numbers to bench/results/<date>-<sha>.json
// with a markdown table beside it. The rule it implements is in
// docs/standards.md, "Benchmarks"; this is how.
//
// It lives in the main module, not a module of its own, because aws-sdk-go-v2
// is already a dependency of the SQS tests and the architecture table lists it
// for "tests and bench only". Nothing in the product imports this package, so
// the SDK stays out of the gostack binary.
//
// Four subcommands share the one binary:
//
//	bench run       the orchestrator, on the host. `make bench-system` and
//	                `make bench-system-quick` run it.
//	bench compare   the regression gate for result files.
//	bench loadgen   the load generator. The orchestrator cross-compiles this
//	                binary into a scratch image and runs it in a container that
//	                joins the target's network namespace (--network
//	                container:<target>), so the load reaches the target on
//	                127.0.0.1:4566 inside that namespace. Host port 4566 is never
//	                bound and no port is published.
//	bench gate      reads benchstat output on stdin and fails on a regression
//	                over 10% with p<0.05. The bench workflow runs it.
//
// # Workloads and cells
//
// A workload (workload.go) is one service's load shape: it names the service,
// the operations it times, its cells, and how to run one cell against an
// endpoint. sqs.go holds the only one so far. The orchestrator, the result file,
// the report and the compare gate know nothing of the service; they read the
// operations from the cell. A cell id is <service>/<workload>/<options...>, and
// a cell names the environment variable that turns its persistence on.
//
// # The SQS matrix
//
// A cell of the sqs/messaging workload is one point of these axes:
//
//	payload      256 B, 64 KiB, 1 MiB
//	queue        standard, fifo
//	shape        single (send, receive one, delete), batch (send 10, receive up
//	             to 10, delete batch) and longpoll (a receive with
//	             WaitTimeSeconds=1 that a send wakes)
//	protocol     json through aws-sdk-go-v2, query through a form post built in
//	             the load generator
//	persistence  off, and PERSIST_SQS=1 with a tmpfs at /persisted-data
//	concurrency  1, 16, 64
//
// -profile full runs all 216 cells. -profile quick runs 10: the base cell (256 B,
// standard, single, json, no persistence) at concurrency 16, 1 and 64, and at
// concurrency 16 the base cell with one axis changed at a time. Every quick cell
// is a full cell with the same id, so the two profiles compare. The persistence
// cells are left out for a target that does not persist, which is LocalStack.
//
// Each cell runs -reps times (3), each run -duration long (2s), each run
// against a container of its own, so the memory of an earlier cell does not
// carry into a later one. The result keeps, for every figure, the minimum,
// median and maximum of the runs.
//
// # What else is measured
//
// For each target, in a container of its own:
//
//   - the time from `docker run` to the "Ready." log line, over -startup-runs
//     containers;
//   - idle memory and idle CPU over -idle, on -idle-runs containers, read from
//     the Docker Engine stats API (cgroup counters): memory is anon plus mapped
//     file pages, the resident set of the processes in the container, and CPU
//     is the change of the cumulative CPU time over the window, so a wakeup
//     that burns a microsecond still counts. The full profile does it with the
//     image HEALTHCHECK on and off (`docker run --no-healthcheck`), so the
//     difference is the cost of the check;
//   - an agentic workload: 200 cycles of create queue, 10 SendMessage,
//     receive 10, delete queue, at concurrency 16;
//   - image size.
//
// # The gate
//
// `bench compare -base A.json -head B.json`, or -base-dir to take the newest
// git-tracked result with the head's host fingerprint, holds head against base
// on the gostack and gostack-slim cells both files have. It exits 1 when a
// metric moved past its limit (constants in compare.go) and the ranges of its
// runs do not overlap. LocalStack never gates. A result carries a host
// fingerprint (OS, architecture, CPU count, Docker version, OrbStack); results
// with different fingerprints are not compared, and compare says so and exits 0.
// `bench run -compare-dir bench/results` runs the same comparison after the
// measurement. The scheduled CI run compares with the result the previous
// passing run on main kept as a workflow artifact (.github/workflows/bench-system.yml).
//
// # The result file
//
// Schema 3. A cell holds its operations as a map from name to latency spread.
// loadReport converts a schema 2 file, whose cells had send, receive and delete
// fields and ids with no service, so the committed results still compare.
package main
