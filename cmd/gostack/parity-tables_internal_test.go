package main

import (
	"encoding/json"
	"errors"
	"go/build"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/stretchr/testify/require"
)

const (
	modulePath = "github.com/kylecannon/gostack"
	apiPrefix  = modulePath + "/internal/api/"
	svcPrefix  = modulePath + "/internal/service/"
	inprocPath = modulePath + "/internal/inproc"
)

// parityTables are the hand-kept tables parity/ledger.py selects the services
// of a parity run with, as `ledger.py tables` prints them.
type parityTables struct {
	DependsOn    map[string][]string `json:"depends_on"`
	EveryService string              `json:"every_service"`
	RoutingReach []string            `json:"routing_reach"`
}

// Test_parityTables holds the tables in parity/ledger.py to the code. Parity
// selection runs a changed service's suite and the suites the tables say it
// can break; a table that drifts from the code skips a suite the change broke.
//
// It is a Go test because which services set a routing hook is known only by
// building registrations(), and ci.yml's parity-ledger job, which runs
// parity/test_*.py, has no Go; the test job running this has python3, which
// make parity-check needs anyway. Imports are read with go/build for the
// image's platform, linux without cgo, so no go command runs.
func Test_parityTables(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	tables := readParityTables(t, root)
	graph := importGraph(t, root)
	services := serviceDirs(t, root)

	t.Run("ROUTING_REACH lists every service with a hook on other services' requests", func(t *testing.T) {
		require.ElementsMatch(t, routingReach(t), tables.RoutingReach, "ROUTING_REACH in parity/ledger.py")
	})

	t.Run("internal/api/<svc> is imported by internal/service/<svc> alone", func(t *testing.T) {
		require.Empty(t, strayAPIImports(graph), "a change to internal/api/<svc> selects only <svc> and its dependents")
	})

	t.Run("DEPENDS_ON keys are the services that call or import other services", func(t *testing.T) {
		want := callers(graph, services)
		got := maps.Clone(tables.DependsOn)
		delete(got, tables.EveryService)

		require.ElementsMatch(t, slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(got)),
			"DEPENDS_ON keys in parity/ledger.py: the services whose package imports internal/inproc or another service's")

		for svc, others := range want {
			require.Subset(t, got[svc], others, "DEPENDS_ON[%q] in parity/ledger.py: the services %s imports", svc, svc)
		}
	})

	t.Run("DEPENDS_ON names services that exist", func(t *testing.T) {
		for svc, dependencies := range tables.DependsOn {
			if svc != tables.EveryService {
				require.Contains(t, services, svc, "a DEPENDS_ON key in parity/ledger.py")
			}

			require.NotEmpty(t, dependencies, "DEPENDS_ON[%q] in parity/ledger.py", svc)
			require.Subset(t, services, dependencies, "DEPENDS_ON[%q] in parity/ledger.py", svc)
		}
	})
}

// routingReach lists the services registrations() gives a hook that runs on
// requests meant for any service.
func routingReach(t *testing.T) []string {
	t.Helper()

	cfg, err := config.Load()
	require.NoError(t, err)

	regs := newProviders(cfg, func(string) {}).regs

	var reach []string

	for i := range regs {
		reg := &regs[i]
		if reg.Mount != nil || reg.ClaimHost != nil || reg.Claim != nil || reg.Fallback != nil || reg.Preprocess != nil {
			reach = append(reach, reg.Metadata.Name)
		}
	}

	return reach
}

// strayAPIImports lists every import of internal/api/<svc> by a package
// outside internal/service/<svc> and internal/api/<svc>.
func strayAPIImports(graph map[string][]string) []string {
	var strays []string

	for _, pkg := range slices.Sorted(maps.Keys(graph)) {
		for _, imported := range graph[pkg] {
			if svc, ok := serviceOf(imported, apiPrefix); ok && !ownedBy(pkg, svc) {
				strays = append(strays, pkg+" imports "+imported)
			}
		}
	}

	return strays
}

