package protocol

import (
	"bytes"
	"io"
	"sync/atomic"
)

// DefaultMaxBody is the cap on a request body that is not streamed, when
// GOSTACK_MAX_REQUEST_BODY is not set.
const DefaultMaxBody int64 = 16 << 20

var maxBody = newMaxBody()

func newMaxBody() *atomic.Int64 {
	limit := new(atomic.Int64)
	limit.Store(DefaultMaxBody)

	return limit
}

// MaxBody is how many bytes of a request body that is not streamed gostack
// reads: a form, a json body, an xml document or a blob or string payload. It
// counts decoded bytes, so a gzip bomb stops at this size. Zero means no limit,
// which is LocalStack's behavior. A streamed payload (an S3 object body) never
// has a limit.
func MaxBody() int64 {
	return maxBody.Load()
}

// SetMaxBody sets what MaxBody returns: bytes, or zero for no limit. Start-up
// calls it once from GOSTACK_MAX_REQUEST_BODY, before the gateway serves; a
// test that changes it puts the old value back.
func SetMaxBody(limit int64) {
	maxBody.Store(max(limit, 0))
}

// LimitBody returns r limited to one byte more than MaxBody, so that a caller
// can tell a body of exactly MaxBody bytes from a longer one with OverBody. It
// returns r itself when there is no limit.
func LimitBody(r io.Reader) io.Reader {
	limit := MaxBody()
	if limit == 0 {
		return r
	}

	return io.LimitReader(r, limit+1)
}

// ReadBody reads r through LimitBody into one buffer. A body that states its
// length, within MaxBody, is read into a buffer of that length and allocated
// once; io.ReadAll would grow its buffer about 25% at a time and allocate
// some five times the body on the way. A body of unknown length (-1, as a gzip
// body has) or one longer than it stated grows as io.ReadAll's does. Check the
// result with OverBody.
func ReadBody(r io.Reader, length int64) ([]byte, error) {
	limit := MaxBody()
	if length < 0 || (limit > 0 && length > limit) {
		length = 0
	}

	buf := bytes.NewBuffer(make([]byte, 0, length+bytes.MinRead))

	_, err := buf.ReadFrom(LimitBody(r))

	return buf.Bytes(), err
}

// OverBody reports whether n bytes, read through LimitBody, are more than
// MaxBody allows.
func OverBody(n int) bool {
	limit := MaxBody()

	return limit > 0 && int64(n) > limit
}
