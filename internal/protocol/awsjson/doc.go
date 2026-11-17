// Package awsjson is the runtime of the AWS json protocol: a json body, an
// X-Amz-Target header naming the operation, and json errors.
//
// The generated input and output types carry json tags with the wire names, so
// [Decode] and [WriteResponse] hand them to sonic as they are. The types in
// this package cover what a tag cannot say: the timestamp formats other than
// epoch seconds.
package awsjson