// callers maps each service whose package imports internal/inproc or another
// service's package, directly or not, to the other services it imports.
func callers(graph map[string][]string, services []string) map[string][]string {
	found := map[string][]string{}

	for _, svc := range services {
		deps := closure(graph, svcPrefix+svc)

		var others []string

		for _, dep := range deps {
			if other, ok := serviceDir(dep); ok && other != svc && !slices.Contains(others, other) {
				others = append(others, other)
			}
		}

		if len(others) > 0 || slices.Contains(deps, inprocPath) {
			found[svc] = others
		}
	}

	return found
}

// serviceDir returns the service a package under internal/service/ or
// internal/api/ belongs to.
func serviceDir(pkg string) (string, bool) {
	if svc, ok := serviceOf(pkg, svcPrefix); ok {
		return svc, true
	}

	return serviceOf(pkg, apiPrefix)
}

// readParityTables runs `parity/ledger.py tables`.
func readParityTables(t *testing.T, root string) parityTables {
	t.Helper()

	// go test reuses a cached pass unless a file this process opened changed;
	// python3 reads ledger.py in another process, so open it here too.
	_, err := os.ReadFile(filepath.Join(root, "parity", "ledger.py"))
	require.NoError(t, err)

	cmd := exec.CommandContext(t.Context(), "python3", "-I", "-B", "parity/ledger.py", "tables")
	cmd.Dir = root

	out, err := cmd.Output()
	require.NoError(t, err, "python3 parity/ledger.py tables")

	var tables parityTables
	require.NoError(t, json.Unmarshal(out, &tables))

	return tables
}

// serviceDirs lists the directories under internal/service/ that hold a
// package: the services parity selection knows by directory.
func serviceDirs(t *testing.T, root string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, "internal", "service"))
	require.NoError(t, err)

	var names []string

	for _, entry := range entries {
		if entry.IsDir() && importsOf(t, root, svcPrefix+entry.Name()) != nil {
			names = append(names, entry.Name())
		}
	}

	require.NotEmpty(t, names)

	return names
}

// importGraph maps every package of this module that the binary or a service
// package builds, for the image's platform, to the packages it imports.
func importGraph(t *testing.T, root string) map[string][]string {
	t.Helper()

	graph := map[string][]string{}
	queue := []string{modulePath + "/cmd/gostack"}

	entries, err := os.ReadDir(filepath.Join(root, "internal", "service"))
	require.NoError(t, err)

	for _, entry := range entries {
		if entry.IsDir() {
			queue = append(queue, svcPrefix+entry.Name())
		}
	}

	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]

		if _, seen := graph[pkg]; seen {
			continue
		}

		imports := importsOf(t, root, pkg)
		graph[pkg] = imports

		for _, imported := range imports {
			if strings.HasPrefix(imported, modulePath+"/") {
				queue = append(queue, imported)
			}
		}
	}

	return graph
}

// importsOf lists the imports of the non-test files of a package of this
// module as the image builds it, or nil when its directory holds none.
func importsOf(t *testing.T, root, pkg string) []string {
	t.Helper()

	ctx := build.Default
	ctx.GOOS = "linux"
	ctx.GOARCH = "amd64"
	ctx.CgoEnabled = false

	dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, modulePath+"/")))

	found, err := ctx.ImportDir(dir, 0)
	if _, empty := errors.AsType[*build.NoGoError](err); empty {
		return nil
	}

	require.NoError(t, err, pkg)

	return append([]string{}, found.Imports...)
}

// closure lists pkg and every package of the graph it imports, directly or not.
func closure(graph map[string][]string, pkg string) []string {
	deps := []string{pkg}

	for i := 0; i < len(deps); i++ {
		for _, imported := range graph[deps[i]] {
			if _, inModule := graph[imported]; inModule && !slices.Contains(deps, imported) {
				deps = append(deps, imported)
			}
		}
	}

	return deps
}

// serviceOf returns the service directory a package under prefix is in.
func serviceOf(pkg, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(pkg, prefix)
	if !ok {
		return "", false
	}

	svc, _, _ := strings.Cut(rest, "/")

	return svc, true
}

// ownedBy reports whether pkg is in internal/service/<svc> or internal/api/<svc>.
func ownedBy(pkg, svc string) bool {
	owner, ok := serviceDir(pkg)

	return ok && owner == svc
}
