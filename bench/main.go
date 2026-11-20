package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const (
	cmdRun = "run"
	usage1 = "usage: bench run|compare|loadgen|gate|select [flags]"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := dispatch(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)

	stop()

	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage1)
	}

	switch args[0] {
	case cmdRun:
		return orchestrate(ctx, args[1:], stdout, stderr)
	case "compare":
		return compareMain(ctx, args[1:], stdout)
	case "loadgen":
		return loadgenMain(ctx, args[1:], stdout)
	case "gate":
		return gateMain(ctx, args[1:], stdin, stdout, nil)
	case "select":
		return selectMain(ctx, args[1:], stdout, execBin)
	default:
		return fmt.Errorf("unknown command %q: %s", args[0], usage1)
	}
}
