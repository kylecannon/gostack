// Command bench is the gostack system benchmark. It runs one workload against
// the gostack images and the LocalStack image the parity harness builds, and
// writes the numbers to bench/results/<date>-<sha>.json with a markdown table
// beside it.
//
// It lives in the main module, not a module of its own, because aws-sdk-go-v2
// is already a dependency of the SQS tests and the architecture table lists it
// for "tests and bench only". Nothing in the product imports this package, so
// the SDK stays out of the gostack binary.
//
// Three subcommands share the one binary:
//
//	bench run       the orchestrator, on the host. `make bench-system` runs it.
//	bench loadgen   the load generator. The orchestrator cross-compiles this
//	                binary into a scratch image and runs it in a container that
//	                joins the target's network namespace (--network
//	                container:<target>), so the load reaches the target on
//	                127.0.0.1:4566 inside that namespace. Host port 4566 is never
//	                bound and no port is published.
//	bench gate      reads benchstat output on stdin and fails on a regression
//	                over 10% with p<0.05. The bench workflow runs it.
//
// What `bench run` measures for each target, every one in a container of its
// own with a unique name:
//
//   - the time from `docker run` to the "Ready." log line, median of five
//     containers;
//   - idle memory and idle CPU over 60 seconds, read from the Docker Engine
//     stats API (cgroup counters): memory is anon plus mapped file pages, the
//     resident set of the processes in the container, and CPU is the change of
//     the cumulative CPU time over the window, so a wakeup that burns a
//     microsecond still counts;
//   - SQS SendMessage, ReceiveMessage and DeleteMessage p50 and p99 at
//     concurrency 1, 16 and 64, 30 seconds each. Every worker loops send,
//     receive one message, delete it, against one standard queue;
//   - an agentic workload: 200 cycles of create queue, 10 SendMessage, receive
//     10, delete queue, at concurrency 16, with total time and peak memory;
//   - image size.
package main
