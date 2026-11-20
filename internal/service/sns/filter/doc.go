// Package filter reads, validates and matches SNS subscription filter
// policies, after LocalStack's localstack/services/sns/filter.py at 8b9a79f.
//
// Parse validates a policy with LocalStack's messages, which AWS's
// aws.validated snapshots in tests/aws/services/sns/test_sns_filter_policy.py
// share up to the Jackson location AWS appends. MatchAttributes and MatchBody
// decide delivery for the two FilterPolicyScope values.
//
// Where LocalStack raises an exception other than InvalidParameterException,
// this package answers instead of failing:
//   - a policy that is JSON but not an object is refused with "Filter is not
//     an object", a "$or" alternative that is not an object with "Only JSON
//     object is allowed in array of $or relationship.", and a "numeric"
//     operand that is neither an array nor empty with "Value of numeric must
//     be an array.", event-ruler's messages for those shapes;
//   - a string operator tested against a value that is not a string, and a
//     String.Array attribute whose value is missing or not iterable, do not
//     match.
//
// One behavior follows AWS's numeric-value-matching page over LocalStack: an
// anything-but number refuses a Number attribute of the same value, so
// {"anything-but": [100]} refuses a Number attribute "100". The page shows
// nothing for a plain number, so {"price": [100]} compares with the text, as
// LocalStack does, and a Number attribute "100" passes it only as "100".
package filter
