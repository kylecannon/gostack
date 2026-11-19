package persist

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"

	"github.com/kylecannon/gostack/internal/blob"
)

// BlobStore is a [blob.Store] over the assets of a service, for a persisted
// service whose bodies blob.Store carries, such as S3's. A missing asset is
// [blob.ErrNotFound], and is logged once per id: the state names it, so a
// read of it fails until the object is replaced or deleted.
type BlobStore struct {
	assets *Assets
	// missing holds the ids already logged as missing.
	missing sync.Map
}

var _ blob.Store = (*BlobStore)(nil)

// NewBlobStore returns the blob.Store of a.
func NewBlobStore(a *Assets) *BlobStore {
	return &BlobStore{assets: a}
}

// Create starts a new asset.
//
//nolint:ireturn // blob.Store's contract returns the Writer interface.
func (s *BlobStore) Create() (blob.Writer, error) {
	w, err := s.assets.Create()
	if err != nil {
		return nil, err
	}

	return w, nil
}

// Open returns a reader of the asset named id.
func (s *BlobStore) Open(id string) (io.ReadSeekCloser, error) {
	f, err := s.assets.Open(id)
	if err == nil {
		return f, nil
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	if _, logged := s.missing.LoadOrStore(id, struct{}{}); !logged {
		s.assets.log.Error("persist: an asset the state names is missing; reading it fails",
			"dir", s.assets.dir, "id", id)
	}

	return nil, fmt.Errorf("open asset %s: %w: %w", id, blob.ErrNotFound, err)
}

// Remove deletes the asset named id when no snapshot can name it any more,
// as [Assets.Remove] does.
func (s *BlobStore) Remove(id string) error {
	s.missing.Delete(id)

	return s.assets.Remove(id)
}
