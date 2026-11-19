package spec

import "strings"

// HTTP is the "http" trait of an operation: the method and request URI the
// rest protocols bind it to, and the status of a successful response. The
// request URI is a path template, /{Bucket}/{Key+}, optionally followed by a
// literal query, ?list-type=2. ResponseCode is 0 when the model does not give
// one.
type HTTP struct {
	Method       string `json:"method"`
	RequestURI   string `json:"requestUri"`
	ResponseCode int    `json:"responseCode"`
}

// QueryLiteral is one key of the literal query of a request URI. Value is empty
// when the URI gives the key alone, as S3's ?uploads does.
type QueryLiteral struct {
	Key   string
	Value string
}

// Path returns the request URI without its literal query.
func (h *HTTP) Path() string {
	path, _, _ := strings.Cut(h.RequestURI, "?")

	return path
}

// Query returns the literal query of the request URI in the order it is
// written: ?select&select-type=2 is {select ""} then {select-type 2}.
func (h *HTTP) Query() []QueryLiteral {
	_, query, _ := strings.Cut(h.RequestURI, "?")

	var out []QueryLiteral

	for pair := range strings.SplitSeq(query, "&") {
		if pair == "" {
			continue
		}

		key, value, _ := strings.Cut(pair, "=")
		out = append(out, QueryLiteral{Key: key, Value: value})
	}

	return out
}

// HTTPChecksum is the "httpChecksum" trait of an operation: the input members
// that name the request's checksum algorithm and ask for the response's to be
// validated, and the algorithms a response may carry.
type HTTPChecksum struct {
	RequestAlgorithmMember      string   `json:"requestAlgorithmMember"`
	RequestChecksumRequired     bool     `json:"requestChecksumRequired"`
	RequestValidationModeMember string   `json:"requestValidationModeMember"`
	ResponseAlgorithms          []string `json:"responseAlgorithms"`
}
