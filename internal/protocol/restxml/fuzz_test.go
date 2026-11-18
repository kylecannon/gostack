package restxml_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/restxml"
)

// FuzzDecode walks any body the way generated code does, reading leaves as
// every type and recursing into the rest. It must not panic, must stop, and
// must report a malformed body only as ErrMalformed.
func FuzzDecode(f *testing.F) {
	f.Add(`<s3:R xmlns:s3="u" a="1"><s3:K>v</s3:K><L><member>1</member></L></s3:R>`)
	f.Add(`<?xml version="1.0"?><R><T>2019-12-16T23:48:18Z</T><B>aGk=</B><E/></R>`)
	f.Add(`<a><b></a></b>`)
	f.Add(strings.Repeat("<a>", restxml.MaxDepth+2))
	f.Add(strings.Repeat("<a>", restxml.MaxDepth+1) + strings.Repeat("</a>", restxml.MaxDepth+1))

	f.Fuzz(func(t *testing.T, body string) {
		d := restxml.NewDecoder(strings.NewReader(body))
		if _, ok := d.Root(); ok {
			_, _ = d.Attr("a")
			walk(d, 0)
		}

		err := d.Err()
		if err != nil && !errors.Is(err, restxml.ErrMalformed) {
			t.Fatalf("error is not ErrMalformed: %v", err)
		}
	})
}

// walk reads the children of the element entered last, choosing how to read
// each by its position, so every reader meets every shape of input.
func walk(d *restxml.Decoder, i int) {
	for _, ok := d.Next(); ok; _, ok = d.Next() {
		i++

		switch i % 8 {
		case 0:
			walk(d, i)
		case 1:
			_ = d.String()
		case 2:
			_ = d.Int64()
		case 3:
			_ = d.Float64()
		case 4:
			_ = d.Bool()
		case 5:
			_ = d.Time("")
		case 6:
			_ = d.Blob()
		default:
			d.Skip()
		}
	}
}
