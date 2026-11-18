// Package awserr defines the error value every AWS operation returns. The
// gateway serializes it to the wire: the code, message, HTTP status and fault
// side come from the fields, and QueryCode carries the awsQueryCompatible code
// for the json protocol.
package awserr
