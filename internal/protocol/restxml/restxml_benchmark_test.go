package restxml_test

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/kylecannon/gostack/internal/protocol/restxml"
)

const listKeys = 1000

// encodeListObjectsV2 writes a ListObjectsV2 result of listKeys keys, as the
// generated S3 code will.
func encodeListObjectsV2(w *restxml.Writer, modified *protocol.Time) {
	w.Open("ListBucketResult")
	w.Namespace("", "http://s3.amazonaws.com/doc/2006-03-01/")
	w.String("Name", "bucket")
	w.String("Prefix", "")
	w.Int("KeyCount", listKeys)
	w.Int("MaxKeys", listKeys)
	w.Bool("IsTruncated", false)

	for i := range listKeys {
		w.Open("Contents")
		w.String("Key", "photos/2019/12/"+strconv.Itoa(i)+".jpg")
		w.Time("LastModified", modified, rest.TimeS3)
		w.String("ETag", `"9b2cf535f27731c974343645a3985328"`)
		w.Int("Size", 65536)
		w.String("StorageClass", "STANDARD")
		w.Close("Contents")
	}

	w.Close("ListBucketResult")
}

// BenchmarkListObjectsV2 encodes a 1,000-key ListObjectsV2 result, and decodes
// it the way generated code reads a body.
func BenchmarkListObjectsV2(b *testing.B) {
	modified := &protocol.Time{Time: time.Date(2019, 12, 16, 23, 48, 18, 0, time.UTC)}

	b.Run("op=encode/keys=1000", func(b *testing.B) {
		w := restxml.NewWriter()
		encodeListObjectsV2(w, modified)
		b.SetBytes(int64(len(w.Bytes())))
		b.ReportAllocs()

		for b.Loop() {
			encodeListObjectsV2(restxml.NewWriter(), modified)
		}
	})

	w := restxml.NewWriter()
	encodeListObjectsV2(w, modified)
	body := w.Bytes()

	b.Run("op=decode/keys=1000", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()

		for b.Loop() {
			decodeListObjectsV2(b, body)
		}
	})
}

func decodeListObjectsV2(b *testing.B, body []byte) {
	b.Helper()

	d := restxml.NewDecoder(bytes.NewReader(body))
	d.Root()

	keys := 0

	for name, ok := d.Next(); ok; name, ok = d.Next() {
		if name != "Contents" {
			d.Skip()

			continue
		}

		for field, ok := d.Next(); ok; field, ok = d.Next() {
			switch field {
			case "LastModified":
				_ = d.Time(rest.TimeS3)
			case "Size":
				_ = d.Int64()
			default:
				_ = d.String()
			}
		}

		keys++
	}

	if d.Err() != nil || keys != listKeys {
		b.Fatalf("decoded %d keys: %v", keys, d.Err())
	}
}
