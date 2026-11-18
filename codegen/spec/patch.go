package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Patch is one RFC 6902 operation, as LocalStack's spec-patches.json lists
// them for the jsonpatch library.
type Patch struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	From  string          `json:"from"`
	Value json.RawMessage `json:"value"`
}

// ApplyPatches applies ops to the json document doc in order and returns the
// patched document. The input is not changed. Like jsonpatch, it stops with an
// error at the first operation that cannot be applied.
func ApplyPatches(doc []byte, ops []Patch) ([]byte, error) {
	tree, err := decodeTree(doc)
	if err != nil {
		return nil, err
	}

	for i, op := range ops {
		tree, err = applyOne(tree, op)
		if err != nil {
			return nil, fmt.Errorf("patch %d (%s %s): %w", i, op.Op, op.Path, err)
		}
	}

	out, err := encodeTree(nil, tree)
	if err != nil {
		return nil, fmt.Errorf("encode patched document: %w", err)
	}

	return out, nil
}

// object is a json object that keeps its keys in order, with the semantics of
// the Python dict LocalStack's jsonpatch works on: a new key goes last, and a
// key that is set again keeps its place. The order is the order a structure's
// members go on the wire, so a patch that removes a member and adds it back
// moves it last, as S3's Contents and Versions are.
type object struct {
	keys []string
	vals map[string]any
}

func (o *object) set(key string, value any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}

	o.vals[key] = value
}

func (o *object) remove(key string) {
	delete(o.vals, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
}

// decodeTree reads json into objects, arrays and scalars, keeping the order of
// object keys and numbers as written, so a large integer in a spec comes out
// of a patch unchanged.
func decodeTree(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	v, err := decodeValue(dec)
	if err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}

	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}

	switch tok {
	case json.Delim('{'):
		return decodeObject(dec)
	case json.Delim('['):
		return decodeArray(dec)
	default:
		return tok, nil
	}
}

// decodeObject reads the members of an object after its '{', and the '}'.
func decodeObject(dec *json.Decoder) (any, error) {
	o := &object{vals: map[string]any{}}

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("read key: %w", err)
		}

		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("object key %v is not a string", tok)
		}

		v, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}

		o.set(key, v)
	}

	_, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("read object end: %w", err)
	}

	return o, nil
}

// decodeArray reads the elements of an array after its '[', and the ']'. An
// empty array stays [], never null.
func decodeArray(dec *json.Decoder) (any, error) {
	arr := []any{}

	for dec.More() {
		v, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}

		arr = append(arr, v)
	}

	_, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("read array end: %w", err)
	}

	return arr, nil
}

// encodeTree appends v to buf as json, the members of each object in order.
func encodeTree(buf []byte, v any) ([]byte, error) {
	var err error

	switch node := v.(type) {
	case *object:
		buf = append(buf, '{')

		for i, key := range node.keys {
			if i > 0 {
				buf = append(buf, ',')
			}

			buf, err = encodeTree(buf, key)
			if err != nil {
				return nil, err
			}

			buf, err = encodeTree(append(buf, ':'), node.vals[key])
			if err != nil {
				return nil, err
			}
		}

		return append(buf, '}'), nil
	case []any:
		buf = append(buf, '[')

		for i, e := range node {
			if i > 0 {
				buf = append(buf, ',')
			}

			buf, err = encodeTree(buf, e)
			if err != nil {
				return nil, err
			}
		}

		return append(buf, ']'), nil
	default:
		scalar, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode %v: %w", v, err)
		}

		return append(buf, scalar...), nil
	}
}

// plain turns the objects in v into maps, for a comparison that ignores key
// order, as Python's dict equality does.
func plain(v any) any {
	switch node := v.(type) {
	case *object:
		m := make(map[string]any, len(node.vals))
		for k, val := range node.vals {
			m[k] = plain(val)
		}

		return m
	case []any:
		out := make([]any, len(node))
		for i, val := range node {
			out[i] = plain(val)
		}

		return out
	default:
		return v
	}
}

func applyOne(tree any, op Patch) (any, error) {
	switch op.Op {
	case "add":
		value, err := decodeTree(op.Value)
		if err != nil {
			return nil, err
		}

		return add(tree, op.Path, value)
	case "remove":
		return remove(tree, op.Path)
	case "replace":
		return replace(tree, op.Path, op.Value)
	case "move":
		return move(tree, op)
	case "copy":
		return copyValue(tree, op)
	case "test":
		return tree, testValue(tree, op)
	default:
		return nil, fmt.Errorf("unknown operation %q", op.Op)
	}
}

// replace sets a value that must exist, in its place: an object member keeps
// its position among the keys.
func replace(tree any, path string, raw json.RawMessage) (any, error) {
	value, err := decodeTree(raw)
	if err != nil {
		return nil, err
	}

	tokens, err := pointer(path)
	if err != nil {
		return nil, err
	}

	if len(tokens) == 0 {
		return value, nil
	}

	return update(tree, tokens, func(parent any, last string) (any, error) {
		switch node := parent.(type) {
		case *object:
			if _, ok := node.vals[last]; !ok {
				return nil, fmt.Errorf("no member %q", last)
			}

			node.set(last, value)

			return node, nil
		case []any:
			i, err := index(last, len(node))
			if err != nil {
				return nil, err
			}

			node[i] = value

			return node, nil
		default:
			return nil, fmt.Errorf("cannot replace %q", last)
		}
	})
}

