package spec

import (
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
)

const modelFile = "service-2.json"

// Loader reads models from a specs directory laid out as vendor-specs.sh
// writes it: botocore/<service>/<version>/service-2.json, and LocalStack's
// spec-patches.json under localstack/.
type Loader struct {
	root    *os.Root
	patches map[string][]Patch
}

// NewLoader opens the specs directory dir and reads its patch file.
func NewLoader(dir string) (*Loader, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open specs directory: %w", err)
	}

	data, err := root.ReadFile("localstack/spec-patches.json")
	if err != nil {
		_ = root.Close()

		return nil, fmt.Errorf("read spec patches: %w", err)
	}

	patches, err := ParsePatches(data)
	if err != nil {
		_ = root.Close()

		return nil, err
	}

	return &Loader{root: root, patches: patches}, nil
}

// Close releases the specs directory.
func (l *Loader) Close() error {
	err := l.root.Close()
	if err != nil {
		return fmt.Errorf("close specs directory: %w", err)
	}

	return nil
}

// Botocore loads the latest api-version of a botocore service and applies
// LocalStack's patches for it, the way LocalStack's PatchingLoader does.
func (l *Loader) Botocore(name string) (*Service, error) {
	dir := path.Join("botocore", name)

	f, err := l.root.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}

	entries, err := f.ReadDir(-1)
	_ = f.Close()

	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}

	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}

	if len(versions) == 0 {
		return nil, fmt.Errorf("no api-version under %s", dir)
	}

	version := slices.Max(versions)

	data, err := l.root.ReadFile(path.Join(dir, version, modelFile))
	if err != nil {
		return nil, fmt.Errorf("read %s model: %w", name, err)
	}

	if ops := l.patches[path.Join(name, version, "service-2")]; len(ops) > 0 {
		data, err = ApplyPatches(data, ops)
		if err != nil {
			return nil, fmt.Errorf("patch %s model: %w", name, err)
		}
	}

	return ParseBytes(data)
}

// File loads the model at a path relative to the specs directory, without
// patches. It is how LocalStack's own sqs-query model is read.
func (l *Loader) File(rel string) (*Service, error) {
	if rel == "" {
		return nil, errors.New("empty model path")
	}

	data, err := l.root.ReadFile(rel)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}

	return ParseBytes(data)
}
