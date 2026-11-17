// Package query is the runtime of the AWS query protocol: a form-encoded
// request and an XML response.
//
// Generated code in internal/api/<svc> drives it. [Decoder] reads request
// members out of [url.Values] by key, [Writer] appends response elements, and
// [WriteResponse] and [WriteError] wrap them in the envelopes LocalStack
// sends. Nothing here reads a service model, so the same calls serve every
// service.
package query
