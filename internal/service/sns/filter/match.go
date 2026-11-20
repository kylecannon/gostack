package filter

import (
	"encoding/binary"
	"math"
	"net/netip"
	"strconv"
	"strings"

	"github.com/kylecannon/gostack/internal/protocol"
)

// Attribute is one message attribute as Publish received it. StringValue is
// empty for a Binary attribute; the matcher reads an empty value as None, as
// LocalStack does.
type Attribute struct {
	DataType    string `json:"DataType"`
	StringValue string `json:"StringValue,omitempty"`
}

// Body is a message body decoded once for every subscription that filters on
// it. A nil *Body is a body that is not a JSON object, which no policy
// matches.
type Body struct {
	root value
}

// ParseBody decodes a message body for MatchBody. It returns nil for a body
// that is not a JSON object.
func ParseBody(body string) *Body {
	root, ok := decode(body)
	if !ok || root.kind != kindDict {
		return nil
	}
	return &Body{root: root}
}

// MatchAttributes reports whether a message with attrs passes the policy under
// the MessageAttributes scope. A String.Array attribute passes a condition
// when one of its members does.
func (p *Policy) MatchAttributes(attrs map[string]Attribute) bool {
	if p.Empty() {
		return true
	}
	for _, terms := range p.branches {
		if p.attributeBranch(terms, attrs) {
			return true
		}
	}
	return false
}

func (p *Policy) attributeBranch(terms []term, attrs map[string]Attribute) bool {
	for _, t := range terms {
		var found *Attribute
		if attr, ok := attrs[p.keys[t.key]]; ok {
			found = &attr
		}
		if !matchAttribute(t.conds, found) {
			return false
		}
	}
	return true
}

// matchAttribute is LocalStack's _evaluate_filter_policy_conditions_on_attribute
// for attr, nil when the message has no such attribute.
func matchAttribute(conds []cond, attr *Attribute) bool {
	if attr == nil {
		return anyCond(conds, nil)
	}
	if attr.DataType == "String.Array" {
		return matchArray(conds, attr.StringValue)
	}
	v := null
	if attr.StringValue != "" {
		// AWS compares a Number attribute with a number in the policy by
		// value (numeric-value-matching.html); LocalStack compares the text
		// with the number and never finds them equal.
		number := attr.DataType == "Number" || strings.HasPrefix(attr.DataType, "Number.")
		v = &value{kind: kindString, str: attr.StringValue, numberText: number}
	}
	return anyCond(conds, v)
}

// anyCond reports whether one of conds holds for v, nil when absent.
func anyCond(conds []cond, v *value) bool {
	for i := range conds {
		if conds[i].match(v) {
			return true
		}
	}
	return false
}

// matchArray tests every member of a String.Array value, read as json.loads
// reads it and iterated as Python iterates the result: a list by member, an
// object by name, a string by character. A value that is missing, not JSON,
// or a scalar matches nothing.
func matchArray(conds []cond, text string) bool {
	if text == "" {
		return false
	}
	arr, ok := decode(text)
	if !ok {
		return false
	}
	switch arr.kind {
	case kindList:
		for i := range arr.list {
			if anyCond(conds, &arr.list[i]) {
				return true
			}
		}
	case kindDict:
		for i := range arr.dict {
			if anyCond(conds, &value{kind: kindString, str: arr.dict[i].key}) {
				return true
			}
		}
	case kindString:
		for _, r := range arr.str {
			if anyCond(conds, &value{kind: kindString, str: string(r)}) {
				return true
			}
		}
	default:
	}
	return false
}

// MatchBody reports whether a message whose decoded body is b passes the
// policy under the MessageBody scope. Names nest with "." on both sides, so
// {"a.b": ...} in the policy tests {"a": {"b": ...}}, {"a.b": ...} and every
// mix; an array in the body passes when one of its members does.
func (p *Policy) MatchBody(b *Body) bool {
	if p.Empty() {
		return true
	}
	if b == nil {
		return false
	}
	seen := make([]*observed, len(p.keys))
	for _, terms := range p.branches {
		if p.bodyBranch(terms, b, seen) {
			return true
		}
	}
	return false
}

func (p *Policy) bodyBranch(terms []term, b *Body, seen []*observed) bool {
	for _, t := range terms {
		o := seen[t.key]
		if o == nil {
			o = observe(&b.root, p.keys[t.key])
			seen[t.key] = o
		}
		if !o.match(t.conds) {
			return false
		}
	}
	return true
}

// observed is what one flattened name can be in LocalStack's flatten_payload
// of a body: absent from at least one flattened payload, and every value it
// holds in one. LocalStack forms the cross product of every array on the
// policy's paths and tests each name against every product; a name's test
// needs only its own values, so this is the same answer without the product.
type observed struct {
	absent bool
	vals   []*value
}

