package blob

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
)

const (
	// memoryLimit is the size up to which a body stays in memory. A larger one
	// spills to a temporary file, as LocalStack's SpooledTemporaryFile rolls
	// over at S3_MAX_FILE_SIZE_BYTES.
	memoryLimit = 512 << 10
	// DefaultSpillDir is where the ephemeral store spills bodies over
	// memoryLimit, under the volume the container declares.
	DefaultSpillDir = "/var/lib/localstack/tmp"

	blobIDBytes = 16
	spillMode   = 0o700
)

// ErrNotFound is returned by Open and Remove for an id the store does not hold.
var ErrNotFound = errors.New("no such blob")

// Store holds the bodies of objects and parts under ids it picks. A body is
// written once and never changed: a new body is a new blob. A range read opens
// the blob and seeks; a copy of a range opens one blob, seeks, and writes into a
// new one, so no body is ever read into memory whole.
type Store interface {
	// Create starts a new blob.
	Create() (Writer, error)
	// Open returns a reader of the blob, with its own offset. The blob stays
	// readable through it until it is closed, even after Remove.
	Open(id string) (io.ReadSeekCloser, error)
	// Remove drops the blob. Readers already open keep reading it.
	Remove(id string) error
}

// Writer receives the body of one blob. Exactly one of Commit and Abort
// ends it.
type Writer interface {
	io.Writer
	// Commit stores what was written and returns the new blob's id and size.
	Commit() (id string, size int64, err error)
	// Abort drops what was written.
	Abort()
}

// Ephemeral is the Store of a service whose bodies are not persisted: bodies up
// to memoryLimit stay in memory, and larger ones spill to a temporary file that
// is unlinked as soon as it is created, so nothing is left on disk once the
// process is gone.
type Ephemeral struct {
	dir string

	mu    sync.Mutex
	blobs map[string]*blob
}

// blob is one stored body: in data, or in file when it spilled.
type blob struct {
	data []byte
	file *os.File
	size int64
	// refs counts the store's own reference and each open reader. The file is
	// closed when it drops to zero. Guarded by Ephemeral.mu.
	refs int
}

var _ Store = (*Ephemeral)(nil)

// NewEphemeral returns an empty store that spills large bodies under dir. When
// dir cannot be created or written, as on a host run outside the container, it
// spills to the system's temporary directory instead.
func NewEphemeral(dir string) *Ephemeral {
	return &Ephemeral{dir: dir, blobs: make(map[string]*blob)}
}

// Create starts a new blob.
//
//nolint:ireturn // Store's contract returns the Writer interface.
func (e *Ephemeral) Create() (Writer, error) {
	return &ephemeralWriter{store: e}, nil
}

// Open returns a reader of the blob with its own offset.
func (e *Ephemeral) Open(id string) (io.ReadSeekCloser, error) {
	e.mu.Lock()

	b, ok := e.blobs[id]
	if ok {
		b.refs++
	}

	e.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("open blob %s: %w", id, ErrNotFound)
	}

	reader := &blobReader{store: e, blob: b}
	if b.file != nil {
		reader.ReadSeeker = io.NewSectionReader(b.file, 0, b.size)
	} else {
		reader.ReadSeeker = bytes.NewReader(b.data)
	}

	return reader, nil
}

// Remove drops the blob. Readers already open keep reading it.
func (e *Ephemeral) Remove(id string) error {
	e.mu.Lock()

	b, ok := e.blobs[id]
	delete(e.blobs, id)

	e.mu.Unlock()

	if !ok {
		return fmt.Errorf("remove blob %s: %w", id, ErrNotFound)
	}

	e.release(b)

	return nil
}

// Close drops every blob. A reader still open keeps its blob until it closes.
func (e *Ephemeral) Close() {
	e.mu.Lock()

	blobs := e.blobs
	e.blobs = make(map[string]*blob)

	e.mu.Unlock()

	for _, b := range blobs {
		e.release(b)
	}
}

// Len returns the number of blobs the store holds.
func (e *Ephemeral) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return len(e.blobs)
}

func (e *Ephemeral) release(b *blob) {
	e.mu.Lock()

	b.refs--
	last := b.refs == 0

	e.mu.Unlock()

	if last && b.file != nil {
		_ = b.file.Close()
	}
}

// tempFile creates a file for a spilled body and unlinks it at once: the data
// lives as long as the file is open.
func (e *Ephemeral) tempFile() (*os.File, error) {
	file, err := createTemp(e.dir)
	if err != nil {
		file, err = createTemp(os.TempDir())
		if err != nil {
			return nil, err
		}
	}

	_ = os.Remove(file.Name())

	return file, nil
}

func createTemp(dir string) (*os.File, error) {
	err := os.MkdirAll(dir, spillMode)
	if err != nil {
		return nil, fmt.Errorf("create spill directory: %w", err)
	}

	file, err := os.CreateTemp(dir, "gostack-s3-blob-*")
	if err != nil {
		return nil, fmt.Errorf("create spill file: %w", err)
	}

	return file, nil
}

func (e *Ephemeral) add(b *blob) string {
	var raw [blobIDBytes]byte

	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])

	e.mu.Lock()
	e.blobs[id] = b
	e.mu.Unlock()

	return id
}

type blobReader struct {
	io.ReadSeeker

	store *Ephemeral
	blob  *blob
	once  sync.Once
}

// Close lets the blob go. A second Close does nothing.
func (r *blobReader) Close() error {
	r.once.Do(func() { r.store.release(r.blob) })

	return nil
}

type ephemeralWriter struct {
	store *Ephemeral
	buf   []byte
	file  *os.File
	size  int64
	err   error
}

func (w *ephemeralWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	if w.file == nil && len(w.buf)+len(p) <= memoryLimit {
		w.buf = append(w.buf, p...)
		w.size += int64(len(p))

		return len(p), nil
	}

	if w.file == nil {
		w.err = w.spill()
		if w.err != nil {
			return 0, w.err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)

	if err != nil {
		w.err = fmt.Errorf("write blob: %w", err)

		return n, w.err
	}

	return n, nil
}

func (w *ephemeralWriter) Commit() (string, int64, error) {
	if w.err != nil {
		w.Abort()

		return "", 0, w.err
	}

	id := w.store.add(&blob{data: slices.Clip(w.buf), file: w.file, size: w.size, refs: 1})
	w.file, w.buf = nil, nil

	return id, w.size, nil
}

func (w *ephemeralWriter) Abort() {
	if w.file != nil {
		_ = w.file.Close()
	}

	w.file, w.buf = nil, nil
}

// spill moves what is buffered to a temporary file, which takes every write
// after it.
func (w *ephemeralWriter) spill() error {
	file, err := w.store.tempFile()
	if err != nil {
		return err
	}

	_, err = file.Write(w.buf)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("write blob: %w", err)
	}

	w.file, w.buf = file, nil

	return nil
}
