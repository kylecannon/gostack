// Package restxml is the body codec of the AWS rest-xml protocol: an XML
// request body, an XML response body, and the two XML error envelopes.
//
// Generated code in internal/api/<svc> drives it, with package rest for the
// members outside the body. [Decoder] streams a request body element by
// element, [Writer] appends response elements, and [WriteResponse],
// [WriteError] and [WriteS3Error] send them. Nothing here reads a service
// model or uses reflection.
package restxml
