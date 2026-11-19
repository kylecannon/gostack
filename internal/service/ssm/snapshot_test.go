package ssm_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// putSample writes the state the persistence tests save: a SecureString with
// two versions, a label on the first and a tag, a plain String, and a
// maintenance window. It returns the window's id.
func putSample(c *client) any {
	c.ok(kPutParameter, obj{
		kName: appDB, kValue: "one", kType: "SecureString",
		kTags: []obj{{fieldKey: "team", kValue: "core"}},
	})
	c.ok(kPutParameter, obj{kName: appDB, kValue: "two", kOverwrite: true})
	c.ok(kLabelParameterVersion, obj{kName: appDB, fieldVersion: 1, fieldLabels: []string{"first"}})
	c.ok(kPutParameter, obj{kName: "plain", kValue: "p", kType: kString})

	return c.ok(kCreateMaintenanceWindow, dailyWindow("w"))[fieldWindowID]
}

// dailyWindow is a CreateMaintenanceWindow body for a window named name.
func dailyWindow(name string) obj {
	return obj{
		kName: name, "Schedule": "rate(1 day)", "Duration": 1, "Cutoff": 0, "AllowUnassociatedTargets": true,
	}
}

// checkSample reads back through r everything putSample wrote.
func checkSample(t *testing.T, r *client, windowID any) {
	t.Helper()

	require.Equal(t, "one", param(r.ok(kGetParameter, obj{kName: "/app/db:first", "WithDecryption": true}))[kValue])
	require.Equal(t, "kms:alias/aws/ssm:two", param(r.ok(kGetParameter, obj{kName: appDB}))[kValue])
	require.Len(t, list(r.ok(kGetParameterHistory, obj{kName: appDB}), kParameters), 2)
	require.Len(t, list(r.ok(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: appDB}),
		kTagList), 1)
	require.Equal(t, "p", param(r.ok(kGetParameter, obj{kName: "/plain"}))[kValue])
	require.Equal(t, "w", r.ok(kGetMaintenanceWindow, obj{fieldWindowID: windowID})[kName])
}

// Test_snapshotRoundTrip saves parameters with versions, labels and tags and a
// window, restores them into a new provider, and reads them back.
func Test_snapshotRoundTrip(t *testing.T) {
	source := ssm.New()
	id := putSample(newClient(t, source))

	var file bytes.Buffer
	require.NoError(t, source.Snapshot(&file))
	require.Contains(t, file.String(), "kms:alias/aws/ssm:one", "a SecureString is written as stored")

	target := ssm.New()
	require.NoError(t, target.Restore(bytes.NewReader(file.Bytes())))

	r := newClient(t, target)
	checkSample(t, r, id)

	var again bytes.Buffer
	require.NoError(t, target.Snapshot(&again))
	require.Equal(t, file.String(), again.String(), "the same state gives the same bytes")

	// A new version after the restore numbers on from the restored ones.
	require.InDelta(t, 3, r.ok(kPutParameter, obj{kName: appDB, kValue: "3", kOverwrite: true})[kVersion], 0)
}

func Test_restoreRefusesBadSnapshots(t *testing.T) {
	// Each file is valid json but for the first, so each refusal is the check
	// its name says, and the error carries that check's words.
	for name, tc := range map[string]struct{ file, want string }{
		"not json":      {"{", "ssm snapshot"},
		"wrong version": {`{"v":2,"data":{"states":[]}}`, "version 2 is not 1"},
		"no versions": {
			`{"v":1,"data":{"states":[{"account":"a","region":"r","parameters":[{"versions":[]}]}]}}`,
			"a parameter has 0 versions",
		},
		"two names": {
			`{"v":1,"data":{"states":[{"account":"a","region":"r","parameters":[{"versions":[` +
				`{"name":"x","version":1},{"name":"y","version":2}]}]}]}}`,
			`holds a version of "y"`,
		},
		"out of order": {
			`{"v":1,"data":{"states":[{"account":"a","region":"r","parameters":[{"versions":[` +
				`{"name":"x","version":2},{"name":"x","version":1}]}]}]}}`,
			"out of order",
		},
		"place twice": {
			`{"v":1,"data":{"states":[{"account":"a","region":"r","parameters":[]},` +
				`{"account":"a","region":"r","parameters":[]}]}}`,
			"appears twice",
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := ssm.New()
			c := newClient(t, p)
			c.ok(kPutParameter, obj{kName: "kept", kValue: "v", kType: kString})

			require.ErrorContains(t, p.Restore(strings.NewReader(tc.file)), tc.want)
			c.fails(kGetParameter, obj{kName: "kept"}, kParameterNotFound)
		})
	}
}

// FuzzRestore feeds the snapshot decoder arbitrary bytes. Whatever it accepts
// must snapshot again and restore to the same bytes.
func FuzzRestore(f *testing.F) {
	f.Add([]byte(`{"v":1,"data":{"states":[{"account":"000000000000","region":"us-east-1","parameters":[` +
		`{"versions":[{"name":"/a/b","type":"String","value":"v","modified":"2024-01-01T00:00:00Z","version":1,` +
		`"dataType":"text","labels":["l"],"tier":"Standard"}],"tags":{"k":"v"}}],"windows":[{"id":"mw-1",` +
		`"created":"2024-01-01T00:00:00Z"}]}]}}`))
	f.Add([]byte(`{"v":1,"data":{"states":[]}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		p := ssm.New()
		if p.Restore(bytes.NewReader(data)) != nil {
			return
		}

		var first, second bytes.Buffer
		require.NoError(t, p.Snapshot(&first))

		q := ssm.New()
		require.NoError(t, q.Restore(bytes.NewReader(first.Bytes())))
		require.NoError(t, q.Snapshot(&second))
		require.Equal(t, first.String(), second.String())
	})
}
