package ssm_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// Test_snapshotRoundTrip saves parameters with versions, labels and tags and a
// window, restores them into a new provider, and reads them back.
func Test_snapshotRoundTrip(t *testing.T) {
	source := ssm.New()
	c := newClient(t, source)

	c.ok(kPutParameter, obj{
		kName: appDB, kValue: "one", kType: "SecureString",
		kTags: []obj{{fieldKey: "team", kValue: "core"}},
	})
	c.ok(kPutParameter, obj{kName: appDB, kValue: "two", kOverwrite: true})
	c.ok(kLabelParameterVersion, obj{kName: appDB, fieldVersion: 1, fieldLabels: []string{"first"}})
	c.ok(kPutParameter, obj{kName: "plain", kValue: "p", kType: kString})
	id := c.ok(kCreateMaintenanceWindow, obj{
		kName: "w", "Schedule": "rate(1 day)", "Duration": 1, "Cutoff": 0,
		"AllowUnassociatedTargets": true,
	})[fieldWindowID]

	var file bytes.Buffer
	require.NoError(t, source.Snapshot(&file))
	require.Contains(t, file.String(), "kms:alias/aws/ssm:one", "a SecureString is written as stored")

	target := ssm.New()
	require.NoError(t, target.Restore(bytes.NewReader(file.Bytes())))

	r := newClient(t, target)
	require.Equal(t, "one", param(r.ok(kGetParameter, obj{kName: "/app/db:first", "WithDecryption": true}))[kValue])
	require.Equal(t, "kms:alias/aws/ssm:two", param(r.ok(kGetParameter, obj{kName: appDB}))[kValue])
	require.Len(t, list(r.ok(kGetParameterHistory, obj{kName: appDB}), kParameters), 2)
	require.Len(t, list(r.ok(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: appDB}),
		kTagList), 1)
	require.Equal(t, "p", param(r.ok(kGetParameter, obj{kName: "/plain"}))[kValue])
	require.Equal(t, "w", r.ok(kGetMaintenanceWindow, obj{fieldWindowID: id})[kName])

	var again bytes.Buffer
	require.NoError(t, target.Snapshot(&again))
	require.Equal(t, file.String(), again.String(), "the same state gives the same bytes")

	// A new version after the restore numbers on from the restored ones.
	require.InDelta(t, 3, r.ok(kPutParameter, obj{kName: appDB, kValue: "3", kOverwrite: true})[kVersion], 0)
}

func Test_restoreRefusesBadSnapshots(t *testing.T) {
	for name, file := range map[string]string{
		"not json":      "{",
		"wrong version": `{"v":2,kData:{kStates:[]}}`,
		"no versions":   `{"v":1,kData:{kStates:[{kAccount:"a",kRegion:"r",kParametersX:[{kVersions:[]}]}]}}`,
		"two names": `{"v":1,kData:{kStates:[{kAccount:"a",kRegion:"r",kParametersX:[{kVersions:[` +
			`{kNameX:"x",kVersionX:1},{kNameX:"y",kVersionX:2}]}]}]}}`,
		"out of order": `{"v":1,kData:{kStates:[{kAccount:"a",kRegion:"r",kParametersX:[{kVersions:[` +
			`{kNameX:"x",kVersionX:2},{kNameX:"x",kVersionX:1}]}]}]}}`,
		"place twice": `{"v":1,kData:{kStates:[{kAccount:"a",kRegion:"r",kParametersX:[]},` +
			`{kAccount:"a",kRegion:"r",kParametersX:[]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := ssm.New()
			c := newClient(t, p)
			c.ok(kPutParameter, obj{kName: "kept", kValue: "v", kType: kString})

			require.Error(t, p.Restore(strings.NewReader(file)))
			c.fails(kGetParameter, obj{kName: "kept"}, kParameterNotFound)
		})
	}
}

// FuzzRestore feeds the snapshot decoder arbitrary bytes. Whatever it accepts
// must snapshot again and restore to the same bytes.
func FuzzRestore(f *testing.F) {
	f.Add([]byte(`{"v":1,kData:{kStates:[{kAccount:"000000000000",kRegion:"us-east-1",kParametersX:[` +
		`{kVersions:[{kNameX:"/a/b","type":kString,kValueX:"v","modified":"2024-01-01T00:00:00Z",kVersionX:1,` +
		`"dataType":"text","labels":["l"],"tier":"Standard"}],"tags":{"k":"v"}}],"windows":[{"id":"mw-1",` +
		`"created":"2024-01-01T00:00:00Z"}]}]}}`))
	f.Add([]byte(`{"v":1,kData:{kStates:[]}}`))

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
