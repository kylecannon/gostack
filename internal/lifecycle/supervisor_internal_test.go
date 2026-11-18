//go:build unix

package lifecycle

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	helperEnv       = "GOSTACK_LIFECYCLE_HELPER"
	helperFileEnv   = "GOSTACK_LIFECYCLE_HELPER_FILE"
	helperExitCode  = 7
	helperTestName  = "^TestHelperProcess$"
	firstFakePID    = 100
	shutdownTimeout = 5 * time.Second
)

// TestHelperProcess is the fake child the tests start through os/exec: the test
// binary runs itself, and this test body plays the child. It does nothing in a
// normal run.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("only runs as a child of the supervisor tests")
	}
	switch mode {
	case "exit":
		exitChild(helperExitCode)
	case "wait-term":
		terms := make(chan os.Signal, 1)
		signal.Notify(terms, syscall.SIGTERM)
		appendLine(os.Getenv(helperFileEnv), "started "+os.Getenv(SupervisorPIDEnv))
		<-terms
		appendLine(os.Getenv(helperFileEnv), "terminated")
		exitChild(0)
	default:
		exitChild(2)
	}
}

// exitChild ends the fake child with the given status, skipping the test
// framework's own exit handling.
func exitChild(code int) {
	os.Exit(code) //nolint:revive // deep-exit: the fake child must exit with a chosen status
}

func appendLine(path, line string) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		exitChild(3)
	}
	_, _ = file.WriteString(line + "\n")
	_ = file.Close()
}

func helperCommand(t *testing.T, mode, file string) func() *exec.Cmd {
	t.Helper()

	return func() *exec.Cmd {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run="+helperTestName)
		cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperFileEnv+"="+file)

		return cmd
	}
}

// fakeChild records the signals it is sent.
type fakeChild struct {
	pid      int
	signals  chan os.Signal
	released atomic.Bool
}

func (f *fakeChild) PID() int { return f.pid }

func (f *fakeChild) Signal(sig os.Signal) error {
	f.signals <- sig

	return nil
}

func (f *fakeChild) Release() { f.released.Store(true) }

// drain returns the signals received so far.
func (f *fakeChild) drain() []os.Signal {
	var got []os.Signal
	for {
		select {
		case sig := <-f.signals:
			got = append(got, sig)
		default:
			return got
		}
	}
}

// rig runs Supervisor.supervise against fake children and a hand-fed reaper.
type rig struct {
	supervisor Supervisor
	signals    chan os.Signal
	exits      chan exitStatus
	children   []*fakeChild
	start      func() (childProcess, error)
	result     chan supervised
}

type supervised struct {
	code int
	err  error
}

func newRig(ctx context.Context, timeout time.Duration) *rig {
	r := &rig{
		supervisor: Supervisor{ShutdownTimeout: timeout},
		signals:    make(chan os.Signal, signalBuffer),
		exits:      make(chan exitStatus),
		result:     make(chan supervised, 1),
	}
	r.start = func() (childProcess, error) { return r.newChild(), nil }
	go func() {
		code, err := r.supervisor.supervise(ctx, r.start, r.signals, r.exits)
		r.result <- supervised{code: code, err: err}
	}()

	return r
}

func (r *rig) newChild() *fakeChild {
	child := &fakeChild{pid: firstFakePID + len(r.children), signals: make(chan os.Signal, 8)}
	r.children = append(r.children, child)

	return child
}

func (r *rig) finished() (supervised, bool) {
	select {
	case got := <-r.result:
		return got, true
	default:
		return supervised{}, false
	}
}

