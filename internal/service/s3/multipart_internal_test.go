package s3

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommitVersionThenRunsAfterEveryCheck: the step that follows a commit does
// not run when any check refuses, the second of several included, and runs
// once, with the version already the key's current one, when none does.
func TestCommitVersionThenRunsAfterEveryCheck(t *testing.T) {
	errRefused := errors.New("refused")
	pass := func(*Object) error { return nil }
	refuse := func(*Object) error { return errRefused }

	tests := []struct {
		name      string
		giveCheck []func(*Object) error
		wantErr   error
		wantRuns  int
	}{
		{"no check", nil, nil, 1},
		{"every check passes", []func(*Object) error{pass, nil, pass}, nil, 1},
		{"the first check refuses", []func(*Object) error{refuse, pass}, errRefused, 0},
		{"the second check refuses", []func(*Object) error{pass, refuse}, errRefused, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t)
			b := &Bucket{Name: bucketName, Objects: map[string]*Object{}, Uploads: map[string]*MultipartUpload{}}

			w, err := p.blobs.Create()
			require.NoError(t, err)

			id, _, err := w.Commit()
			require.NoError(t, err)

			runs, current := 0, false

			err = p.commitVersionThen(b, "k", &Version{BlobID: id, ETag: "e"}, func() {
				runs++
				current = b.Objects["k"] != nil && b.Objects["k"].current() != nil
			}, tt.giveCheck...)

			require.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, tt.wantRuns, runs)
			assert.Equal(t, tt.wantRuns == 1, current, "the step sees the committed version")
		})
	}
}

func TestVersionPartRange(t *testing.T) {
	whole := &Version{Size: 7, ETag: "e"}
	parted := &Version{Size: 9, Parts: []ObjectPart{
		{Number: 1, Offset: 0, Size: 5, ETag: "a"},
		{Number: 2, Offset: 5, Size: 4, ETag: "b"},
	}}

	tests := []struct {
		name     string
		give     *Version
		giveN    int32
		wantPart ObjectPart
		wantOK   bool
	}{
		{"an object put whole is its own part 1", whole, 1, ObjectPart{Number: 1, Size: 7, ETag: "e"}, true},
		{"an object put whole has no part 2", whole, 2, ObjectPart{}, false},
		{"a multipart object's second part", parted, 2, ObjectPart{Number: 2, Offset: 5, Size: 4, ETag: "b"}, true},
		{"past a multipart object's last part", parted, 3, ObjectPart{}, false},
		{"part zero", parted, 0, ObjectPart{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			part, ok := tt.give.partRange(tt.giveN)
			assert.Equal(t, tt.wantOK, ok)

			if ok {
				assert.Equal(t, tt.wantPart, part)
			}
		})
	}
}
