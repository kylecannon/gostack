package main

import (
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kylecannon/gostack/codegen/spec"
)

// modulePath is the import path of the module the generator writes into.
const modulePath = "github.com/kylecannon/gostack"

// fixturePackages is the directory, below each protocol's, that holds the
// packages generated from that protocol's fixtures. Only tests import them.
const fixturePackages = "fixturegen_test"

// fixtureKinds are the two fixture files of a protocol, in registry order.
var fixtureKinds = []string{"input", "output"}

// fixtureProtocols are the protocols whose botocore fixtures run through
// generated code, each with its directory below -out.
var fixtureProtocols = []struct{ proto, dir string }{
	{protoRestXML, "restxml"},
	{protoRestJSON, "restjson"},
}

// generateFixtures writes, for each rest protocol, one package for each group
// of its botocore fixtures, generated as a service of its own, and a registry
// of them, under out/<protocol>/fixturegen_test. out is relative to the module
// root, as the import paths in the registry are built from it.
func generateFixtures(fixtures, out string, outRoot *os.Root, stdout io.Writer) error {
	root, err := os.OpenRoot(fixtures)
	if err != nil {
		return fmt.Errorf("open fixture directory: %w", err)
	}

	defer func() { _ = root.Close() }()

	for _, fp := range fixtureProtocols {
		base := fp.dir + "/" + fixturePackages
		pkgs := map[string][]string{}
		written := map[string]map[string][]byte{}

		for _, kind := range fixtureKinds {
			data, err := root.ReadFile(kind + "/" + fp.proto + ".json")
			if err != nil {
				return fmt.Errorf("read %s fixtures: %w", kind, err)
			}

			groups, err := spec.ParseFixtures(data)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", kind, fp.proto, err)
			}

			for i := range groups {
				pkg := fmt.Sprintf("%s%02d", strings.TrimSuffix(kind, "put"), i)

				files, err := fixtureGroupFiles(pkg, &groups[i])
				if err != nil {
					return fmt.Errorf("%s/%s group %d (%s): %w", kind, fp.proto, i, groups[i].Description, err)
				}

				written[base+"/"+pkg] = files
				pkgs[kind] = append(pkgs[kind], pkg)
			}
		}

		registry, err := fixtureRegistry(path.Join(modulePath, filepath.ToSlash(filepath.Clean(out)), base), pkgs)
		if err != nil {
			return err
		}

		written[base] = map[string][]byte{"registry.go": registry}

		err = writeFixtureDirs(outRoot, base, written)
		if err != nil {
			return err
		}

		_, err = fmt.Fprintf(stdout, "generated %s fixtures: %d input and %d output groups\n",
			fp.proto, len(pkgs[fixtureKinds[0]]), len(pkgs[fixtureKinds[1]]))
		if err != nil {
			return fmt.Errorf("report %s fixtures: %w", fp.proto, err)
		}
	}

	return nil
}

// fixtureGroupFiles generates the service of one fixture group: its one
// operation and its shapes, under the package name pkg, plus the Fixture
// handler the harness drives it with.
func fixtureGroupFiles(pkg string, grp *spec.FixtureGroup) (map[string][]byte, error) {
	if len(grp.Cases) == 0 {
		return nil, errors.New("group has no cases")
	}

	given := grp.Cases[0].Given
	md := grp.Metadata
	md.EndpointPrefix = pkg

	svc := &spec.Service{
		Metadata:   md,
		Operations: map[string]*spec.Operation{given.Name: &given},
		Shapes:     grp.Shapes,
	}

	g, err := newModel(pkg, serviceOptions{}, svc, nil)
	if err != nil {
		return nil, err
	}

	files, err := g.files()
	if err != nil {
		return nil, err
	}

	src, err := g.render(g.fixtureFile())
	if err != nil {
		return nil, fmt.Errorf("fixture.go: %w", err)
	}

	files["fixture.go"] = src

	return files, nil
}

// fixtureFile writes the handler and the functions the fixture harness
// drives a group's service with.
func (g *generator) fixtureFile() string {
	p := &printer{}

	p.f("// Fixture is the Handler the fixture harness drives. It keeps the input of")
	p.f("// the call it gets and answers with Out, or with Err.")
	p.f("type Fixture struct {")
	p.f("\tUnimplemented")
	p.f("\tIn any")
	p.f("\tOut any")
	p.f("\tErr error")
	p.f("}")
	p.blank()

	for _, name := range g.opNames {
		op := g.ops[name]

		p.f("// %s keeps in and answers with f.Out, or f.Err.", name)
		p.f("func (f *Fixture) %s(_ context.Context, in *%s) (*%s, error) {", name, op.input.goName, op.output.goName)
		p.f("\tf.In = in")
		p.f("\tif f.Err != nil {")
		p.f("\t\treturn nil, f.Err")
		p.f("\t}")
		p.f("\tout, _ := f.Out.(*%s)", op.output.goName)
		p.f("\treturn out, nil")
		p.f("}")
		p.blank()
	}

	p.f("// Run serves rc with a Fixture that answers with out, an output of the")
	p.f("// operation or nil, or with err, and returns the input the call decoded, or nil.")
	p.f("func Run(w http.ResponseWriter, rc *request.Context, out any, err error) any {")
	p.f("\tf := &Fixture{Out: out, Err: err}")
	p.f("\tServe(f, w, rc)")
	p.f("\treturn f.In")
	p.f("}")
	p.blank()
	p.f("// NewOutput returns an empty output of the operation, for the harness to fill.")
	p.f("func NewOutput() any {")
	p.f("\treturn &%s{}", g.ops[g.opNames[0]].output.goName)
	p.f("}")
	p.blank()
	g.emitFixtureError(p)

	return p.buf.String()
}