func Test_Supervisor_supervise(t *testing.T) {
	t.Run("a child that exits on its own ends the supervisor with its code", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), shutdownTimeout)
			synctest.Wait()
			r.exits <- exitStatus{pid: firstFakePID, code: helperExitCode}
			synctest.Wait()

			got, done := r.finished()
			require.True(t, done)
			require.NoError(t, got.err)
			require.Equal(t, helperExitCode, got.code)
			require.Len(t, r.children, 1)
			require.True(t, r.children[0].released.Load())
		})
	})

	t.Run("exits of unrelated processes are ignored", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), shutdownTimeout)
			synctest.Wait()
			r.exits <- exitStatus{pid: 999, code: 1}
			synctest.Wait()

			_, done := r.finished()
			require.False(t, done)

			r.exits <- exitStatus{pid: firstFakePID}
			synctest.Wait()
			_, done = r.finished()
			require.True(t, done)
		})
	})

	t.Run("SIGTERM and SIGINT both reach the child as SIGTERM", func(t *testing.T) {
		for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t.Context(), shutdownTimeout)
				synctest.Wait()
				r.signals <- sig
				synctest.Wait()

				require.Equal(t, []os.Signal{syscall.SIGTERM}, r.children[0].drain())
				_, done := r.finished()
				require.False(t, done, "the supervisor waits for the child to end")

				r.exits <- exitStatus{pid: firstFakePID}
				synctest.Wait()
				got, done := r.finished()
				require.True(t, done)
				require.Zero(t, got.code)
				require.Len(t, r.children, 1, "a stop does not restart")
			})
		}
	})

	t.Run("SIGHUP is forwarded and the child keeps running", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), shutdownTimeout)
			synctest.Wait()
			r.signals <- syscall.SIGHUP
			synctest.Wait()

			require.Equal(t, []os.Signal{syscall.SIGHUP}, r.children[0].drain())
			_, done := r.finished()
			require.False(t, done)

			r.exits <- exitStatus{pid: firstFakePID}
			synctest.Wait()
		})
	})

	t.Run("the restart signal stops the child and starts a new one", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), shutdownTimeout)
			synctest.Wait()
			r.signals <- RestartSignal
			synctest.Wait()
			require.Equal(t, []os.Signal{syscall.SIGTERM}, r.children[0].drain())

			r.exits <- exitStatus{pid: firstFakePID}
			synctest.Wait()
			require.Len(t, r.children, 2, "a second child started")
			require.True(t, r.children[0].released.Load())
			_, done := r.finished()
			require.False(t, done)

			r.exits <- exitStatus{pid: firstFakePID + 1, code: helperExitCode}
			synctest.Wait()
			got, done := r.finished()
			require.True(t, done)
			require.Equal(t, helperExitCode, got.code)
			require.Len(t, r.children, 2)
		})
	})

	t.Run("a second restart request while stopping sends no second SIGTERM", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), shutdownTimeout)
			synctest.Wait()
			r.signals <- RestartSignal
			r.signals <- RestartSignal
			synctest.Wait()

			require.Equal(t, []os.Signal{syscall.SIGTERM}, r.children[0].drain())
			r.exits <- exitStatus{pid: firstFakePID}
			synctest.Wait()
			require.Len(t, r.children, 2)
			r.exits <- exitStatus{pid: firstFakePID + 1}
			synctest.Wait()
		})
	})

	t.Run("SIGTERM during a pending restart cancels the restart", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), shutdownTimeout)
			synctest.Wait()
			r.signals <- RestartSignal
			r.signals <- syscall.SIGTERM
			synctest.Wait()

			r.exits <- exitStatus{pid: firstFakePID}
			synctest.Wait()
			_, done := r.finished()
			require.True(t, done)
			require.Len(t, r.children, 1)
		})
	})

	t.Run("a restart request while stopping for SIGTERM does not restart", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), shutdownTimeout)
			synctest.Wait()
			r.signals <- syscall.SIGTERM
			r.signals <- RestartSignal
			synctest.Wait()

			r.exits <- exitStatus{pid: firstFakePID}
			synctest.Wait()
			_, done := r.finished()
			require.True(t, done)
			require.Len(t, r.children, 1)
		})
	})

	t.Run("a child that ignores SIGTERM is killed after SHUTDOWN_TIMEOUT", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), shutdownTimeout)
			synctest.Wait()
			r.signals <- syscall.SIGTERM
			synctest.Wait()
			require.Equal(t, []os.Signal{syscall.SIGTERM}, r.children[0].drain())

			time.Sleep(shutdownTimeout - time.Second)
			synctest.Wait()
			require.Empty(t, r.children[0].drain(), "still within the timeout")

			time.Sleep(time.Second)
			synctest.Wait()
			require.Equal(t, []os.Signal{syscall.SIGKILL}, r.children[0].drain())

			r.exits <- exitStatus{pid: firstFakePID, code: signalExitBase + int(syscall.SIGKILL)}
			synctest.Wait()
			got, done := r.finished()
			require.True(t, done)
			require.Equal(t, signalExitBase+int(syscall.SIGKILL), got.code)
		})
	})

	t.Run("a child that stops in time is not killed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), shutdownTimeout)
			synctest.Wait()
			r.signals <- syscall.SIGTERM
			synctest.Wait()
			r.children[0].drain()

			time.Sleep(shutdownTimeout - time.Second)
			r.exits <- exitStatus{pid: firstFakePID}
			synctest.Wait()
			_, done := r.finished()
			require.True(t, done)

			time.Sleep(time.Hour)
			require.Empty(t, r.children[0].drain())
		})
	})

	t.Run("a zero timeout waits without a limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t.Context(), 0)
			synctest.Wait()
			r.signals <- syscall.SIGTERM
			synctest.Wait()
			r.children[0].drain()

			time.Sleep(24 * time.Hour)
			synctest.Wait()
			require.Empty(t, r.children[0].drain())
			r.exits <- exitStatus{pid: firstFakePID}
			synctest.Wait()
		})
	})

	t.Run("a cancelled context stops the child like SIGTERM", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			r := newRig(ctx, shutdownTimeout)
			synctest.Wait()
			cancel()
			synctest.Wait()

			require.Equal(t, []os.Signal{syscall.SIGTERM}, r.children[0].drain())
			r.exits <- exitStatus{pid: firstFakePID}
			synctest.Wait()
			_, done := r.finished()
			require.True(t, done)
		})
	})

	t.Run("a child that cannot start is an error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			errBoom := errors.New("boom")
			r := &rig{
				supervisor: Supervisor{},
				signals:    make(chan os.Signal, signalBuffer),
				exits:      make(chan exitStatus),
				start:      func() (childProcess, error) { return nil, errBoom },
			}
			code, err := r.supervisor.supervise(t.Context(), r.start, r.signals, r.exits)
			require.ErrorIs(t, err, errBoom)
			require.Equal(t, 1, code)
		})
	})
}

