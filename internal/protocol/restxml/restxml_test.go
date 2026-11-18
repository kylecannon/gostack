package restxml_test

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/kylecannon/gostack/internal/protocol/restxml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_Decoder(t *testing.T) {
	t.Parallel()

	t.Run("namespaced and bare bodies read alike", func(t *testing.T) {
		t.Parallel()

		for _, body := range []string{
			`<s3:Cfg xmlns:s3="http://s3.amazonaws.com/doc/2006-03-01/"><s3:Loc>eu</s3:Loc></s3:Cfg>`,
			`<Cfg xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Loc>eu</Loc></Cfg>`,
			`<?xml version="1.0"?><!-- c --><Cfg><Loc>eu</Loc></Cfg>`,
		} {
			d := restxml.NewDecoder(strings.NewReader(body))

			root, ok := d.Root()
			require.True(t, ok, body)
			assert.Equal(t, "Cfg", root)

			name, ok := d.Next()
			require.True(t, ok)
			assert.Equal(t, "Loc", name)
			assert.Equal(t, "eu", *d.String())

			_, ok = d.Next()
			require.False(t, ok)
			require.NoError(t, d.Err())
		}
	})

	t.Run("typed values and attributes", func(t *testing.T) {
		t.Parallel()

		d := restxml.NewDecoder(strings.NewReader(`<R xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" ` +
			`xsi:type="CanonicalUser"><I> 12 </I><F>-Infinity</F><B>TRUE</B><T>2019-12-16T23:48:18Z</T>` +
			`<Bl>aGk=</Bl><E/><S>  a &amp; b  </S><X><Y>skipped</Y></X></R>`))

		_, ok := d.Root()
		require.True(t, ok)

		typ, ok := d.Attr("xsi:type")
		require.True(t, ok)
		assert.Equal(t, "CanonicalUser", typ)

		_, ok = d.Attr("xsi")
		assert.False(t, ok, "a namespace declaration is no attribute")

		got := map[string]any{}

		for name, ok := d.Next(); ok; name, ok = d.Next() {
			switch name {
			case "I":
				got[name] = *d.Int32()
			case "F":
				got[name] = *d.Float64() < 0
			case "B":
				got[name] = *d.Bool()
			case "T":
				got[name] = d.Time("").Unix()
			case "Bl":
				got[name] = string(d.Blob())
			case "E":
				got[name] = d.Blob()
			case "S":
				got[name] = *d.String()
			default:
				d.Skip()
			}
		}

		require.NoError(t, d.Err())
		assert.Equal(t, map[string]any{
			"I": int32(12), "F": true, "B": true, "T": int64(1576540098), "Bl": "hi", "E": []byte{}, "S": "  a & b  ",
		}, got)
	})

	t.Run("an empty body has no root and no error", func(t *testing.T) {
		t.Parallel()

		d := restxml.NewDecoder(strings.NewReader(" \n"))
		_, ok := d.Root()
		require.False(t, ok)
		require.NoError(t, d.Err())
	})
}

func Test_Decoder_Malformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		giveBody string
	}{
		{name: "mismatched end tag", giveBody: "<a><b></a></b>"},
		{name: "prefix mismatch", giveBody: "<p:a></q:a>"},
		{name: "body ends inside an element", giveBody: "<a><b>"},
		{name: "not xml", giveBody: "<a><<"},
		{name: "unknown entity", giveBody: "<a>&ent;</a>"},
		{name: "too deep", giveBody: strings.Repeat("<a>", restxml.MaxDepth+1)},
		{name: "wrong type", giveBody: "<a><n>x</n></a>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := restxml.NewDecoder(strings.NewReader(tt.giveBody))
			if _, ok := d.Root(); ok {
				for _, ok := d.Next(); ok; _, ok = d.Next() {
					_ = d.Int32()
				}
			}

			require.ErrorIs(t, d.Err(), restxml.ErrMalformed)
		})
	}
}

// element is a parsed element, to compare what the Writer wrote without
// comparing its bytes.
type element struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Text    string     `xml:",chardata"`
	Kids    []element  `xml:",any"`
}

func Test_Writer(t *testing.T) {
	t.Parallel()

	tm := &protocol.Time{Time: time.Date(2019, 12, 16, 23, 48, 18, 500000000, time.UTC)}

	w := restxml.NewWriter()
	w.Open("Root")
	w.Namespace("", "http://s3.amazonaws.com/doc/2006-03-01/")
	w.Open("Grantee")
	w.Namespace("xsi", "http://www.w3.org/2001/XMLSchema-instance")
	w.Attr("xsi:type", `a"b`)
	w.Close("Grantee")
	w.String("Empty", "")
	w.String("Escaped", "a&<b>\r\"")
	w.Time("When", tm, rest.TimeS3)
	w.Int("N", -3)
	w.Float("F", 1.5)
	w.Bool("B", false)
	w.Blob("Bl", []byte("hi"))
	w.Open("Scalar")
	w.Namespace("", "urn:x")
	w.Text("v")
	w.Close("Scalar")
	w.Close("Root")

	var got element

	//nolint:musttag // element takes any element; XMLName and the any fields need no names
	require.NoError(t, xml.NewDecoder(bytes.NewReader(w.Bytes())).Decode(&got))
	assert.Equal(t, "http://s3.amazonaws.com/doc/2006-03-01/", got.XMLName.Space)

	texts := map[string]string{}
	for _, k := range got.Kids {
		texts[k.XMLName.Local] = k.Text
	}

	assert.Equal(t, map[string]string{
		"Grantee": "", "Empty": "", "Escaped": "a&<b>\r\"", "When": "2019-12-16T23:48:18.000Z",
		"N": "-3", "F": "1.5", "B": "false", "Bl": "aGk=", "Scalar": "v",
	}, texts)

	require.Len(t, got.Kids[0].Attrs, 2)
	assert.Equal(t, `a"b`, got.Kids[0].Attrs[1].Value)
}
