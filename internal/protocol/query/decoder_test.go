package query_test

import (
	"math"
	"net/url"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const valueMember = "Value"

func values(t *testing.T, raw string) url.Values {
	t.Helper()

	v, err := url.ParseQuery(raw)
	require.NoError(t, err)

	return v
}

func Test_Decoder_Scalars(t *testing.T) {
	t.Parallel()

	d := query.NewDecoder(values(t,
		"S=text&I=-7&L=9007199254740993&F=2.5&B=TRUE&Blob=aGk%3D&T=2000-01-02T20%3A34%3A56Z&U=946845296"))

	require.NotNil(t, d.String("S"))
	assert.Equal(t, "text", *d.String("S"))
	assert.Nil(t, d.String("missing"))
	assert.Equal(t, int32(-7), *d.Int32("I"))
	assert.Equal(t, int64(9007199254740993), *d.Int64("L"))
	assert.InDelta(t, 2.5, *d.Float64("F"), 0)
	assert.True(t, *d.Bool("B"))
	assert.Equal(t, []byte("hi"), d.Blob("Blob"))
	assert.Equal(t, int64(946845296), d.Time("T", query.TimeISO8601).Unix())
	assert.Equal(t, int64(946845296), d.Time("U", query.TimeUnix).Unix())
	require.Nil(t, d.Err())
}

func Test_Decoder_Floats(t *testing.T) {
	t.Parallel()

	d := query.NewDecoder(values(t, "A=NaN&B=Infinity&C=-Infinity"))

	assert.True(t, math.IsNaN(*d.Float64("A")))
	assert.True(t, math.IsInf(*d.Float64("B"), 1))
	assert.True(t, math.IsInf(*d.Float64("C"), -1))
	require.Nil(t, d.Err())
}

func Test_Decoder_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		giveRaw string
		read    func(*query.Decoder)
	}{
		{name: "int32 not a number", giveRaw: "N=5.0", read: func(d *query.Decoder) { d.Int32("N") }},
		{name: "int64 not a number", giveRaw: "N=ten", read: func(d *query.Decoder) { d.Int64("N") }},
		{name: "float not a number", giveRaw: "N=x", read: func(d *query.Decoder) { d.Float64("N") }},
		{name: "bool not true or false", giveRaw: "N=yes", read: func(d *query.Decoder) { d.Bool("N") }},
		{name: "blob with a dangling data character", giveRaw: "N=aGVsb", read: func(d *query.Decoder) { d.Blob("N") }},
		{name: "timestamp not iso8601", giveRaw: "N=yesterday", read: func(d *query.Decoder) { d.Time("N", "") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := query.NewDecoder(values(t, tt.giveRaw))
			tt.read(d)

			require.NotNil(t, d.Err())
			assert.Equal(t, "InvalidParameterValue", d.Err().Code)
			assert.Equal(t, 400, d.Err().StatusCode)
		})
	}

	t.Run("the first error is kept", func(t *testing.T) {
		t.Parallel()

		d := query.NewDecoder(values(t, "A=x&B=y"))
		d.Int32("A")
		d.Int32("B")

		require.NotNil(t, d.Err())
		assert.Contains(t, d.Err().Message, "A")
	})
}

func Test_Decoder_Has(t *testing.T) {
	t.Parallel()

	d := query.NewDecoder(values(t, "Nested.Foo=1&AttributeName.1=a&Plain=x"))

	assert.True(t, d.Has("Nested"), "a key below counts")
	assert.True(t, d.Has("Plain"), "the key itself counts")
	assert.False(t, d.Has("Attribute"), "a longer name does not match without the dot")
	assert.False(t, d.Has("Nest"))
}

