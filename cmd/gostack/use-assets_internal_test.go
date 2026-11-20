package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_providers_useAssets_logsWhyS3IsNotPersisted: S3's assets opened, but
// its state file is an absolute link out of the base dir, so the startup load
// refuses S3. useAssets then leaves S3 its ephemeral store and logs the reason
// once, with the load's error. (An unopenable base dir, as for a read-only one,
// never gets here: persist logs "s3 bodies are not persisted" for it, without
// the load's reason, and returns no assets.)
func Test_providers_useAssets_logsWhyS3IsNotPersisted(t *testing.T) {
	logs := captureLog(t)
	eng, dir := persistentEngine(t)
	state := &assetful{}

	eng.Register(s3Service, state)

	assets, err := eng.Assets(s3Service)
	require.NoError(t, err)
	require.NotNil(t, assets)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, s3Service), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "outside.json"), filepath.Join(dir, s3Service, "store.json")))

	loadErr := eng.Load(t.Context())
	require.Error(t, loadErr, "the startup load refuses S3's state file")

	(&providers{}).useAssets(eng, assets, loadErr)

	text := logs.String()
	assert.Contains(t, text, "s3 bodies are not persisted: the startup load refused s3")
	assert.Contains(t, text, "outside.json", "the load's error names the link")
}
