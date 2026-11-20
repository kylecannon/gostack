// Package value is DynamoDB's attribute value model over the generated
// api.AttributeValue: the ten types and the check that a value sets exactly
// one, number parsing and canonical form, set rules, the order and equality of
// key values, and item size as DynamoDB counts it.
//
// A value that passed Validate holds its numbers in canonical form, so two
// equal numbers are the same string and an encoded key identifies one item.
// Stored items are never changed in place: a write replaces the whole item, so
// a reader may keep the map it was given.
package value
