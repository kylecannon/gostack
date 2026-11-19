package sqs

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

const (
	decimalBase    = 10
	floatPrecision = 256 // bits, enough to cut a json number of any size the body holds
)

// sentNumber is the number a caller sent for the member name, for an error
// message. The decoders read an integer into an int32 and clamp one past that
// range to the nearest bound, where LocalStack keeps the whole number and
// echoes it. So a value that is not a bound is itself. A bound is looked up
// again in what the request carried, which the generated readers keep: the form
// values of a query call, the body of a json call. The first member called
// name that lies past the bound is the one; the bound itself is the answer when
// nothing is found.
func sentNumber(rc *request.Context, name string, value int) string {
	shown := strconv.Itoa(value)

	if (value != math.MaxInt32 && value != math.MinInt32) || rc == nil || rc.Request == nil {
		return shown
	}

	var candidates []string

	if rc.Request.Form != nil {
		candidates = formMembers(rc.Request.Form, name)
	} else {
		candidates = jsonMembers(rc.Request, name)
	}

	for _, text := range candidates {
		decimal, ok := protocol.PyIntDecimal(text)
		if ok && past(decimal, value) {
			return decimal
		}
	}

	return shown
}

// past reports whether the integer in decimal lies beyond bound, which is
// math.MaxInt32 or math.MinInt32, on the side the bound is on.
func past(decimal string, bound int) bool {
	n, ok := new(big.Int).SetString(decimal, decimalBase)
	if !ok {
		return false
	}

	order := n.Cmp(big.NewInt(int64(bound)))
	if bound > 0 {
		return order > 0
	}

	return order < 0
}

// formMembers returns the values of every form key that is name or ends in
// ".name", which is how a member of a list entry is spelled.
func formMembers(form map[string][]string, name string) []string {
	var out []string

	for key, values := range form {
		if key == name || strings.HasSuffix(key, "."+name) {
			out = append(out, values...)
		}
	}

	return out
}

// jsonMembers returns, as text, every number or string the json body holds
// under a member called name. It reads the body and puts it back.
func jsonMembers(r *http.Request, name string) []string {
	if r.Body == nil {
		return nil
	}

	raw, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))

	if err != nil {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var doc any

	if dec.Decode(&doc) != nil {
		return nil
	}

	return collect(doc, name, nil)
}

// collect appends the scalars called name anywhere in doc. A json number with a
// fraction or an exponent is cut to an integer, as Python's int(float()) does.
func collect(doc any, name string, out []string) []string {
	switch v := doc.(type) {
	case map[string]any:
		for key, member := range v {
			if key == name {
				out = appendScalar(out, member)

				continue
			}

			out = collect(member, name, out)
		}
	case []any:
		for _, member := range v {
			out = collect(member, name, out)
		}
	default:
		// A scalar under another name is not what is looked for.
	}

	return out
}

func appendScalar(out []string, v any) []string {
	switch s := v.(type) {
	case string:
		return append(out, s)
	case json.Number:
		text := s.String()
		if !strings.ContainsAny(text, ".eE") {
			return append(out, text)
		}

		f, _, err := big.ParseFloat(text, decimalBase, floatPrecision, big.ToZero)
		if err != nil {
			return out
		}

		n, _ := f.Int(nil)

		return append(out, n.String())
	default:
		return out
	}
}
