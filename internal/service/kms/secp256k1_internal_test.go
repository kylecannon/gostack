package kms

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// FuzzParseSECGK1Private: the secp256k1 private key reader never panics, and a
// key it reads writes back as DER it reads as the same key.
func FuzzParseSECGK1Private(f *testing.F) {
	f.Add(unhex(f, k1PKCS8))
	f.Add(unhex(f, k1SEC1))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, der []byte) {
		key, err := parseSECGK1Private(der)
		if err != nil {
			return
		}

		out, err := marshalSECGK1Private(key)
		require.NoError(t, err)

		again, err := parseSECGK1Private(out)
		require.NoError(t, err)
		require.True(t, again.PubKey().IsEqual(key.PubKey()))
	})
}

// FuzzParseSECGK1Public: the secp256k1 public key reader, which reads a peer
// key of DeriveSharedSecret, never panics, and a key it reads writes back as
// DER it reads as the same key.
func FuzzParseSECGK1Public(f *testing.F) {
	f.Add(unhex(f, k1SPKI))
	f.Add(unhex(f, k1PeerSPKI))
	f.Add([]byte("InvalidPublicKey"))

	f.Fuzz(func(t *testing.T, der []byte) {
		key, err := parseSECGK1Public(der)
		if err != nil {
			return
		}

		out, err := marshalSECGK1Public(key)
		require.NoError(t, err)

		again, err := parseSECGK1Public(out)
		require.NoError(t, err)
		require.True(t, again.IsEqual(key))
	})
}

// secp256k1Module is the dependency only this package may import.
const secp256k1Module = "github.com/decred/dcrd/dcrec/secp256k1"

// Test_secp256k1_importedOnlyHere walks every Go file of the repository and
// fails when a package other than internal/service/kms imports the secp256k1
// dependency, which the user approved for KMS alone.
func Test_secp256k1_importedOnlyHere(t *testing.T) {
	t.Parallel()

	here, err := os.Getwd()
	require.NoError(t, err)

	root := moduleRoot(t, here)
	fset := token.NewFileSet()

	var (
		importers []string
		files     int
	)

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		files++

		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}

		for _, spec := range file.Imports {
			imported, unquoteErr := strconv.Unquote(spec.Path.Value)
			if unquoteErr != nil {
				return fmt.Errorf("read an import of %s: %w", path, unquoteErr)
			}

			if strings.HasPrefix(imported, secp256k1Module) && filepath.Dir(path) != here {
				importers = append(importers, path)
			}
		}

		return nil
	})
	require.NoError(t, err)
	require.Greater(t, files, 100, "the walk found too few Go files to have covered the repository")
	require.Empty(t, importers, "only internal/service/kms may import "+secp256k1Module)
}

// moduleRoot is the nearest directory at or above dir that holds go.mod and is
// the gostack module.
func moduleRoot(t *testing.T, dir string) string {
	t.Helper()

	for {
		mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(mod), "module github.com/kylecannon/gostack\n") {
			return dir
		}

		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no gostack go.mod above the package")

		dir = parent
	}
}
