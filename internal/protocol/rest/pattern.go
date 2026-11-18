package rest

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Pattern is the path of an operation's requestUri, such as
// "/{Bucket}/{Key+}", split into literal and label segments.
type Pattern struct {
	segments []segment
	labels   int
}

type segment struct {
	text   string // the literal, or the label name
	label  bool
	greedy bool
}

// Label is one URI label of a matched request, unescaped.
type Label struct {
	Name  string
	Value string
}

// Labels are the URI labels of a matched request, in path order.
type Labels []Label

// errPattern marks a requestUri ParsePattern cannot read.
var errPattern = errors.New("rest pattern")

// ParsePattern reads the path of a requestUri. Everything from the first "?"
// on is the literal querystring, which routing reads and matching ignores. A
// label fills a whole segment; a greedy label, "{Name+}", is the last one.
func ParsePattern(uri string) (*Pattern, error) {
	path, _, _ := strings.Cut(uri, "?")
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("%w %q: no leading slash", errPattern, uri)
	}

	p := &Pattern{}

	path = strings.TrimSuffix(path[1:], "/")
	if path == "" {
		return p, nil
	}

	parts := strings.Split(path, "/")
	for i, part := range parts {
		seg, err := parseSegment(part)
		if err != nil {
			return nil, fmt.Errorf("%w %q: %w", errPattern, uri, err)
		}

		if seg.greedy && i != len(parts)-1 {
			return nil, fmt.Errorf("%w %q: greedy label %q is not the last segment", errPattern, uri, seg.text)
		}

		if seg.label {
			p.labels++
		}

		p.segments = append(p.segments, seg)
	}

	return p, nil
}

// MustParsePattern is ParsePattern for package-level variables; it panics on
// a requestUri ParsePattern cannot read.
func MustParsePattern(uri string) *Pattern {
	p, err := ParsePattern(uri)
	if err != nil {
		panic(err)
	}

	return p
}

func parseSegment(part string) (segment, error) {
	name, isLabel := strings.CutPrefix(part, "{")
	if !isLabel {
		if strings.ContainsAny(part, "{}") {
			return segment{}, fmt.Errorf("segment %q holds a partial label", part)
		}

		return segment{text: part}, nil
	}

	name, closed := strings.CutSuffix(name, "}")
	if !closed || name == "" || strings.ContainsAny(name, "{}") {
		return segment{}, fmt.Errorf("segment %q is not one label", part)
	}

	name, greedy := strings.CutSuffix(name, "+")

	return segment{text: name, label: true, greedy: greedy}, nil
}

// Match reports whether an escaped request path, as URL.EscapedPath returns
// it, has this pattern's shape, and returns its labels.
//
// The path is split on "/" while still escaped, so an escaped "%2F" stays
// inside its label. Each label is then unescaped exactly once; "//" and ".."
// inside a greedy label are kept. A label never matches an empty segment.
// Trailing slashes are ignored unless the pattern ends in a greedy label, whose
// value keeps them, as LocalStack's router does.
func (p *Pattern) Match(escapedPath string) (Labels, bool) {
	rest, ok := strings.CutPrefix(escapedPath, "/")
	if !ok {
		return nil, false
	}

	if n := len(p.segments); n == 0 || !p.segments[n-1].greedy {
		rest = strings.TrimRight(rest, "/")
	}

	var labels Labels
	if p.labels > 0 {
		labels = make(Labels, 0, p.labels)
	}

	for i, seg := range p.segments {
		if rest == "" && i > 0 {
			return nil, false
		}

		part := rest
		if !seg.greedy {
			part, rest, _ = strings.Cut(rest, "/")
		} else {
			rest = ""
		}

		value, err := url.PathUnescape(part)
		if err != nil {
			return nil, false
		}

		if !seg.label {
			if value != seg.text {
				return nil, false
			}

			continue
		}

		if value == "" {
			return nil, false
		}

		labels = append(labels, Label{Name: seg.text, Value: value})
	}

	if rest != "" {
		return nil, false
	}

	return labels, true
}

// Get returns the value of the label called name.
func (l Labels) Get(name string) (string, bool) {
	for _, label := range l {
		if label.Name == name {
			return label.Value, true
		}
	}

	return "", false
}
