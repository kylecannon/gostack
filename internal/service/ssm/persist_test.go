package ssm_test

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// ssmName is the name the engine keys SSM's state by.
const ssmName = "ssm"

// handlerFor is the gateway in front of p alone, telling eng about every
// change. A nil eng is told nothing.
func handlerFor(tb testing.TB, p *ssm.Provider, eng gateway.Dirtier) http.Handler {
	tb.Helper()

	settings := func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} }

	g, err := gateway.New(settings, []service.Registration{p.Registration()}, nil, eng)
	require.NoError(tb, err)

	return g.Handler()
}

// clientWith is newClient with eng told about every change.
func clientWith(t *testing.T, p *ssm.Provider, eng gateway.Dirtier) *client {
	t.Helper()

	server := httptest.NewServer(handlerFor(t, p, eng))
	t.Cleanup(server.Close)

	return &client{t: t, url: server.URL}
}

// startEngine loads SSM from dir through an engine that writes format, and
// returns the engine and a client of the loaded provider.
func startEngine(t *testing.T, dir string, format persist.Format) (*persist.Engine, *client) {
	t.Helper()

	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Formats: []persist.Format{format},
		MountDetector: func(string) bool { return false },
	})

	p := ssm.New()
	eng.Register(ssmName, p.Registration().Persist)
	require.NoError(t, eng.Load(t.Context()))

	return eng, clientWith(t, p, eng)
}

// TestPersistedSSMSurvivesARestart writes parameters with versions, labels,
// tags and a SecureString through the gateway, flushes in each PERSIST_FORMAT,
// and loads them into a new provider.
func TestPersistedSSMSurvivesARestart(t *testing.T) {
	for format, file := range map[persist.Format]string{
		persist.FormatJSON: "store.json", persist.FormatBinary: "store.bin",
	} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			eng, c := startEngine(t, dir, format)

			id := putSample(c)
			require.NoError(t, eng.Flush(t.Context()))

			_, err := os.Stat(filepath.Join(dir, ssmName, file))
			require.NoError(t, err, "the state is written as %s", file)

			_, r := startEngine(t, dir, format)
			checkSample(t, r, id)
		})
	}
}

// marks counts MarkDirty calls.
type marks struct {
	mu sync.Mutex
	n  int
}

func (m *marks) MarkDirty(string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.n++
}

func (m *marks) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.n
}

// The parameters the dirty-marking test writes.
const (
	dirtyA = "/d/a"
	dirtyB = "/d/b"
)

// with is a copy of body with key set to value.
func with(body obj, key string, value any) obj {
	out := maps.Clone(body)
	out[key] = value

	return out
}

// TestSSMOnlyChangesMarkItDirty sends each implemented operation once: the
// ones that change state mark SSM dirty, the reads and a 501 do not.
func TestSSMOnlyChangesMarkItDirty(t *testing.T) {
	m := &marks{}
	c := clientWith(t, ssm.New(), m)

	tags := obj{kResourceType: resourceParameter, kResourceID: dirtyA}
	for _, step := range []struct {
		op      string
		body    obj
		changes bool
	}{
		{kPutParameter, obj{kName: dirtyA, kValue: "v", kType: kString}, true},
		{kPutParameter, obj{kName: dirtyB, kValue: "v", kType: kString}, true},
		{kGetParameter, obj{kName: dirtyA}, false},
		{kGetParameters, obj{fieldNames: []string{dirtyA}}, false},
		{kGetParametersByPath, obj{kPath: "/d"}, false},
		{kDescribeParameters, obj{}, false},
		{kGetParameterHistory, obj{kName: dirtyA}, false},
		{kLabelParameterVersion, obj{kName: dirtyA, fieldLabels: []string{"l"}}, true},
		{"UnlabelParameterVersion", obj{kName: dirtyA, fieldVersion: 1, fieldLabels: []string{"l"}}, true},
		{"AddTagsToResource", with(tags, kTags, []obj{{fieldKey: "k", kValue: "v"}}), true},
		{kListTagsForResource, tags, false},
		{"RemoveTagsFromResource", with(tags, "TagKeys", []string{"k"}), true},
		{"DescribeMaintenanceWindows", obj{}, false},
		{"DeleteParameter", obj{kName: dirtyA}, true},
		{"DeleteParameters", obj{fieldNames: []string{dirtyB}}, true},
	} {
		before := m.count()
		c.ok(step.op, step.body)

		if step.changes {
			require.Greater(t, m.count(), before, "%s marks SSM dirty", step.op)
		} else {
			require.Equal(t, before, m.count(), "%s leaves SSM clean", step.op)
		}
	}

	before := m.count()
	id := c.ok(kCreateMaintenanceWindow, dailyWindow("win"))[fieldWindowID]
	require.Greater(t, m.count(), before, "CreateMaintenanceWindow marks SSM dirty")

	before = m.count()
	c.ok(kGetMaintenanceWindow, obj{fieldWindowID: id})
	require.Equal(t, before, m.count(), "GetMaintenanceWindow leaves SSM clean")

	c.ok(kDeleteMaintenanceWindow, obj{fieldWindowID: id})
	require.Greater(t, m.count(), before, "DeleteMaintenanceWindow marks SSM dirty")

	before = m.count()
	status, _ := c.call("SendCommand", obj{"DocumentName": "AWS-RunShellScript"})
	require.Equal(t, 501, status)
	require.Equal(t, before, m.count(), "a 501 leaves SSM clean")
}
