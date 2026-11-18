package s3

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
