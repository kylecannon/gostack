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
//	                over 10% with p<0.001 that the accepted list does not
//	                hold. The bench workflow runs it.
//
// # Workloads and cells
//
// A workload (workload.go) is one service's load shape: it names the service,
// the operations it times, its cells, and how to run one cell against an
// endpoint. sqs.go and s3.go hold them. The orchestrator, the result file,
// the report and the compare gate know nothing of the service; they read the
// operations from the cell. A cell id is <service>/<workload>/<options...>, and
// a cell names the environment variable that turns its persistence on.
// `bench run -services s3` runs the cells and the agentic cycle of the named
// services only.
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
// # The S3 matrix
//
// s3.go speaks S3 over plain HTTP, path style, with a SigV4 header the services
// route on and nobody checks. It has three workloads:
//
//	s3/objects    a PutObject then a GetObject of one key a worker, at 1 KiB,
//	              64 KiB, 1 MiB and 16 MiB, at concurrency 1, 16 and 64
//	s3/listing    ListObjectsV2 over a bucket seeded with 1000 or 10000 keys,
//	              flat or with delimiter "/", at concurrency 1, 16 and 64. A cycle
//	              pages through every key, 1000 a page; each page is timed
//	s3/multipart  CreateMultipartUpload, eight 8 MiB UploadPart and
//	              CompleteMultipartUpload: a 64 MiB object, at concurrency 1 and 4
//	              (64 workers would hold 4 GiB in flight)
//
// A listing cell seeds its bucket inside the load generator's run, so the RSS and
// CPU the orchestrator reads for it include the seeding. The latencies do not.
//
// Every cell also runs with PERSIST_S3=1 and a tmpfs at /persisted-data, as the
// SQS cells do. The persistence cells are left out for LocalStack, whose
// Community image does not restore S3 under PERSISTENCE=1. The tmpfs keeps the
// disk out of the figure: a cell measures the work persistence adds, the blob
// written and renamed at request time, and the save, which syncs the blobs
// committed since the last one and writes the snapshot.
//
// -profile full runs 52 cells per image that persists, 26 on LocalStack.
// -profile quick runs 11: the 8 persistence-off cells, and the base cell of each
// workload with persistence on. The
// agentic workload is 200 cycles of create bucket, put 10 objects, list them,
// delete them, delete the bucket, at concurrency 16.
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
// # The flags
//
// The markdown report flags three things (flags.go) and leaves every other gap to
// the tables: a gostack cell ten times slower than its LocalStack twin, memory
// that grows with object size (RSS above the start of the run that follows the
// payload and not the runtime warming up), and CPU over 1% of a core while idle.
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
// `bench run -base-image B -base-commit C -base-out DIR` measures image B as
// the base of the one target, next to the target's own image as the head, in
// one run: each startup run, idle run and run of a cell goes base head, then
// head base, and so on (roundOrder in run.go). It writes two ordinary result
// files, the base's in DIR and the head's in -out, for `bench compare`. The
// pull request workflow measures this way, so drift over the job lands on both
// sides.
//
// # Errors and dead targets
//
// A run is not a measurement when its target was not working. The orchestrator
// checks, after every run of a cell, idle run and agentic workload, that the
// target container is still running (`docker inspect`), and fails the run when
// it is not (errTargetDied). A cell run that fails is left out of the figures
// and named in the cell's failed_runs, so Runs counts the completed ones and a
// side never ends with fewer runs than the other without a record of it. A cell
// that has no completed run has no figures and a problem; the report lists
// every one under "Problems". An operation that reported errors, and the
// cycles of an agentic workload that errored, are recorded the same way, with
// their counts. Their runs stay in the figures.
//
// `bench compare` reads those records as a second rule beside the metric
// limits (problems.go). It fails when the head has a problem the base does not:
// an operation with errors in the head and none in the base, failed runs in a
// cell the base ran clean, a cell with no completed run where the base has
// runs, a measurement the head could not take (startup, idle, an agentic
// workload) of a kind the base did not also fail to take (the kind is the text before the first ":" of its
// line), and any of these in a cell only the head has. A head cell the base
// does not have is checked: it fails when it has a failed run, no completed
// run or operation errors, and stays under "Not compared" when clean. A base
// cell with no completed run does not hide the head's errors there: they fail
// unless the base's problem in that cell was of the same kind. A problem on
// both sides is listed and does not fail, with two exceptions: the head has
// more failed runs in a cell than the base, or its errors in a cell and
// operation are at least 10 and more than 10 times the base's (the agentic
// cycles follow the same rule). A head that errors where the base has none
// fails at one error. That is deliberate: the committed results hold 0 errors
// in over 900,000 operations, a stray error costs one rerun, and a floor would
// let a rare race through. Every row names the cell, the operation and both
// counts.
//
// A gated target, or a cell with a completed run, that the base has and the
// head lacks fails as "missing from the head", naming it. A cell or target
// missing on both sides, a target that is not gated, and a cell the head was
// not asked to measure (a different profile, a service the head's settings
// leave out, or a cell the head's own matrix no longer has, listed as "removed
// from the matrix") do not. A cell with no completed run in a result that
// predates failed_runs counts as having failed every run that result's reps
// asked for. Cells with a different number of runs are listed. A row
// of a regression or a change computed from fewer runs than the result asked
// for says so, such as "head 4 of 5 runs", because the overlap rule was sized
// for the full count. The committed results hold no operation error and no
// problem, LocalStack's included, so any problem is new. The accepted list does
// not apply to it: a run that errors is fixed, not accepted. LocalStack never
// gates.
//
// Every container the run starts is removed with `docker rm -f -v`, so the
// anonymous volume the image declares goes with it. `-out` and `-base-out` are
// compared as absolute paths with their links resolved (`/tmp/x` and
// `/private/tmp/x` are one directory), so one directory spelled two ways is
// refused.
//
// `-accepted bench/accepted.yaml` on compare, run and gate holds the regressions
// a reviewer accepted (accepted.go). An entry applies while the head's commit
// holds its commit and the base's does not; compare reads both commits from the
// result files, gate takes them as -base and -head. A held regression is listed
// under "Accepted" and does not fail.
//
// # The result file
//
// Schema 3. A cell holds its operations as a map from name to latency spread.
// loadReport converts a schema 2 file, whose cells had send, receive and delete
// fields and ids with no service, so the committed results still compare. A
// cell's failed_runs, the repetitions that did not complete, was added without
// a version bump: no field changed its meaning, and a file without it has none.
package main
