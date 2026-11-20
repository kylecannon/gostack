package sqs

import (
	"bytes"
	"encoding/json/jsontext"
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
// under a member called name, in the order the body holds them. It reads the
// body and puts it back. It walks the body's tokens and keeps only what it
// returns, so its memory is the body and the nesting, however wide the body.
func jsonMembers(r *http.Request, name string) []string {
	if r.Body == nil {
		return nil
	}

	raw, err := protocol.ReadRequest(r)
	r.Body = io.NopCloser(bytes.NewReader(raw))

	if err != nil {
		return nil
	}

	out, err := collect(raw, name)
	if err != nil {
		return nil
	}

	return out
}

// collect returns the scalars called name anywhere in the first json value of
// raw. A member called name whose value is an object or a list is not looked
// into, and a repeated member is read each time it appears.
func collect(raw []byte, name string) ([]string, error) {
	// A bytes.Buffer is read in place, where any other reader is copied.
	dec := jsontext.NewDecoder(bytes.NewBuffer(raw),
		jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))

	var out []string

	for {
		if isMemberName(dec) {
			tok, err := dec.ReadToken()
			if err != nil {
				return nil, err //nolint:wrapcheck // the caller only checks for an error
			}

			if tok.String() == name {
				out, err = appendScalar(dec, out)
				if err != nil {
					return nil, err
				}
			}

			continue
		}

		_, err := dec.ReadToken()
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller only checks for an error
		}

		if dec.StackDepth() == 0 {
			return out, nil
		}
	}
}

// isMemberName reports whether the next token of dec is the name of an object
// member.
func isMemberName(dec *jsontext.Decoder) bool {
	kind, length := dec.StackIndex(dec.StackDepth())

	return kind == '{' && length%2 == 0 && dec.PeekKind() != '}'
}

// appendScalar reads the value after a member called name and appends it when
// it is a string or a number. A number with a fraction or an exponent is cut to
// an integer, as Python's int(float()) does.
func appendScalar(dec *jsontext.Decoder, out []string) ([]string, error) {
	switch dec.PeekKind() {
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller only checks for an error
		}

		return append(out, tok.String()), nil
	case '0':
		value, err := dec.ReadValue()
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller only checks for an error
		}

		text := string(value)
		if !strings.ContainsAny(text, ".eE") {
			return append(out, text), nil
		}

		f, _, err := big.ParseFloat(text, decimalBase, floatPrecision, big.ToZero)
		if err != nil {
			return out, nil //nolint:nilerr // a number big cannot read is not one to echo
		}

		n, _ := f.Int(nil)

		return append(out, n.String()), nil
	default:
		// An object or a list is skipped whole, and so are true, false and null.
		return out, dec.SkipValue() //nolint:wrapcheck // the caller only checks for an error
	}
}
