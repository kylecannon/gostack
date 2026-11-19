// Package constraint builds the message AWS json and query services answer a
// request with when it breaks the constraints of the service model: "1
// validation error detected: Value 'x' at 'member' failed to satisfy
// constraint: Member must ...". Generated code runs the checks; this package
// holds the one copy of the message form that six generated packages share.
package constraint

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Path is where a value sits in a request, spelled as AWS spells it: the
// member names with their first letter in lower case, joined by dots, with
// "<n>.member" after a list member and "<key>.member" after a map member. A
// nil *Path is the request itself. A path is rendered only when a check fails,
// so a valid request builds no string.
type Path struct {
	parent *Path
	member string
	// elem is the list position or map key after member, if any.
	elem   string
	isElem bool
}

// Field is the path of the structure in member.
func (p *Path) Field(member string) Path {
	return Path{parent: p, member: member}
}

// Item is the path of the element at index i of the list in member.
func (p *Path) Item(member string, i int) Path {
	return Path{parent: p, member: member, elem: strconv.Itoa(i + 1), isElem: true}
}

// Entry is the path of the value at key of the map in member.
func (p *Path) Entry(member, key string) Path {
	return Path{parent: p, member: member, elem: key, isElem: true}
}

func (p *Path) appendTo(b *strings.Builder) {
	if p == nil {
		return
	}

	p.parent.appendTo(b)
	b.WriteString(p.member)

	if p.isElem {
		b.WriteByte('.')
		b.WriteString(p.elem)
		b.WriteString(".member")
	}

	b.WriteByte('.')
}

// at renders the path of member below p.
func (p *Path) at(member string) string {
	var b strings.Builder

	p.appendTo(&b)
	b.WriteString(member)

	return b.String()
}

// Report collects the failures of one request in the order they are found.
type Report struct {
	failures []string
}

// Null records a required member the request does not set.
func (r *Report) Null(at *Path, member string) {
	r.failures = append(r.failures,
		"Value null at '"+at.at(member)+"' failed to satisfy constraint: Member must not be null")
}

// Value records a member whose value, shown as text, breaks rule.
func (r *Report) Value(at *Path, member, text, rule string) {
	r.failures = append(r.failures, "Value '"+text+"' at '"+at.at(member)+"' failed to satisfy constraint: "+rule)
}

// Hidden records a member that breaks rule without showing its value, as AWS
// does for a sensitive one.
func (r *Report) Hidden(at *Path, member, rule string) {
	r.failures = append(r.failures, "Value at '"+at.at(member)+"' failed to satisfy constraint: "+rule)
}

// Failed reports whether any check failed.
func (r *Report) Failed() bool {
	return len(r.failures) > 0
}

// Message is the text of the error: the count, then every failure, joined by
// "; ".
func (r *Report) Message() string {
	noun := "errors"
	if len(r.failures) == 1 {
		noun = "error"
	}

	return strconv.Itoa(len(r.failures)) + " validation " + noun + " detected: " + strings.Join(r.failures, "; ")
}

// Length is the length a string constraint measures: characters (Unicode
// code points), as LocalStack's own checks count them with Python's len, so
// a character outside the Basic Multilingual Plane counts one. No snapshot
// pins AWS's count.
func Length(s string) int {
	return utf8.RuneCountInString(s)
}

// Number is the text of a number in a message.
func Number[T int32 | int64 | float64](v T) string {
	return fmt.Sprint(v)
}

// Items is the text of a list of scalars in a message: "[a, b]".
func Items[T any](s []T) string {
	parts := make([]string, len(s))
	for i, e := range s {
		parts[i] = fmt.Sprint(e)
	}

	return "[" + strings.Join(parts, ", ") + "]"
}

// Keys returns the keys of m in order, so the failures of a map's values are
// reported in the same order on every run.
func Keys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// Pattern is a regular expression compiled the first time it is used, so a
// package with many of them costs nothing at start-up.
type Pattern struct {
	expr string
	once sync.Once
	re   *regexp.Regexp
}

// NewPattern returns a Pattern for expr, which must compile.
func NewPattern(expr string) *Pattern {
	return &Pattern{expr: expr}
}

// MatchString reports whether s matches the expression.
func (p *Pattern) MatchString(s string) bool {
	p.once.Do(func() { p.re = regexp.MustCompile(p.expr) })

	return p.re.MatchString(s)
}