// emitFixtureError writes NewError, which makes each error of the service.
func (g *generator) emitFixtureError(p *printer) {
	p.f("// NewError returns the members of an error shape, nil for one with none, and the")
	p.f("// function that makes the error a handler returns from them and a message. The")
	p.f("// function is nil for a shape the service does not have.")
	p.f("func NewError(shape string) (any, func(string) error) {")
	p.f("\tswitch shape {")

	for _, ex := range g.exceptions {
		p.f("\tcase %q:", ex.shape)

		if ex.members != nil {
			p.f("\t\tv := &%s{}", ex.members.goName)
			p.f("\t\treturn v, v.Err")
		} else {
			p.f("\t\treturn nil, func(msg string) error { return %s(msg) }", constructorName(ex.shape))
		}
	}

	p.f("\tdefault:")
	p.f("\t\treturn nil, nil")
	p.f("\t}")
	p.f("}")
}

// fixtureRegistry writes the package that lists the generated packages of a
// protocol's fixture groups, in the order of their files.
func fixtureRegistry(importBase string, pkgs map[string][]string) ([]byte, error) {
	var b strings.Builder

	b.WriteString(generatedHeader)
	b.WriteString("// Package fixturegen lists the services generated from the botocore protocol\n")
	b.WriteString("// fixtures, one for each group. Only tests import it.\n")
	b.WriteString("package fixturegen\n\nimport (\n\t\"net/http\"\n\n")
	fmt.Fprintf(&b, "\t%q\n\t%q\n", modulePath+"/internal/request", modulePath+"/internal/service")

	for _, kind := range fixtureKinds {
		for _, pkg := range pkgs[kind] {
			fmt.Fprintf(&b, "\t%q\n", importBase+"/"+pkg)
		}
	}

	b.WriteString(")\n\n")
	b.WriteString("// Group is the generated service of one fixture group.\n")
	b.WriteString("type Group struct {\n\tMetadata service.Metadata\n")
	b.WriteString("\tRun func(w http.ResponseWriter, rc *request.Context, out any, err error) any\n")
	b.WriteString("\tNewOutput func() any\n\tNewError func(shape string) (any, func(string) error)\n}\n")

	for _, kind := range fixtureKinds {
		fmt.Fprintf(&b, "\n// %s holds the groups of %s/<protocol>.json in file order.\n", exportName(kind), kind)
		fmt.Fprintf(&b, "var %s = []Group{\n", exportName(kind))

		for _, pkg := range pkgs[kind] {
			fmt.Fprintf(&b, "\t{Metadata: %[1]s.Metadata, Run: %[1]s.Run, NewOutput: %[1]s.NewOutput, "+
				"NewError: %[1]s.NewError},\n", pkg)
		}

		b.WriteString("}\n")
	}

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("format the fixture registry: %w", err)
	}

	return src, nil
}

// writeFixtureDirs writes the files of each directory below base and removes
// the generated files there, and in the group directories no longer written,
// that this run did not write.
func writeFixtureDirs(outRoot *os.Root, base string, written map[string]map[string][]byte) error {
	err := outRoot.MkdirAll(base, dirPerm)
	if err != nil {
		return fmt.Errorf("create %s: %w", base, err)
	}

	d, err := outRoot.Open(base)
	if err != nil {
		return fmt.Errorf("open %s: %w", base, err)
	}

	entries, err := d.ReadDir(-1)
	_ = d.Close()

	if err != nil {
		return fmt.Errorf("list %s: %w", base, err)
	}

	for _, e := range entries {
		if dir := base + "/" + e.Name(); e.IsDir() && written[dir] == nil {
			written[dir] = map[string][]byte{}
		}
	}

	for _, dir := range sortedKeys(written) {
		err = outRoot.MkdirAll(dir, dirPerm)
		if err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}

		for _, file := range sortedKeys(written[dir]) {
			err = outRoot.WriteFile(dir+"/"+file, written[dir][file], filePerm)
			if err != nil {
				return fmt.Errorf("write %s/%s: %w", dir, file, err)
			}
		}

		err = removeStale(outRoot, dir, written[dir])
		if err != nil {
			return err
		}
	}

	return nil
}