func (o *observed) match(conds []cond) bool {
	if o.absent && anyCond(conds, nil) {
		return true
	}
	for _, v := range o.vals {
		if anyCond(conds, v) {
			return true
		}
	}
	return false
}

func observe(root *value, key string) *observed {
	w := walker{key: key}
	absent := w.node(root, 0)
	return &observed{absent: absent, vals: w.vals}
}

// walker gathers the values one flattened name can take. A node at path
// key[:c] reports whether the name can still be absent after it: false when
// every way through the node sets the name, which discards what earlier
// siblings set.
type walker struct {
	key  string
	vals []*value
}

func (w *walker) node(v *value, c int) bool {
	switch v.kind {
	case kindDict:
		return w.dict(v, c)
	case kindList:
		if len(v.list) == 0 {
			return true
		}
		absent := false
		for i := range v.list {
			if w.node(&v.list[i], c) {
				absent = true
			}
		}
		return absent
	default:
		if c == len(w.key) {
			w.vals = append(w.vals, v)
			return false
		}
		return true
	}
}

func (w *walker) dict(v *value, c int) bool {
	absent := true
	start := len(w.vals)
	for i := range v.dict {
		nc, ok := w.child(c, v.dict[i].key)
		if !ok {
			continue
		}
		mark := len(w.vals)
		if !w.node(&v.dict[i].val, nc) {
			// Every way through this member sets the name, so what the
			// members before it set is overwritten.
			w.vals = append(w.vals[:start], w.vals[mark:]...)
			absent = false
		}
	}
	return absent
}

// child returns how much of the key the path of member name under a node at
// key[:c] covers, and false when no name under that path is the key.
func (w *walker) child(c int, name string) (int, bool) {
	k := w.key
	if c == 0 {
		// An empty path joins without a dot (joinPath).
		if name == "" {
			return 0, true
		}
		if k == name || strings.HasPrefix(k, name) && k[len(name)] == '.' {
			return len(name), true
		}
		return 0, false
	}
	if c >= len(k) || k[c] != '.' || !strings.HasPrefix(k[c+1:], name) {
		return 0, false
	}
	end := c + 1 + len(name)
	if end == len(k) || k[end] == '.' {
		return end, true
	}
	return 0, false
}

// op is one of LocalStack's _evaluate_condition branches.
type op uint8

const (
	opNever op = iota + 1
	opEqual
	opExists
	opNotEqual
	opNotIn
	opNotPrefix
	opPrefix
	opSuffix
	opEqualsIgnoreCase
	opNumeric
	opCIDR
)

type cond struct {
	op      op
	operand *value
	str     string
	network netip.Prefix
}

// stringOps are the operators LocalStack tests after anything-but, in order.
var stringOps = []struct {
	name string
	op   op
}{{operatorPrefix, opPrefix}, {"suffix", opSuffix}, {"equals-ignore-case", opEqualsIgnoreCase}}

// compileCond reads a rule as LocalStack's _evaluate_condition does. Each
// operator is tested by truthiness in a fixed order, so an operator whose
// operand Python reads as false, such as {"prefix": ""}, matches nothing.
func compileCond(r *value) cond {
	if r.kind != kindDict {
		return cond{op: opEqual, operand: r}
	}
	if ex, ok := r.get("exists"); ok && ex.kind != kindNull {
		return cond{op: opExists, operand: ex}
	}
	if ab, ok := r.get("anything-but"); ok && ab.truthy() {
		return compileAnythingBut(ab)
	}
	for _, s := range stringOps {
		if v, ok := r.get(s.name); ok && v.truthy() {
			switch {
			case v.kind != kindString:
				return cond{op: opNever}
			case s.op == opEqualsIgnoreCase:
				return cond{op: s.op, str: strings.ToLower(v.str)}
			default:
				return cond{op: s.op, str: v.str}
			}
		}
	}
	if n, ok := r.get("numeric"); ok && n.truthy() {
		return cond{op: opNumeric, operand: n}
	}
	if c, ok := r.get("cidr"); ok && c.truthy() && c.kind == kindString {
		if network, ok := parseNetwork(c.str); ok {
			return cond{op: opCIDR, network: network}
		}
	}
	return cond{op: opNever}
}

func compileAnythingBut(ab *value) cond {
	switch ab.kind {
	case kindDict:
		if p, ok := ab.get(operatorPrefix); ok && p.kind == kindString {
			return cond{op: opNotPrefix, str: p.str}
		}
		return cond{op: opNever}
	case kindList:
		return cond{op: opNotIn, operand: ab}
	default:
		return cond{op: opNotEqual, operand: ab}
	}
}

