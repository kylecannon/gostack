// Package lifecycle runs gostack's process lifecycle: the init hooks, the Ready.
// marker and the PID 1 supervisor.
//
// # Init hooks
//
// [Hooks] runs the scripts in <root>/<stage>.d for the boot, start, ready and
// shutdown stages, as LocalStack's runtime/init.py does. A .sh script is
// executed directly, so it needs the executable bit and a shebang. A .py script
// runs under python3. Scripts run in sorted order, and each one ends in the
// SUCCESSFUL or ERROR state. A failing script is logged and does not stop the
// stage.
//
// # Ready
//
// [Process.Ready] runs the ready hooks, prints "Ready." on stdout, which
// testcontainers waits for, and then reports ready.
//
// # Supervisor
//
// When gostack runs as PID 1, [Supervisor] re-executes the binary as a child,
// reaps zombies, forwards signals, and restarts the child on request, as
// LocalStack's bin/localstack-supervisor does:
//
//   - SIGTERM and SIGINT reach the child as SIGTERM. SIGHUP is forwarded as
//     SIGHUP. The supervisor exits with the child's exit code, or 128 plus the
//     signal number when a signal killed the child.
//   - A child still running SHUTDOWN_TIMEOUT after its SIGTERM gets SIGKILL.
//   - SIGUSR1 asks for a restart. The child sends it with [SignalRestart],
//     which reads the SUPERVISOR_PID variable the supervisor sets in the
//     child's environment. LocalStack signals the same way, so a restart is a
//     signal and not an exit code or a pipe. The supervisor stops the child
//     with SIGTERM and starts a new one.
//   - Zombies are reaped on SIGCHLD, so an idle supervisor is asleep.
package lifecycle
