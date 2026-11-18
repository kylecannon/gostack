package lifecycle_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

const (
	helloPython   = "hello.py"
	stageShutdown = "shutdown"
	logFileEnv    = "GOSTACK_TEST_HOOK_LOG"
	appendBody    = "#!/bin/sh\necho \"$(basename \"$0\")\" >> \"$" + logFileEnv + "\"\n"
)

// hookRoot is a temporary init root and the log file the test scripts append to.
type hookRoot struct {
	t    *testing.T
	root string
	log  string
}

func newHookRoot(t *testing.T) *hookRoot {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "hook.log")
	t.Setenv(logFileEnv, log)

	return &hookRoot{t: t, root: filepath.Join(dir, "init"), log: log}
}

// write creates a file under the root with the given mode.
func (h *hookRoot) write(rel, body string, mode os.FileMode) {
	h.t.Helper()
	path := filepath.Join(h.root, rel)
	require.NoError(h.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(h.t, os.WriteFile(path, []byte(body), mode))
}

// ran returns the names of the scripts that ran, in order.
func (h *hookRoot) ran() []string {
	h.t.Helper()
	data, err := os.ReadFile(h.log)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(h.t, err)

	return strings.Fields(string(data))
}

func states(scripts []lifecycle.Script) map[string]lifecycle.State {
	got := make(map[string]lifecycle.State, len(scripts))
	for _, script := range scripts {
		got[filepath.Base(script.Path)] = script.State
	}

	return got
}

func Test_Hooks_Run(t *testing.T) {
	t.Run("scripts run in sorted order, a directory's files before its subdirectories", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("boot.d/10-b.sh", appendBody, 0o755)
		h.write("boot.d/01-a.sh", appendBody, 0o755)
		h.write("boot.d/05-sub/02-nested.sh", appendBody, 0o755)
		h.write("boot.d/20-z.sh", appendBody, 0o755)
		h.write("boot.d/00-sub/03-first.sh", appendBody, 0o755)
		hooks := lifecycle.NewHooks(h.root)

		scripts := hooks.Run(t.Context(), lifecycle.StageBoot)

		require.Equal(t, []string{"01-a.sh", "10-b.sh", "20-z.sh", "03-first.sh", "02-nested.sh"}, h.ran())
		require.Len(t, scripts, 5)
		for _, script := range scripts {
			require.Equal(t, lifecycle.StateSuccessful, script.State)
			require.Equal(t, lifecycle.StageBoot, script.Stage)
			require.True(t, filepath.IsAbs(script.Path))
		}
		require.True(t, hooks.Completed(lifecycle.StageBoot))
	})

	t.Run("each stage reads its own directory", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("boot.d/boot.sh", appendBody, 0o755)
		h.write("start.d/start.sh", appendBody, 0o755)
		h.write("ready.d/ready.sh", appendBody, 0o755)
		h.write("shutdown.d/shutdown.sh", appendBody, 0o755)
		hooks := lifecycle.NewHooks(h.root)

		for _, stage := range lifecycle.Stages() {
			require.False(t, hooks.Completed(stage))
			require.Len(t, hooks.Run(t.Context(), stage), 1)
			require.True(t, hooks.Completed(stage))
		}

		require.Equal(t, []string{"boot.sh", "start.sh", "ready.sh", "shutdown.sh"}, h.ran())
	})

	t.Run("a failing script is marked ERROR and the stage goes on", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("ready.d/01-fail.sh", "#!/bin/sh\nexit 3\n", 0o755)
		h.write("ready.d/02-ok.sh", appendBody, 0o755)
		hooks := lifecycle.NewHooks(h.root)

		got := states(hooks.Run(t.Context(), lifecycle.StageReady))

		require.Equal(t, map[string]lifecycle.State{
			"01-fail.sh": lifecycle.StateError,
			"02-ok.sh":   lifecycle.StateSuccessful,
		}, got)
		require.Equal(t, []string{"02-ok.sh"}, h.ran())
	})

	t.Run("a .sh script without the executable bit is an error", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("start.d/plain.sh", appendBody, 0o644)
		hooks := lifecycle.NewHooks(h.root)

		got := states(hooks.Run(t.Context(), lifecycle.StageStart))

		require.Equal(t, map[string]lifecycle.State{"plain.sh": lifecycle.StateError}, got)
		require.Empty(t, h.ran(), "there is no /bin/sh fallback")
	})

	t.Run("a script whose interpreter is missing is an error and the log names it", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("start.d/ghost.sh", "#!/nonexistent/interpreter\n", 0o755)
		var logs bytes.Buffer
		hooks := lifecycle.NewHooks(h.root)
		hooks.Logger = slog.New(slog.NewTextHandler(&logs, nil))

		got := states(hooks.Run(t.Context(), lifecycle.StageStart))

		require.Equal(t, map[string]lifecycle.State{"ghost.sh": lifecycle.StateError}, got)
		require.Contains(t, logs.String(), "/nonexistent/interpreter")
	})

	t.Run("python scripts run under python3", func(t *testing.T) {
		_, err := exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 is not installed")
		}
		h := newHookRoot(t)
		h.write("boot.d/"+helloPython, "import os\nopen(os.environ['"+logFileEnv+"'], 'a').write('hello.py\\n')\n", 0o644)
		h.write("boot.d/broken.py", "raise SystemExit(4)\n", 0o644)
		hooks := lifecycle.NewHooks(h.root)

		got := states(hooks.Run(t.Context(), lifecycle.StageBoot))

		require.Equal(t, map[string]lifecycle.State{
			"broken.py": lifecycle.StateError,
			helloPython: lifecycle.StateSuccessful,
		}, got)
		require.Equal(t, []string{helloPython}, h.ran())
	})

	t.Run("python scripts fail with a named interpreter when python3 is missing", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("boot.d/"+helloPython, "print('hi')\n", 0o644)
		t.Setenv("PATH", t.TempDir())
		var logs bytes.Buffer
		hooks := lifecycle.NewHooks(h.root)
		hooks.Logger = slog.New(slog.NewTextHandler(&logs, nil))

		got := states(hooks.Run(t.Context(), lifecycle.StageBoot))

		require.Equal(t, map[string]lifecycle.State{helloPython: lifecycle.StateError}, got)
		require.Contains(t, logs.String(), "python3")
	})

	t.Run("files without a runner and non-regular entries are not scripts", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("boot.d/notes.txt", "x", 0o755)
		h.write("boot.d/run.bash", appendBody, 0o755)
		h.write("boot.d/real.sh", appendBody, 0o755)
		require.NoError(t, os.Symlink("real.sh", filepath.Join(h.root, "boot.d", "link.sh")))
		require.NoError(t, os.Symlink("missing.sh", filepath.Join(h.root, "boot.d", "broken.sh")))
		require.NoError(t, os.Symlink(filepath.Join(h.root, "boot.d"), filepath.Join(h.root, "boot.d", "loop")))
		hooks := lifecycle.NewHooks(h.root)

		got := states(hooks.Run(t.Context(), lifecycle.StageBoot))

		require.Equal(t, map[string]lifecycle.State{
			"link.sh": lifecycle.StateSuccessful,
			"real.sh": lifecycle.StateSuccessful,
		}, got)
	})

	t.Run("a stage runs once", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("ready.d/once.sh", appendBody, 0o755)
		hooks := lifecycle.NewHooks(h.root)

		hooks.Run(t.Context(), lifecycle.StageReady)
		again := hooks.Run(t.Context(), lifecycle.StageReady)

		require.Equal(t, []string{"once.sh"}, h.ran())
		require.Equal(t, map[string]lifecycle.State{"once.sh": lifecycle.StateSuccessful}, states(again))
	})

	t.Run("a missing root has no scripts and the stage still completes", func(t *testing.T) {
		hooks := lifecycle.NewHooks(filepath.Join(t.TempDir(), "absent"))

		require.Empty(t, hooks.Run(t.Context(), lifecycle.StageBoot))
		require.True(t, hooks.Completed(lifecycle.StageBoot))
		require.False(t, hooks.Completed(lifecycle.StageStart))
	})

	t.Run("scripts inherit the process environment and output goes to the writers", func(t *testing.T) {
		h := newHookRoot(t)
		t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")
		h.write("start.d/env.sh", "#!/bin/sh\necho \"region=$AWS_DEFAULT_REGION\"\necho oops >&2\n", 0o755)
		var stdout, stderr bytes.Buffer
		hooks := lifecycle.NewHooks(h.root)
		hooks.Stdout, hooks.Stderr = &stdout, &stderr

		hooks.Run(t.Context(), lifecycle.StageStart)

		require.Equal(t, "region=eu-west-1\n", stdout.String())
		require.Equal(t, "oops\n", stderr.String())
	})

	t.Run("a cancelled context kills a running script", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("start.d/slow.sh", "#!/bin/sh\nexec sleep 60\n", 0o755)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		hooks := lifecycle.NewHooks(h.root)

		got := states(hooks.Run(ctx, lifecycle.StageStart))

		require.Equal(t, map[string]lifecycle.State{"slow.sh": lifecycle.StateError}, got)
	})
}

