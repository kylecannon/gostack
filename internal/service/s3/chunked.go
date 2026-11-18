package s3

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol/awschunked"
)

const (
	headerContentSHA256 = "X-Amz-Content-Sha256"
	headerDecodedLength = "X-Amz-Decoded-Content-Length"
	headerTrailer       = "X-Amz-Trailer"

	// streamingPrefix starts the payload hash of every streaming upload:
	// STREAMING-AWS4-HMAC-SHA256-PAYLOAD[-TRAILER],
	// STREAMING-UNSIGNED-PAYLOAD-TRAILER and the ECDSA forms.
	streamingPrefix = "STREAMING-"
	// streamingSigned is the one payload hash whose request must declare its
	// decoded length (presigned_url.py:737-748).
	streamingSigned = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"

	encodingAWSChunked = "aws-chunked"
	trailerPrefix      = "x-amz-checksum-"
)

// streamingBody returns the payload of a PutObject or UploadPart body, and the
// checksum algorithm the request names. An aws-chunked body, one whose payload
// hash starts STREAMING- or whose Content-Encoding has aws-chunked
// (provider.py:841-844), is decoded as it is read, and may name its algorithm
// in x-amz-trailer instead of a header; the trailer then carries the value,
// which trailerChecksum reads once the body is consumed.
//
// A request signed with STREAMING-AWS4-HMAC-SHA256-PAYLOAD without a numeric
// x-amz-decoded-content-length is refused, as LocalStack's
// _validate_streaming_headers refuses it.
func streamingBody(h http.Header, body io.Reader, algorithm string) (io.Reader, string, error) {
	payloadHash := h.Get(headerContentSHA256)
	chunked := strings.HasPrefix(payloadHash, streamingPrefix) ||
		strings.Contains(h.Get("Content-Encoding"), encodingAWSChunked)
	if !chunked {
		return body, algorithm, nil
	}

	decodedLength := int64(-1)

	if declared := h.Get(headerDecodedLength); declared != "" {
		n, err := strconv.ParseInt(declared, 10, 64)
		if err != nil || n < 0 {
			return nil, "", s3api.NewSignatureDoesNotMatch(`Wrong "X-Amz-Decoded-Content-Length" header`)
		}

		decodedLength = n
	} else if payloadHash == streamingSigned {
		return nil, "", s3api.NewSignatureDoesNotMatch(`"X-Amz-Decoded-Content-Length" header is missing`)
	}

	if algorithm == "" {
		named, err := trailerAlgorithm(h.Get(headerTrailer))
		if err != nil {
			return nil, "", err
		}

		algorithm = named
	}

	if body == nil {
		body = http.NoBody
	}

	return awschunked.NewReader(body, decodedLength), algorithm, nil
}

// bodyReadError is what a failed read of a PutObject or UploadPart body answers.
// A body that is not valid aws-chunked, or whose payload is not the decoded
// length it declared, is the client's, so it answers 400 where an SDK does not
// retry: IncompleteBody, S3's code for a body that is not the length sent, and
// InvalidRequest for a chunk that cannot be parsed. LocalStack has no mapping
// for either; its decoder's EOFError and ValueError answer 500. Any other
// error is the server's.
func bodyReadError(err error) error {
	switch {
	case errors.Is(err, awschunked.ErrLength):
		return configError("IncompleteBody",
			"You did not provide the number of bytes specified by the x-amz-decoded-content-length HTTP header.",
			http.StatusBadRequest)
	case errors.Is(err, awschunked.ErrMalformed):
		return s3api.NewInvalidRequest("The request body is not valid aws-chunked content.")
	default:
		return fmt.Errorf("read object body: %w", err)
	}
}

// trailerAlgorithm returns the checksum algorithm an x-amz-trailer header
// names. Names are matched whole, so x-amz-checksum-crc32c is CRC32C alone,
// where LocalStack's substring match (utils.py:207-212) also finds CRC32 in
// it and refuses the request.
func trailerAlgorithm(trailer string) (string, error) {
	var algorithm string

	for name := range strings.SplitSeq(trailer, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if !strings.HasPrefix(name, trailerPrefix) {
			continue
		}

		named := strings.ToUpper(strings.TrimPrefix(name, trailerPrefix))
		if !knownAlgorithm(named) {
			return "", s3api.NewInvalidRequest("The value specified in the x-amz-trailer header is not supported")
		}

		if algorithm != "" && algorithm != named {
			return "", s3api.NewInvalidRequest(
				"Expecting a single x-amz-checksum- header. Multiple checksum Types are not allowed.")
		}

		algorithm = named
	}

	return algorithm, nil
}

// trailerChecksum returns the checksum of algorithm that the trailer of an
// aws-chunked body carried, once the body has been read to its end; "" when
// body is not aws-chunked or its trailer has none. A value that is not a
// checksum of algorithm's size is refused as LocalStack refuses it, after the
// body was written (provider.py:875-879).
func trailerChecksum(body io.Reader, algorithm string) (string, error) {
	chunked, ok := body.(*awschunked.Reader)
	if !ok {
		return "", nil
	}

	value := chunked.Trailer().Get(trailerPrefix + strings.ToLower(algorithm))
	if value != "" && !validChecksum(algorithm, value) {
		return "", invalidChecksumValue(algorithm)
	}

	return value, nil
}

// withoutAWSChunked is the Content-Encoding S3 stores: the one sent, without
// aws-chunked (provider.py:855-860). It is nil when nothing else is left.
func withoutAWSChunked(encoding *string) *string {
	if encoding == nil || !strings.Contains(*encoding, encodingAWSChunked) {
		return encoding
	}

	var kept []string

	for coding := range strings.SplitSeq(*encoding, ",") {
		if coding = strings.TrimSpace(coding); coding != encodingAWSChunked && coding != "" {
			kept = append(kept, coding)
		}
	}

	if len(kept) == 0 {
		return nil
	}

	return new(strings.Join(kept, ","))
}