func Test_reapAll(t *testing.T) {
	t.Run("collects exited children and reports their codes", func(t *testing.T) {
		exitCmd := helperCommand(t, "exit", "")()
		require.NoError(t, exitCmd.Start())
		killCmd := helperCommand(t, "wait-term", filepath.Join(t.TempDir(), "log"))()
		require.NoError(t, killCmd.Start())
		require.NoError(t, killCmd.Process.Signal(syscall.SIGKILL))

		want := map[int]int{
			exitCmd.Process.Pid: helperExitCode,
			killCmd.Process.Pid: signalExitBase + int(syscall.SIGKILL),
		}
		got := map[int]int{}
		exits := make(chan exitStatus, len(want))
		require.Eventually(t, func() bool {
			require.True(t, reapAll(t.Context(), waitNoHang, exits))
			for len(exits) > 0 {
				exit := <-exits
				got[exit.pid] = exit.code
			}

			return len(got) == len(want)
		}, 10*time.Second, 10*time.Millisecond)
		require.Equal(t, want, got)

		// Nothing is left: a further reap finds no child and reports nothing.
		require.True(t, reapAll(t.Context(), waitNoHang, exits))
		require.Empty(t, exits)
	})

	t.Run("stops reporting when the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		calls := 0
		wait := func() (int, syscall.WaitStatus, error) {
			calls++

			return 42, 0, nil
		}
		require.False(t, reapAll(ctx, wait, make(chan exitStatus)))
		require.Equal(t, 1, calls)
	})
}

