// Package protocol holds what every AWS wire protocol shares: the protocol
// Kind and the Time type, whose json form is epoch seconds and whose query and
// xml form is ISO 8601. The codec subpackages build on it.
package protocol
