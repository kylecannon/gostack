// Package awschunked decodes the aws-chunked content encoding of an AWS
// streaming upload: the body of a request signed with a STREAMING-* payload
// hash or sent with Content-Encoding aws-chunked.
//
// The body is a run of chunks, each a hex size, optional ;-separated
// extensions (chunk-signature=...), CRLF, that many bytes and CRLF. A chunk of
// size zero ends it, and trailer headers (x-amz-checksum-*,
// x-amz-trailer-signature) may follow. [Reader] streams the payload out of
// it without buffering a chunk and exposes the trailers once the payload has
// been read. Chunk signatures are not verified, as LocalStack's
// AwsChunkedDecoder does not verify them (services/s3/codec.py).
//
// See https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html.
package awschunked

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
)

const (
	// maxLine is the longest chunk header or trailer line a Reader takes. A
	// signed chunk header is about 90 bytes.
	maxLine = 4096
	// maxTrailer is the most bytes a Reader reads after the last chunk.
	maxTrailer = 16 << 10
)

var (
	// ErrMalformed is the error of a body that is not aws-chunked: a bad chunk
	// size, a chunk that does not end in CRLF, a trailer line without a colon,
	// or a body that ends before its last chunk.
	ErrMalformed = errors.New("awschunked: malformed body")
	// ErrLength is the error of a body whose payload is not the decoded length
	// the request declared in x-amz-decoded-content-length.
	ErrLength = errors.New("awschunked: payload length differs from the decoded content length")
)

type state int

const (
	stateHeader state = iota + 1
	stateData
	stateCRLF
)

// Reader decodes one aws-chunked body. It reads from the source only as far
// as the caller asks, so a large body streams through it.
type Reader struct {
	src *bufio.Reader
	// want is the declared payload length, or -1 when none was declared.
	want int64
	// got is the payload read so far, and left what remains of the chunk.
	got, left int64
	state     state
	trailer   http.Header
	// err is sticky: io.EOF once the body ended well, or why it did not.
	err error
}

// NewReader returns a Reader of the aws-chunked body src. decodedLength is
// the payload length the request declared (x-amz-decoded-content-length); a
// body whose payload differs fails with ErrLength. A negative decodedLength
// declares none.
func NewReader(src io.Reader, decodedLength int64) *Reader {
	return &Reader{src: bufio.NewReaderSize(src, maxLine), want: max(decodedLength, -1), state: stateHeader}
}

// Read reads decoded payload into p. It returns io.EOF after the last chunk
// and its trailers have been read, ErrMalformed or ErrLength (wrapped) for a
// body that is not valid, and the source's error otherwise.
func (r *Reader) Read(p []byte) (int, error) {
	for r.err == nil {
		switch r.state {
		case stateHeader:
			r.err = r.readHeader()
		case stateData:
			if len(p) == 0 {
				return 0, nil
			}

			n, err := r.readData(p)
			r.err = err

			if n > 0 {
				return n, nil
			}
		default:
			r.err = r.readCRLF()
		}
	}

	return 0, r.err
}

// Trailer returns the trailer headers that followed the last chunk, once Read
// has returned io.EOF, and nil before.
func (r *Reader) Trailer() http.Header {
	if !errors.Is(r.err, io.EOF) {
		return nil
	}

	return r.trailer
}

// readHeader reads a chunk header line. The last chunk's header is followed
// by the trailers, which it reads too.
func (r *Reader) readHeader() error {
	line, err := r.line()
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: body ends before its last chunk", ErrMalformed)
	}

	if err != nil {
		return err
	}

	sizeField, _, _ := bytes.Cut(line, []byte(";"))

	size, parseErr := strconv.ParseInt(string(bytes.Trim(sizeField, " \t")), 16, 64)
	if parseErr != nil || size < 0 {
		return fmt.Errorf("%w: chunk size %q", ErrMalformed, sizeField)
	}

	if size == 0 {
		return r.readTrailers()
	}

	if r.want >= 0 && size > r.want-r.got {
		return fmt.Errorf("%w: more than %d bytes", ErrLength, r.want)
	}

	r.left = size
	r.state = stateData

	return nil
}

func (r *Reader) readData(p []byte) (int, error) {
	n, err := r.src.Read(p[:min(int64(len(p)), r.left)])
	r.got += int64(n)
	r.left -= int64(n)

	switch {
	case err != nil && !errors.Is(err, io.EOF):
		return n, fmt.Errorf("awschunked: read chunk: %w", err)
	case r.left == 0:
		r.state = stateCRLF
	case n == 0 && errors.Is(err, io.EOF):
		return 0, fmt.Errorf("%w: body ends inside a chunk", ErrMalformed)
	default:
	}

	return n, nil
}

func (r *Reader) readCRLF() error {
	var end [2]byte

	_, err := io.ReadFull(r.src, end[:])
	if err != nil || end != [2]byte{'\r', '\n'} {
		return fmt.Errorf("%w: chunk does not end in CRLF", ErrMalformed)
	}

	r.state = stateHeader

	return nil
}

// readTrailers reads the trailer headers up to the end of the body, as
// LocalStack's decoder does, skipping blank lines, and then checks the
// payload length. It returns io.EOF when the body is good.
func (r *Reader) readTrailers() error {
	read := 0

	for {
		line, err := r.line()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return err
		}

		read += len(line)
		if read > maxTrailer {
			return fmt.Errorf("%w: trailers longer than %d bytes", ErrMalformed, maxTrailer)
		}

		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		name, value, found := bytes.Cut(line, []byte(":"))
		name = bytes.TrimSpace(name)

		if !found || len(name) == 0 {
			return fmt.Errorf("%w: trailer %q", ErrMalformed, line)
		}

		if r.trailer == nil {
			r.trailer = make(http.Header)
		}

		r.trailer.Add(textproto.CanonicalMIMEHeaderKey(string(name)), string(bytes.TrimSpace(value)))
	}

	if r.want >= 0 && r.got != r.want {
		return fmt.Errorf("%w: %d bytes, declared %d", ErrLength, r.got, r.want)
	}

	return io.EOF
}

// line returns the next line without its CRLF or LF. A last line the body
// ends without a newline still counts; io.EOF means there is no line left.
// The slice is valid until the next read.
func (r *Reader) line() ([]byte, error) {
	line, err := r.src.ReadSlice('\n')

	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return nil, fmt.Errorf("%w: line longer than %d bytes", ErrMalformed, maxLine)
	case errors.Is(err, io.EOF) && len(line) == 0:
		return nil, io.EOF
	case err != nil && !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("awschunked: read line: %w", err)
	}

	line = bytes.TrimSuffix(line, []byte("\n"))

	return bytes.TrimSuffix(line, []byte("\r")), nil
}
