package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultInitRoot is where LocalStack looks for init hooks.
const DefaultInitRoot = "/etc/localstack/init"

const (
	shellSuffix  = ".sh"
	pythonSuffix = ".py"
	pythonBinary = "python3"
)

// Hooks discovers and runs the init scripts under a root directory. It is safe
// for concurrent use: [Hooks.Completed] and [Hooks.Scripts] can be called while a
// stage runs.
type Hooks struct {
	// Stdout and Stderr receive the output of the scripts. They default to the
	// process's own.
	Stdout, Stderr io.Writer
	// Logger receives the log lines. It defaults to slog.Default().
	Logger *slog.Logger

	root string

	runMu sync.Mutex // serializes Run

	mu         sync.Mutex // guards the fields below
	discovered bool
	scripts    map[Stage][]*Script
	completed  map[Stage]bool
}

// NewHooks returns the hooks under root, which holds boot.d, start.d, ready.d and
// shutdown.d. A missing directory means no scripts.
func NewHooks(root string) *Hooks {
	return &Hooks{root: root, completed: make(map[Stage]bool)}
}

// Run runs every script of the stage, in order, then marks the stage completed.
// A stage runs once: a second call returns the scripts without running them. A
// script that fails is logged and marked ERROR, and the stage goes on. The
// scripts are killed when ctx ends.
func (h *Hooks) Run(ctx context.Context, stage Stage) []Script {
	h.runMu.Lock()
	defer h.runMu.Unlock()

	scripts := h.discover()[stage]
	if h.Completed(stage) {
		return h.Scripts(stage)
	}
	for _, script := range scripts {
		h.logger().Debug("running init script", "stage", stage, "path", script.Path)
		h.setState(script, StateRunning)
		err := h.runScript(ctx, script.Path)
		if err != nil {
			h.setState(script, StateError)
			h.logger().Error("error while running init script", "stage", stage, "path", script.Path, "error", err)

			continue
		}
		h.setState(script, StateSuccessful)
	}
	h.mu.Lock()
	h.completed[stage] = true
	h.mu.Unlock()

	return h.Scripts(stage)
}

// Completed reports whether the stage has run.
func (h *Hooks) Completed(stage Stage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.completed[stage]
}

// Scripts returns a copy of the stage's scripts and their states. The first call
// discovers the scripts on disk.
func (h *Hooks) Scripts(stage Stage) []Script {
	discovered := h.discover()[stage]
	h.mu.Lock()
	defer h.mu.Unlock()
	scripts := make([]Script, 0, len(discovered))
	for _, script := range discovered {
		scripts = append(scripts, *script)
	}

	return scripts
}

func (h *Hooks) setState(script *Script, state State) {
	h.mu.Lock()
	defer h.mu.Unlock()
	script.State = state
}

// discover lists the scripts of every stage once. Only the pointers' State is
// written afterwards, under h.mu.
func (h *Hooks) discover() map[Stage][]*Script {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.discovered {
		return h.scripts
	}
	h.scripts = make(map[Stage][]*Script, len(Stages()))
	for _, stage := range Stages() {
		h.scripts[stage] = nil
		dir, err := filepath.Abs(filepath.Join(h.root, stage.directory()))
		if err != nil {
			continue
		}
		for _, path := range walkScripts(dir) {
			h.scripts[stage] = append(h.scripts[stage], &Script{Path: path, Stage: stage})
		}
	}
	h.discovered = true

	return h.scripts
}

// walkScripts lists the regular files with a known suffix under dir, in the order
// of Python's os.walk with sorted names: the files of a directory first, then
// each subdirectory. A symlink to a directory is not entered.
func walkScripts(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files, subdirs []string
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue // a broken symlink is not a file
		}
		switch {
		case info.IsDir() && entry.Type()&fs.ModeSymlink == 0:
			subdirs = append(subdirs, path)
		case info.Mode().IsRegular() && hasRunner(path):
			files = append(files, path)
		default:
			// not a script
		}
	}
	for _, subdir := range subdirs {
		files = append(files, walkScripts(subdir)...)
	}

	return files
}

func hasRunner(path string) bool {
	return strings.HasSuffix(path, shellSuffix) || strings.HasSuffix(path, pythonSuffix)
}

// runScript runs one script as a subprocess. A .sh file is executed itself, as
// LocalStack's subprocess.call(args=[path]) does, and a .py file runs under
// python3. The subprocess inherits the process environment, which already holds
// the bare names stripped from LOCALSTACK_*.
func (h *Hooks) runScript(ctx context.Context, path string) error {
	var cmd *exec.Cmd
	if strings.HasSuffix(path, pythonSuffix) {
		cmd = exec.CommandContext(ctx, pythonBinary, path) //nolint:gosec // running the hook is the purpose
	} else {
		cmd = exec.CommandContext(ctx, path)
	}
	cmd.Stdout, cmd.Stderr = h.Stdout, h.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exitErr):
		return fmt.Errorf("script %s returned a non-zero exit code %d", path, exitErr.ExitCode())
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("script %s needs %s, which is not installed: %w", path, pythonBinary, err)
	case errors.Is(err, fs.ErrNotExist) && shebangInterpreter(path) != "":
		return fmt.Errorf("script %s needs %s, which is not installed: %w", path, shebangInterpreter(path), err)
	default:
		return fmt.Errorf("run script %s: %w", path, err)
	}
}

// shebangInterpreter returns the interpreter named on the first line of a script,
// or "" when it has no shebang. Starting a script whose interpreter is missing
// fails with the same "no such file" error as a missing script, so the log needs
// this to name the culprit.
func shebangInterpreter(path string) string {
	file, err := os.Open(path) //nolint:gosec // the path is a script found under the init root
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()

	const maxLine = 256
	line := make([]byte, maxLine)
	n, _ := io.ReadFull(file, line)
	first, _, _ := strings.Cut(string(line[:n]), "\n")
	interpreter, found := strings.CutPrefix(first, "#!")
	if !found {
		return ""
	}
	fields := strings.Fields(interpreter)
	if len(fields) == 0 {
		return ""
	}

	return fields[0]
}

func (h *Hooks) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}

	return slog.Default()
}
