//go:build unix

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const (
	// SupervisorPIDEnv is the variable the supervisor sets in the child's
	// environment, so the child can signal it.
	SupervisorPIDEnv = "SUPERVISOR_PID"

	// RestartSignal asks the supervisor to restart the child.
	RestartSignal = syscall.SIGUSR1

	// signalBuffer holds the four signals the supervisor listens for. Notify never
	// blocks, so a smaller buffer could drop a SIGTERM behind a pending SIGUSR1.
	signalBuffer = 4

	// signalExitBase is added to a signal number to form the exit code of a
	// process a signal killed, as shells do.
	signalExitBase = 128
)

// ErrNoSupervisor is returned by [SignalRestart] when the process does not run
// under a supervisor.
var ErrNoSupervisor = errors.New("no supervisor to signal")

// NeedsSupervisor reports whether this process should run the supervisor: it is
// PID 1, as in a container, and nothing else reaps its zombies.
func NeedsSupervisor() bool {
	return os.Getpid() == 1
}

// SignalRestart asks the supervisor named by SUPERVISOR_PID to restart the child
// process. It returns [ErrNoSupervisor] when the variable is not set.
func SignalRestart() error {
	value := os.Getenv(SupervisorPIDEnv)
	if value == "" {
		return ErrNoSupervisor
	}
	pid, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s=%q is not a process id: %w", SupervisorPIDEnv, value, err)
	}
	err = syscall.Kill(pid, RestartSignal)
	if err != nil {
		return fmt.Errorf("signal supervisor %d: %w", pid, err)
	}

	return nil
}

// Supervisor runs gostack as a child process and keeps it running until it is
// told to stop. See the package documentation for the signals it handles.
type Supervisor struct {
	// Command builds the child's command. It is called again for every restart.
	// When nil, the child is this executable with the same arguments. Unless
	// the command sets them, the child shares the supervisor's stdin, stdout and
	// stderr, and always gets SUPERVISOR_PID in its environment.
	Command func() *exec.Cmd
	// ShutdownTimeout is how long a child gets between its SIGTERM and its
	// SIGKILL. Zero waits without a limit.
	ShutdownTimeout time.Duration
	// Logger receives the supervisor's log lines. It defaults to slog.Default().
	Logger *slog.Logger
}

// Run starts the child and returns its exit code once the supervisor is told to
// stop and the child has ended, or once the child ends on its own. It listens
// for SIGTERM, SIGINT, SIGHUP and SIGUSR1. When ctx ends, Run stops the child as
// it does on SIGTERM. The error is not nil only when the child could not be
// started.
func (s *Supervisor) Run(ctx context.Context) (int, error) {
	signals := make(chan os.Signal, signalBuffer)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, RestartSignal)
	defer signal.Stop(signals)

	return s.runWith(ctx, signals)
}

// runWith is Run with the signals supplied, so tests can send them.
func (s *Supervisor) runWith(ctx context.Context, signals <-chan os.Signal) (int, error) {
	// The reaper outlives ctx. A ctx that ends stops the child like a SIGTERM
	// does, and the supervisor learns that the child is gone from the reaper.
	reapCtx, stopReaping := context.WithCancel(context.WithoutCancel(ctx))
	defer stopReaping()

	exits := make(chan exitStatus)
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		reapOnSIGCHLD(reapCtx, waitNoHang, exits)
	}()
	start := func() (childProcess, error) {
		child, err := s.startCommand()
		if err != nil {
			return nil, err
		}

		return child, nil
	}
	code, err := s.supervise(ctx, start, signals, exits)
	stopReaping()
	<-reaped

	return code, err
}

// exitStatus is a process the reaper collected.
type exitStatus struct {
	pid int
	// code is the exit code, or 128 plus the signal number.
	code int
}

// childProcess is what the supervisor needs of a child.
type childProcess interface {
	PID() int
	Signal(sig os.Signal) error
	// Release frees the handle of a child that has exited.
	Release()
}

// supervise starts the child and restarts it while asked to, and returns the exit
// code of the last child.
func (s *Supervisor) supervise(
	ctx context.Context,
	start func() (childProcess, error),
	signals <-chan os.Signal,
	exits <-chan exitStatus,
) (int, error) {
	for {
		child, err := start()
		if err != nil {
			return 1, fmt.Errorf("start child: %w", err)
		}
		s.logger().Debug("child started", "pid", child.PID())
		outcome := s.waitForChild(ctx, child, signals, exits)
		s.logger().Debug("child exited", "pid", child.PID(), "code", outcome.code)
		child.Release()
		if !outcome.restart {
			return outcome.code, nil
		}
	}
}

