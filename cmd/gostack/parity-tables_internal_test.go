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
	"github.com/kylecannon/gostack/internal/service"
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
	TestDirs     []string            `json:"test_dirs"`
}

// handKeptDependsOn are the DEPENDS_ON keys that are in no import and so are
// not services that call another one, kept by hand: dynamodb is rerun with
// dynamodbstreams, whose package reads dynamodb's state, because dynamodb
// writes the stream records dynamodbstreams serves. They still have to name
// services that exist or are seeded.
var handKeptDependsOn = []string{"dynamodb"}

// Test_parityTables holds the tables in parity/ledger.py to the code. Parity
// selection runs a changed service's suite and the suites the tables say it
// can break; a table that drifts from the code skips a suite the change broke.
//
// It is a Go test because which services set a routing hook is known only by
// building registrations(), and ci.yml's parity-ledger job, which runs
// parity/test_*.py, has no Go; the test job running this has python3, which
// make parity-check needs anyway. Imports are read with go/build for the
// image's platform, linux without cgo, so no go command runs.
//
// A DEPENDS_ON entry may be seeded for a service built next: its key or a
// dependency may name a service that has a generated internal/api/<svc> package
// and no internal/service/<svc> yet. An entry for a service with neither fails,
// and so does a registered service, or a service package or generated API
// package, with no TEST_DIRS entry. A seeded key is not held to the imports
// until its service package exists.
func Test_parityTables(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	tables := readParityTables(t, root)
	graph := importGraph(t, root)
	services := serviceDirs(t, root)
	apis := packageDirs(t, root, "api", apiPrefix)
	known := slices.Concat(services, apis)
	seeded := func(svc string) bool { return !slices.Contains(services, svc) && slices.Contains(apis, svc) }

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

		// Seeded and hand-kept keys are in no import yet or at all; the
		// subtest below holds them to the services that exist.
		for key := range got {
			if _, caller := want[key]; !caller && (seeded(key) || slices.Contains(handKeptDependsOn, key)) {
				delete(got, key)
			}
		}

		require.ElementsMatch(t, slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(got)),
			"DEPENDS_ON keys in parity/ledger.py: the services whose package imports internal/inproc or another service's")

		for svc, others := range want {
			require.Subset(t, got[svc], others, "DEPENDS_ON[%q] in parity/ledger.py: the services %s imports", svc, svc)
		}
	})

	// The EveryService entry is in no import: LocalStack's session fixtures
	// account_id and secondary_account_id call sts.get_caller_identity in every
	// service's test session, so a change to sts can break any service's tests.
	// Without the entry an sts change would run only the sts suite, and the
	// checks above and below pass without it.
	t.Run("every service depends on sts", func(t *testing.T) {
		require.Contains(t, tables.DependsOn, tables.EveryService, "DEPENDS_ON in parity/ledger.py")
		require.Contains(t, tables.DependsOn[tables.EveryService], "sts",
			"DEPENDS_ON[EVERY_SERVICE] in parity/ledger.py: every service's tests call sts, in LocalStack's account_id fixtures")
	})

	t.Run("DEPENDS_ON names services that exist or have a generated API package", func(t *testing.T) {
		require.Empty(t, unknownEntries(tables, known),
			"DEPENDS_ON in parity/ledger.py: entries for services with no internal/service/<svc> and no internal/api/<svc>")
	})

	t.Run("every registered service and service package has a TEST_DIRS entry", func(t *testing.T) {
		require.Subset(t, tables.TestDirs, registeredNames(t), "TEST_DIRS in parity/ledger.py: a registered service")
		require.Subset(t, tables.TestDirs, services, "TEST_DIRS in parity/ledger.py: a package under internal/service")
		require.Subset(t, tables.TestDirs, apis, "TEST_DIRS in parity/ledger.py: a package under internal/api")
	})
}

// unknownEntries lists what DEPENDS_ON names, as a key or a dependency, that is
// not in known, and each key with no dependency.
func unknownEntries(tables parityTables, known []string) []string {
	var found []string

	for _, svc := range slices.Sorted(maps.Keys(tables.DependsOn)) {
		dependencies := tables.DependsOn[svc]

		if svc != tables.EveryService && !slices.Contains(known, svc) {
			found = append(found, "key "+svc)
		}

		if len(dependencies) == 0 {
			found = append(found, "key "+svc+" has no dependency")
		}

		for _, dep := range dependencies {
			if !slices.Contains(known, dep) {
				found = append(found, svc+" depends on "+dep)
			}
		}
	}

	return found
}

func Test_unknownEntries(t *testing.T) {
	const (
		alpha = "alpha"
		beta  = "beta"
	)

	known := []string{"gamma", alpha, beta}
	tables := func(dependsOn map[string][]string) parityTables {
		return parityTables{DependsOn: dependsOn, EveryService: "*"}
	}

	t.Run("accepts services that exist or are seeded, and the every-service key", func(t *testing.T) {
		got := unknownEntries(tables(map[string][]string{beta: {alpha}, "*": {alpha}}), known)

		require.Empty(t, got)
	})

	t.Run("refuses a key or a dependency with no service package and no API package", func(t *testing.T) {
		got := unknownEntries(tables(map[string][]string{"ghost": {alpha}, beta: {"phantom"}}), known)

		require.Equal(t, []string{"beta depends on phantom", "key ghost"}, got)
	})

	t.Run("refuses a key with no dependency", func(t *testing.T) {
		got := unknownEntries(tables(map[string][]string{beta: {}}), known)

		require.Equal(t, []string{"key beta has no dependency"}, got)
	})
}

// registeredNames lists the names of the services registrations() lists.
func registeredNames(t *testing.T) []string {
	t.Helper()

	regs := buildRegistrations(t)
	names := make([]string, 0, len(regs))

	for i := range regs {
		names = append(names, regs[i].Metadata.Name)
	}

	return names
}

// buildRegistrations builds the registrations as the binary does.
func buildRegistrations(t *testing.T) []service.Registration {
	t.Helper()

	cfg, err := config.Load()
	require.NoError(t, err)

	return newProviders(cfg, func(string) {}).regs
}

// routingReach lists the services registrations() gives a hook that runs on
// requests meant for any service.
func routingReach(t *testing.T) []string {
	t.Helper()

	regs := buildRegistrations(t)

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

	return packageDirs(t, root, "service", svcPrefix)
}

// packageDirs lists the directories under internal/<dir>/ that hold a package,
// whose import path is prefix plus the name.
func packageDirs(t *testing.T, root, dir, prefix string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, "internal", dir))
	require.NoError(t, err)

	var names []string

	for _, entry := range entries {
		if entry.IsDir() && importsOf(t, root, prefix+entry.Name()) != nil {
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
