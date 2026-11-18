// Package rest holds what the rest-xml and rest-json protocols share: the
// members a request carries outside its body and a response carries in its
// headers and status line.
//
// Generated code in internal/api/<svc> drives it. [Pattern] matches a
// requestUri against a request path and returns the URI labels, [Decoder]
// reads header, querystring and label members by name, and the Set functions
// write header members on a response. Nothing here reads a service model or
// uses reflection, so the same calls serve every service.
//
// A timestamp format is a botocore timestampFormat name. An empty format is
// the default of the member's location: rfc822 in a header, iso8601 in the
// querystring and in a URI label.
package rest