func Test_Supervisor_Run(t *testing.T) {
	t.Run("a real child's exit code is the supervisor's", func(t *testing.T) {
		supervisor := Supervisor{Command: helperCommand(t, "exit", ""), ShutdownTimeout: shutdownTimeout}
		code, err := supervisor.runWith(t.Context(), make(chan os.Signal))
		require.NoError(t, err)
		require.Equal(t, helperExitCode, code)
	})

	t.Run("SIGTERM stops a real child and its code is returned", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "log")
		signals := make(chan os.Signal, 1)
		supervisor := Supervisor{Command: helperCommand(t, "wait-term", log), ShutdownTimeout: shutdownTimeout}
		done := make(chan supervised, 1)
		go func() {
			code, err := supervisor.runWith(t.Context(), signals)
			done <- supervised{code: code, err: err}
		}()

		waitForLines(t, log, 1)
		signals <- syscall.SIGTERM
		got := <-done
		require.NoError(t, got.err)
		require.Zero(t, got.code)
		lines := readLines(t, log)
		require.Equal(t, "started "+strconv.Itoa(os.Getpid()), lines[0], "the child gets SUPERVISOR_PID")
		require.Equal(t, "terminated", lines[1])
	})

	t.Run("an ended context stops a real child and the exit is seen", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "log")
		ctx, cancel := context.WithCancel(t.Context())
		supervisor := Supervisor{Command: helperCommand(t, "wait-term", log), ShutdownTimeout: shutdownTimeout}
		done := make(chan supervised, 1)
		go func() {
			code, err := supervisor.runWith(ctx, make(chan os.Signal))
			done <- supervised{code: code, err: err}
		}()

		waitForLines(t, log, 1)
		cancel()

		select {
		case got := <-done:
			require.NoError(t, got.err)
			require.Zero(t, got.code, "the child ended on the SIGTERM the context sent")
			require.Equal(t, "terminated", readLines(t, log)[1])
		case <-time.After(shutdownTimeout):
			t.Fatal("the supervisor never saw the child exit: its reaper stopped with the context")
		}
	})

	t.Run("the restart signal replaces a real child", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "log")
		signals := make(chan os.Signal, 2)
		supervisor := Supervisor{Command: helperCommand(t, "wait-term", log), ShutdownTimeout: shutdownTimeout}
		done := make(chan supervised, 1)
		go func() {
			code, err := supervisor.runWith(t.Context(), signals)
			done <- supervised{code: code, err: err}
		}()

		waitForLines(t, log, 1)
		signals <- RestartSignal
		waitForLines(t, log, 3)
		signals <- syscall.SIGTERM
		got := <-done
		require.NoError(t, got.err)
		require.Zero(t, got.code)
		require.Equal(t, []string{
			"started " + strconv.Itoa(os.Getpid()), "terminated",
			"started " + strconv.Itoa(os.Getpid()), "terminated",
		}, readLines(t, log))
	})

	t.Run("a missing executable is an error", func(t *testing.T) {
		missing := func() *exec.Cmd { return exec.CommandContext(t.Context(), "/nonexistent/gostack-child") }
		supervisor := Supervisor{Command: missing}
		code, err := supervisor.runWith(t.Context(), make(chan os.Signal))
		require.Error(t, err)
		require.Equal(t, 1, code)
	})
}

func Test_SignalRestart(t *testing.T) {
	t.Run("without SUPERVISOR_PID there is no supervisor", func(t *testing.T) {
		t.Setenv(SupervisorPIDEnv, "")
		require.ErrorIs(t, SignalRestart(), ErrNoSupervisor)
	})

	t.Run("a bad SUPERVISOR_PID is an error", func(t *testing.T) {
		t.Setenv(SupervisorPIDEnv, "not-a-pid")
		err := SignalRestart()
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNoSupervisor)
	})

	t.Run("the restart signal reaches the supervisor's pid", func(t *testing.T) {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, RestartSignal)
		t.Cleanup(func() { signal.Stop(signals) })
		t.Setenv(SupervisorPIDEnv, strconv.Itoa(os.Getpid()))

		require.NoError(t, SignalRestart())
		require.Equal(t, RestartSignal, <-signals)
	})
}

func Test_NeedsSupervisor(t *testing.T) {
	require.Equal(t, os.Getpid() == 1, NeedsSupervisor())
}

func waitForLines(t *testing.T, path string, want int) {
	t.Helper()
	require.Eventually(t, func() bool { return len(readLines(t, path)) >= want }, 10*time.Second, 10*time.Millisecond)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)

	return strings.Split(strings.TrimSpace(string(data)), "\n")
}
