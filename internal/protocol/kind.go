package protocol

// Kind names an AWS wire protocol.
type Kind int

const (
	// KindQuery is the query protocol: a form-encoded request and an xml
	// response.
	KindQuery Kind = iota + 1
	// KindJSON is the json protocol: an X-Amz-Target header and a json body.
	KindJSON
	// KindRestJSON is the rest-json protocol.
	KindRestJSON
	// KindRestXML is the rest-xml protocol.
	KindRestXML
	// KindEC2 is the ec2 query protocol.
	KindEC2
)