// match tests v: nil when the name is absent, and a null value for Python's
// None when it is present without one.
func (c *cond) match(v *value) bool {
	switch c.op {
	case opEqual:
		if v == nil {
			return false
		}
		// AWS documents the by-value Number comparison for anything-but
		// only, so a plain number compares with the text, as LocalStack does.
		text := *v
		text.numberText = false
		return equal(&text, c.operand)
	case opExists:
		return equal(c.operand, &value{kind: kindBool, b: v != nil})
	default:
	}
	if v == nil || v.kind == kindNull {
		return false
	}
	switch c.op {
	case opNotEqual:
		return !equal(v, c.operand)
	case opNotIn:
		for i := range c.operand.list {
			if equal(v, &c.operand.list[i]) {
				return false
			}
		}
		return true
	case opNotPrefix:
		return v.kind == kindString && !strings.HasPrefix(v.str, c.str)
	case opPrefix:
		return v.kind == kindString && strings.HasPrefix(v.str, c.str)
	case opSuffix:
		return v.kind == kindString && strings.HasSuffix(v.str, c.str)
	case opEqualsIgnoreCase:
		return v.kind == kindString && strings.ToLower(v.str) == c.str
	case opNumeric:
		return matchNumeric(c.operand, v)
	case opCIDR:
		addr, ok := address(v)
		return ok && c.network.Contains(addr)
	default:
		return false
	}
}

// matchNumeric is LocalStack's _evaluate_numeric_condition: v read with
// Python's float(), then every operator and operand pair in turn. A
// malformed rule, which only a "$or" of one alternative can hold, matches
// nothing where LocalStack raises.
func matchNumeric(rule, v *value) bool {
	x, ok := pyFloat(v)
	if !ok || rule.kind != kindList {
		return false
	}
	terms := rule.list
	for i := 0; i < len(terms); i += 2 {
		if i+1 >= len(terms) {
			return false
		}
		operand, ok := pyFloat(&terms[i+1])
		if !ok {
			return false
		}
		if terms[i].kind == kindString && !compare(x, terms[i].str, operand) {
			return false
		}
	}
	return true
}

// compare applies one numeric operator; an unknown one holds, as LocalStack
// skips it. Each test is LocalStack's refusal negated, which a NaN passes.
func compare(x float64, operator string, operand float64) bool {
	switch operator {
	case "=":
		return x == operand
	case ">":
		return !(x <= operand)
	case "<":
		return !(x >= operand)
	case ">=":
		return !(x < operand)
	case "<=":
		return !(x > operand)
	default:
		return true
	}
}

// pyFloat is Python's float(v) for a decoded value. An int too large for a
// float64 is infinite here, where Python raises OverflowError.
func pyFloat(v *value) (float64, bool) {
	if v.kind == kindString {
		return protocol.ParsePyFloat(v.str)
	}
	return v.numeric()
}

// address is Python's ipaddress.ip_address(v): text, or an int below 2**32
// as IPv4 and below 2**64 as IPv6. A zone is dropped, as containment ignores
// it.
func address(v *value) (netip.Addr, bool) {
	switch v.kind {
	case kindString:
		a, err := netip.ParseAddr(v.str)
		if err != nil {
			return netip.Addr{}, false
		}
		return a.WithZone(""), true
	case kindBool:
		if v.b {
			return netip.AddrFrom4([4]byte{0, 0, 0, 1}), true
		}
		return netip.AddrFrom4([4]byte{}), true
	case kindInt:
		n, err := strconv.ParseUint(v.str, 10, 64)
		if err != nil {
			return netip.Addr{}, false
		}
		if n <= math.MaxUint32 {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(n))
			return netip.AddrFrom4(b), true
		}
		var b [16]byte
		binary.BigEndian.PutUint64(b[8:], n)
		return netip.AddrFrom16(b), true
	default:
		return netip.Addr{}, false
	}
}

// parseNetwork is Python's ipaddress.ip_network(s) with strict=True: an
// address, then optionally "/" and a prefix length in ASCII digits, with no
// address bits set past the prefix. A zone on an IPv6 address is dropped.
func parseNetwork(s string) (netip.Prefix, bool) {
	addrText, maskText, hasMask := strings.Cut(s, "/")
	addr, err := netip.ParseAddr(addrText)
	if err != nil || strings.Contains(maskText, "/") {
		return netip.Prefix{}, false
	}
	addr = addr.WithZone("")
	bits := addr.BitLen()
	if hasMask {
		n, ok := prefixLength(maskText, bits)
		if !ok {
			return netip.Prefix{}, false
		}
		bits = n
	}
	network := netip.PrefixFrom(addr, bits)
	if network.Masked().Addr() != addr {
		return netip.Prefix{}, false
	}
	return network, true
}

// prefixLength reads a non-empty run of ASCII digits, leading zeros allowed,
// whose value is at most limit.
func prefixLength(s string, limit int) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	digits := strings.TrimLeft(s, "0")
	if len(digits) > len(strconv.Itoa(limit)) {
		return 0, false
	}
	n, _ := strconv.Atoi("0" + digits)
	return n, n <= limit
}
