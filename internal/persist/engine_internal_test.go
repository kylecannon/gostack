package persist

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nopSnapshotter struct{}

func (nopSnapshotter) Snapshot(io.Writer) error { return nil }
func (nopSnapshotter) Restore(io.Reader) error  { return nil }

func Test_mountinfoHas(t *testing.T) {
	const info = `22 28 0:21 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs none rw
30 28 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
41 30 8:17 /data /persisted-data rw,relatime shared:20 - ext4 /dev/sdb1 rw
42 30 8:33 / /with\040space rw,relatime - ext4 /dev/sdc1 rw
43 30 8:34 / /persisted-data-other rw,relatime - ext4 /dev/sdd1 rw
short line
`

	tests := []struct {
		name     string
		giveInfo string
		givePath string
		want     bool
	}{
		{name: "mounted", giveInfo: info, givePath: "/persisted-data", want: true},
		{name: "root mount", giveInfo: info, givePath: "/", want: true},
		{name: "prefix of another mount", giveInfo: info, givePath: "/persisted", want: false},
		{name: "escaped space", giveInfo: info, givePath: "/with space", want: true},
		{name: "not mounted", giveInfo: info, givePath: "/nowhere", want: false},
		{name: "empty file", giveInfo: "", givePath: "/persisted-data", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mountinfoHas(strings.NewReader(tt.giveInfo), tt.givePath))
		})
	}
}

func Test_Engine_timer(t *testing.T) {
	e := New(Config{PersistVarSet: true, BaseDir: t.TempDir(), Frequency: time.Hour})
	e.Register("sqs", nopSnapshotter{})
	t.Cleanup(func() { _ = e.Close(t.Context()) })

	require.Nil(t, e.timer, "no timer while idle")

	e.MarkDirty("sqs")
	first := e.timer
	require.NotNil(t, first)

	e.MarkDirty("sqs")
	e.MarkDirty("sqs")
	assert.Same(t, first, e.timer, "a burst arms one timer")

	require.NoError(t, e.Flush(t.Context()))
	assert.Nil(t, e.timer, "flush leaves no timer behind")

	e.MarkDirty("sqs")
	assert.NotSame(t, first, e.timer)
	require.NotNil(t, e.timer)

	require.NoError(t, e.Close(t.Context()))
	assert.Nil(t, e.timer)

	e.MarkDirty("sqs")
	assert.Nil(t, e.timer, "no timer after Close")
}
