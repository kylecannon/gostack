// Command gostack-codegen writes the wire layer of each AWS service from its
// botocore model: the Go types, the codecs for every protocol the service
// speaks, the error constructors, and the Serve entry point. Its output is
// internal/api/<service>.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kylecannon/gostack/codegen/spec"
)

func main() {
	err := run(os.Args[1:], os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gostack-codegen:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("gostack-codegen", flag.ContinueOnError)
	specs := fs.String("specs", "specs", "directory the service models are read from")
	out := fs.String("out", "internal/api", "directory the service packages are written to")
	services := fs.String("services", "", "comma-separated botocore service names to generate")

	err := fs.Parse(args)
	if err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if *services == "" {
		return errors.New("-services is required")
	}

	loader, err := spec.NewLoader(*specs)
	if err != nil {
		return fmt.Errorf("load specs: %w", err)
	}

	defer func() { _ = loader.Close() }()

	outRoot, err := os.OpenRoot(*out)
	if err != nil {
		return fmt.Errorf("open output directory: %w", err)
	}

	defer func() { _ = outRoot.Close() }()

	for name := range strings.SplitSeq(*services, ",") {
		name = strings.TrimSpace(name)

		err = generate(name, loader, outRoot)
		if err != nil {
			return err
		}

		_, err = fmt.Fprintf(stdout, "generated %s\n", name)
		if err != nil {
			return fmt.Errorf("report %s: %w", name, err)
		}
	}

	return nil
}

const (
	dirPerm  = 0o750
	filePerm = 0o600
)

// generate writes the six files of one service into outRoot/<service>.
func generate(name string, loader *spec.Loader, outRoot *os.Root) error {
	g, err := newGenerator(name, loader)
	if err != nil {
		return err
	}

	files, err := g.files()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	err = outRoot.MkdirAll(name, dirPerm)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}

	for _, file := range sortedKeys(files) {
		err = outRoot.WriteFile(name+"/"+file, files[file], filePerm)
		if err != nil {
			return fmt.Errorf("write %s/%s: %w", name, file, err)
		}
	}

	return nil
}
