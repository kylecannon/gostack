// Package value is DynamoDB's attribute value model over the generated
// api.AttributeValue: the ten types and the check that a value sets exactly
// one, number parsing and canonical form, set rules, the order and equality of
// key values, and item size as DynamoDB counts it.
//
// A value that passed Validate holds its numbers in canonical form, so two
// equal numbers are the same string and an encoded key identifies one item.
// Stored items are never changed in place: a write replaces the whole item,
// which a table holds as a Packed, a string no one changes, and a reader
// decodes it with a Decoder. What it may keep of what it decodes depends on
// the call:
//
//   - Item returns a map of its own, which the caller keeps.
//   - View returns the Decoder's map, which the next View or ViewSelect on the
//     Decoder, or its Release, rewrites. The values stay valid after that.
//   - Peek returns a map of its own, but its strings are rewritten by the next
//     Peek: the caller reads them and keeps none, not even in a copy that
//     shares them.
//   - ViewSelect decodes into View's map. A value the selection names whole
//     stays valid; a map it keeps part of is reused by the next call.
package value