func Test_Hooks_Scripts(t *testing.T) {
	t.Run("scripts are known before they run", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("ready.d/a.sh", appendBody, 0o755)
		hooks := lifecycle.NewHooks(h.root)

		got := hooks.Scripts(lifecycle.StageReady)

		require.Equal(t, map[string]lifecycle.State{"a.sh": lifecycle.StateUnknown}, states(got))
		require.False(t, hooks.Completed(lifecycle.StageReady))
	})

	t.Run("the result is a copy", func(t *testing.T) {
		h := newHookRoot(t)
		h.write("ready.d/a.sh", appendBody, 0o755)
		hooks := lifecycle.NewHooks(h.root)

		hooks.Scripts(lifecycle.StageReady)[0].State = lifecycle.StateError

		require.Equal(t, lifecycle.StateUnknown, hooks.Scripts(lifecycle.StageReady)[0].State)
	})
}

func Test_Stage(t *testing.T) {
	tests := []struct {
		name      string
		give      string
		want      lifecycle.Stage
		wantFound bool
	}{
		{name: "upper case", give: "BOOT", want: lifecycle.StageBoot, wantFound: true},
		{name: "lower case", give: "start", want: lifecycle.StageStart, wantFound: true},
		{name: "mixed case", give: "Ready", want: lifecycle.StageReady, wantFound: true},
		{name: stageShutdown, give: stageShutdown, want: lifecycle.StageShutdown, wantFound: true},
		{name: "unknown", give: "later", wantFound: false},
		{name: "empty", give: "", wantFound: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := lifecycle.ParseStage(tt.give)
			require.Equal(t, tt.wantFound, found)
			require.Equal(t, tt.want, got)
		})
	}

	t.Run("names match LocalStack's", func(t *testing.T) {
		require.Equal(t, "BOOT", lifecycle.StageBoot.String())
		require.Equal(t, "SHUTDOWN", lifecycle.StageShutdown.String())
		require.Equal(t, "UNKNOWN", lifecycle.Stage(0).String())
		require.Equal(t, "RUNNING", lifecycle.StateRunning.String())
		require.Equal(t, "SUCCESSFUL", lifecycle.StateSuccessful.String())
		require.Equal(t, "ERROR", lifecycle.StateError.String())
		require.Equal(t, "UNKNOWN", lifecycle.StateUnknown.String())
		require.Equal(t, "UNKNOWN", lifecycle.State(99).String())
	})
}