func move(tree any, op Patch) (any, error) {
	value, err := get(tree, op.From)
	if err != nil {
		return nil, err
	}

	tree, err = remove(tree, op.From)
	if err != nil {
		return nil, err
	}

	return add(tree, op.Path, value)
}

func copyValue(tree any, op Patch) (any, error) {
	value, err := get(tree, op.From)
	if err != nil {
		return nil, err
	}

	// A copy shares nothing with its source.
	raw, err := encodeTree(nil, value)
	if err != nil {
		return nil, fmt.Errorf("copy value: %w", err)
	}

	clone, err := decodeTree(raw)
	if err != nil {
		return nil, err
	}

	return add(tree, op.Path, clone)
}

func testValue(tree any, op Patch) error {
	want, err := decodeTree(op.Value)
	if err != nil {
		return err
	}

	got, err := get(tree, op.Path)
	if err != nil {
		return err
	}

	if !reflect.DeepEqual(plain(got), plain(want)) {
		return errors.New("test failed: the value differs")
	}

	return nil
}

// pointer splits a json pointer into its unescaped tokens.
func pointer(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}

	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("pointer %q does not start with /", path)
	}

	tokens := strings.Split(path[1:], "/")
	for i, t := range tokens {
		t = strings.ReplaceAll(t, "~1", "/")
		tokens[i] = strings.ReplaceAll(t, "~0", "~")
	}

	return tokens, nil
}

func get(tree any, path string) (any, error) {
	tokens, err := pointer(path)
	if err != nil {
		return nil, err
	}

	cur := tree

	for _, tok := range tokens {
		switch node := cur.(type) {
		case *object:
			next, ok := node.vals[tok]
			if !ok {
				return nil, fmt.Errorf("no member %q", tok)
			}

			cur = next
		case []any:
			i, err := index(tok, len(node))
			if err != nil {
				return nil, err
			}

			cur = node[i]
		default:
			return nil, fmt.Errorf("cannot step into %q", tok)
		}
	}

	return cur, nil
}

// index parses an array index that must name an existing element.
func index(tok string, length int) (int, error) {
	i, err := strconv.Atoi(tok)
	if err != nil || i < 0 || i >= length {
		return 0, fmt.Errorf("bad array index %q", tok)
	}

	return i, nil
}

// insertIndex parses where an add puts a new element: an existing index, or
// the end of the array, written "-" or as its length.
func insertIndex(tok string, length int) (int, error) {
	if tok == "-" {
		return length, nil
	}

	i, err := strconv.Atoi(tok)
	if err != nil || i < 0 || i > length {
		return 0, fmt.Errorf("bad array index %q", tok)
	}

	return i, nil
}

func add(tree any, path string, value any) (any, error) {
	tokens, err := pointer(path)
	if err != nil {
		return nil, err
	}

	if len(tokens) == 0 {
		return value, nil
	}

	return update(tree, tokens, func(parent any, last string) (any, error) {
		switch node := parent.(type) {
		case *object:
			node.set(last, value)

			return node, nil
		case []any:
			i, err := insertIndex(last, len(node))
			if err != nil {
				return nil, err
			}

			return slices.Insert(node, i, value), nil
		default:
			return nil, fmt.Errorf("cannot add under %q", last)
		}
	})
}

func remove(tree any, path string) (any, error) {
	tokens, err := pointer(path)
	if err != nil {
		return nil, err
	}

	if len(tokens) == 0 {
		return nil, errors.New("cannot remove the document")
	}

	return update(tree, tokens, func(parent any, last string) (any, error) {
		switch node := parent.(type) {
		case *object:
			if _, ok := node.vals[last]; !ok {
				return nil, fmt.Errorf("no member %q", last)
			}

			node.remove(last)

			return node, nil
		case []any:
			i, err := index(last, len(node))
			if err != nil {
				return nil, err
			}

			return slices.Delete(node, i, i+1), nil
		default:
			return nil, fmt.Errorf("cannot remove %q", last)
		}
	})
}

// update walks to the parent of the last token, calls fn on it and stores the
// node fn returns back where it came from, so a changed slice is kept.
func update(tree any, tokens []string, fn func(parent any, last string) (any, error)) (any, error) {
	if len(tokens) == 1 {
		return fn(tree, tokens[0])
	}

	head, rest := tokens[0], tokens[1:]

	switch node := tree.(type) {
	case *object:
		child, ok := node.vals[head]
		if !ok {
			return nil, fmt.Errorf("no member %q", head)
		}

		updated, err := update(child, rest, fn)
		if err != nil {
			return nil, err
		}

		node.vals[head] = updated

		return node, nil
	case []any:
		i, err := index(head, len(node))
		if err != nil {
			return nil, err
		}

		updated, err := update(node[i], rest, fn)
		if err != nil {
			return nil, err
		}

		node[i] = updated

		return node, nil
	default:
		return nil, fmt.Errorf("cannot step into %q", head)
	}
}

// ParsePatches reads the document spec-patches.json: patch lists keyed by
// "<service>/<api-version>/service-2".
func ParsePatches(data []byte) (map[string][]Patch, error) {
	var patches map[string][]Patch

	err := json.NewDecoder(bytes.NewReader(data)).Decode(&patches)
	if err != nil {
		return nil, fmt.Errorf("parse spec patches: %w", err)
	}

	return patches, nil
}