func Test_Decoder_List(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		giveRaw  string
		giveSpec query.ListSpec
		want     []string
	}{
		{
			name:     "member list",
			giveRaw:  "L.member.2=b&L.member.1=a",
			giveSpec: query.ListSpec{Names: []string{"L"}},
			want:     []string{"L.member.1", "L.member.2"},
		},
		{
			name:     "member list with a renamed member",
			giveRaw:  "L.item.1=a",
			giveSpec: query.ListSpec{Names: []string{"L"}, Member: "item"},
			want:     []string{"L.item.1"},
		},
		{
			name:     "flattened list",
			giveRaw:  "L.1=a&L.2=b&L.3=c",
			giveSpec: query.ListSpec{Names: []string{"L"}, Flattened: true},
			want:     []string{"L.1", "L.2", "L.3"},
		},
		{
			name:     "a gap ends the list",
			giveRaw:  "L.1=a&L.3=c",
			giveSpec: query.ListSpec{Names: []string{"L"}, Flattened: true},
			want:     []string{"L.1"},
		},
		{
			name:     "element 1 must exist for the list to exist",
			giveRaw:  "L.2=b",
			giveSpec: query.ListSpec{Names: []string{"L"}, Flattened: true},
			want:     nil,
		},
		{
			name:     "complex elements exist through the keys below them",
			giveRaw:  "L.member.1.Name=a&L.member.2.Name=b",
			giveSpec: query.ListSpec{Names: []string{"L"}, Complex: true},
			want:     []string{"L.member.1", "L.member.2"},
		},
		{
			name:     "the element number matches whole, so 10 is not 1",
			giveRaw:  "L.member.10.Name=a",
			giveSpec: query.ListSpec{Names: []string{"L"}, Complex: true},
			want:     nil,
		},
		{
			name:     "the primary name wins",
			giveRaw:  "AttributeName.1=a&AttributeNames.1=b",
			giveSpec: query.ListSpec{Names: []string{"AttributeName", "AttributeNames"}, Flattened: true},
			want:     []string{"AttributeName.1"},
		},
		{
			name:     "the alias is used when only it is present",
			giveRaw:  "AttributeNames.1=a&AttributeNames.2=b",
			giveSpec: query.ListSpec{Names: []string{"AttributeName", "AttributeNames"}, Flattened: true},
			want:     []string{"AttributeNames.1", "AttributeNames.2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := query.NewDecoder(values(t, tt.giveRaw))

			assert.Equal(t, tt.want, d.List("", &tt.giveSpec))
		})
	}

	t.Run("lists nest under a prefix", func(t *testing.T) {
		t.Parallel()

		d := query.NewDecoder(values(t, "Parent.L.member.1=a"))

		assert.Equal(t, []string{"Parent.L.member.1"}, d.List("Parent", &query.ListSpec{Names: []string{"L"}}))
	})
}

func Test_Decoder_Map(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		giveRaw  string
		giveSpec query.MapSpec
		want     []query.Entry
	}{
		{
			name:     "entry map",
			giveRaw:  "M.entry.1.key=a&M.entry.1.value=1&M.entry.2.key=b&M.entry.2.value=2",
			giveSpec: query.MapSpec{Names: []string{"M"}},
			want: []query.Entry{
				{Key: "M.entry.1.key", Value: "M.entry.1.value"},
				{Key: "M.entry.2.key", Value: "M.entry.2.value"},
			},
		},
		{
			name:     "flattened map with renamed key and value",
			giveRaw:  "M.1.K=a&M.1.V=1",
			giveSpec: query.MapSpec{Names: []string{"M"}, Flattened: true, Key: "K", Value: "V"},
			want:     []query.Entry{{Key: "M.1.K", Value: "M.1.V"}},
		},
		{
			name:     "an entry without a value ends the map",
			giveRaw:  "M.entry.1.key=a&M.entry.1.value=1&M.entry.2.key=b",
			giveSpec: query.MapSpec{Names: []string{"M"}},
			want:     []query.Entry{{Key: "M.entry.1.key", Value: "M.entry.1.value"}},
		},
		{
			name:     "complex values exist through the keys below them",
			giveRaw:  "M.1.Name=a&M.1.Value.StringValue=x",
			giveSpec: query.MapSpec{Names: []string{"M"}, Flattened: true, Key: "Name", Value: valueMember, ComplexValue: true},
			want:     []query.Entry{{Key: "M.1.Name", Value: "M.1.Value"}},
		},
		{
			name:     "alias spelled with Key",
			giveRaw:  "Tags.1.Key=a&Tags.1.Value=1",
			giveSpec: query.MapSpec{Names: []string{"Tag", "Tags"}, Flattened: true, Key: "Key", Value: valueMember},
			want:     []query.Entry{{Key: "Tags.1.Key", Value: "Tags.1.Value"}},
		},
		{
			name:    "alias spelled with Name for the key",
			giveRaw: "MessageAttributes.1.Name=a&MessageAttributes.1.Value=1",
			giveSpec: query.MapSpec{
				Names: []string{"MessageAttribute", "MessageAttributes"}, Flattened: true, Key: "Name", Value: valueMember,
			},
			want: []query.Entry{{Key: "MessageAttributes.1.Name", Value: "MessageAttributes.1.Value"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := query.NewDecoder(values(t, tt.giveRaw))

			assert.Equal(t, tt.want, d.Map("", &tt.giveSpec))
		})
	}
}

func Test_Value(t *testing.T) {
	t.Parallel()

	s := "x"

	assert.Equal(t, "x", query.Value(&s))
	assert.Empty(t, query.Value[string](nil))
	assert.Zero(t, query.Value[int32](nil))
}