// childOutcome is how one child's run ended.
type childOutcome struct {
	code int
	// restart is true when the child was stopped to be started again.
	restart bool
}

// waitForChild forwards signals to the child until the reaper reports its exit.
func (s *Supervisor) waitForChild(
	ctx context.Context,
	child childProcess,
	signals <-chan os.Signal,
	exits <-chan exitStatus,
) childOutcome {
	var (
		restart, stopping bool
		kill              <-chan time.Time
		done              = ctx.Done()
	)
	stop := func() {
		if stopping {
			return
		}
		stopping = true
		s.logger().Debug("stopping child", "pid", child.PID())
		_ = child.Signal(syscall.SIGTERM)
		if s.ShutdownTimeout > 0 {
			timer := time.NewTimer(s.ShutdownTimeout)
			kill = timer.C
		}
	}
	for {
		select {
		case sig := <-signals:
			switch sig {
			case RestartSignal:
				if !stopping {
					restart = true
				}
				stop()
			case syscall.SIGTERM, syscall.SIGINT:
				restart = false
				stop()
			case syscall.SIGHUP:
				_ = child.Signal(syscall.SIGHUP)
			default:
			}
		case <-done:
			done = nil
			restart = false
			stop()
		case <-kill:
			kill = nil
			s.logger().Warn("child did not stop in time, killing it", "pid", child.PID())
			_ = child.Signal(syscall.SIGKILL)
		case exit := <-exits:
			if exit.pid == child.PID() {
				return childOutcome{code: exit.code, restart: restart}
			}
		}
	}
}

func (s *Supervisor) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}

	return slog.Default()
}

// execChild is a started os/exec command. The reaper collects its status, so
// nothing calls Cmd.Wait.
type execChild struct {
	process *os.Process
}

func (c execChild) PID() int { return c.process.Pid }

func (c execChild) Release() { _ = c.process.Release() }

func (c execChild) Signal(sig os.Signal) error {
	err := c.process.Signal(sig)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}

	return err //nolint:wrapcheck // os.Process.Signal already names the operation
}

func (s *Supervisor) startCommand() (execChild, error) {
	var cmd *exec.Cmd
	if s.Command != nil {
		cmd = s.Command()
	} else {
		self, err := os.Executable()
		if err != nil {
			return execChild{}, fmt.Errorf("find own executable: %w", err)
		}
		cmd = exec.Command(self, os.Args[1:]...) //nolint:gosec,noctx // re-executes this binary; a context would SIGKILL it
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, SupervisorPIDEnv+"="+strconv.Itoa(os.Getpid()))
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	err := cmd.Start()
	if err != nil {
		return execChild{}, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	return execChild{process: cmd.Process}, nil
}

// waitFunc collects one exited child without blocking. It returns pid 0 when
// none is left, and an error when there is nothing to wait for.
type waitFunc func() (pid int, status syscall.WaitStatus, err error)

// waitNoHang is wait4(-1, WNOHANG): it collects any child of this process, which
// includes the processes the kernel reparented to it as PID 1.
func waitNoHang() (int, syscall.WaitStatus, error) {
	var status syscall.WaitStatus
	for {
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}

		return pid, status, err //nolint:wrapcheck // the caller tells ECHILD apart from a pid
	}
}

// reapOnSIGCHLD collects every exited child, including adopted ones, whenever a
// SIGCHLD arrives, and reports each on exits. It sleeps in between, so an idle
// supervisor does no work. It returns when ctx ends.
func reapOnSIGCHLD(ctx context.Context, wait waitFunc, exits chan<- exitStatus) {
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	defer signal.Stop(sigchld)

	for {
		if !reapAll(ctx, wait, exits) {
			return
		}
		select {
		case <-sigchld:
		case <-ctx.Done():
			return
		}
	}
}

// reapAll collects children until none has exited. It returns false when ctx
// ended while it was reporting one.
func reapAll(ctx context.Context, wait waitFunc, exits chan<- exitStatus) bool {
	for {
		pid, status, err := wait()
		if err != nil || pid <= 0 {
			return true
		}
		code := status.ExitStatus()
		if status.Signaled() {
			code = signalExitBase + int(status.Signal())
		}
		select {
		case exits <- exitStatus{pid: pid, code: code}:
		case <-ctx.Done():
			return false
		}
	}
}
