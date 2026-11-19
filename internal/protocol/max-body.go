package protocol

import (
	"bytes"
	"errors"
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
// returns r itself when there is no limit. It arms r when r is a BodyMeter, as
// the caller buffers what it reads.
func LimitBody(r io.Reader) io.Reader {
	ArmBody(r)

	limit := MaxBody()
	if limit == 0 {
		return r
	}

	return io.LimitReader(r, limit+1)
}

// ReadBody reads r through LimitBody into one buffer, sized by the bytes that
// arrive and never by the length the client states alone: the first buffer
// holds FreeBody bytes at most, and each next one doubles, but no further than
// the stated length (plus room to see the end) while the body keeps to it, and
// never past one byte more than MaxBody. A body allocates about twice its
// length in all; io.ReadAll would grow 25% at a time and allocate some five
// times. A client that states 16 MiB and sends one byte holds FreeBody. Check
// the result with OverBody.
func ReadBody(r io.Reader, length int64) ([]byte, error) {
	limit := MaxBody()
	if length < 0 || (limit > 0 && length > limit) {
		length = -1
	}

	first := int64(FreeBody)
	if length >= 0 {
		first = min(first, length+bytes.MinRead)
	}

	buf := make([]byte, 0, first)
	r = LimitBody(r)

	for {
		if len(buf) == cap(buf) {
			if limit > 0 && int64(len(buf)) > limit {
				return buf, nil
			}

			buf = append(make([]byte, 0, nextSize(len(buf), length, limit)), buf...)
		}

		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]

		if errors.Is(err, io.EOF) {
			return buf, nil
		}

		if err != nil {
			return buf, err
		}
	}
}

// nextSize is the capacity ReadBody grows a full buffer of have bytes to:
// twice have, but no further than the stated length while the body is shorter,
// and never past one byte more than limit.
func nextSize(have int, length, limit int64) int {
	const growth = 2

	next := growth * int64(have)
	if length >= 0 && int64(have) < length {
		next = min(next, length+bytes.MinRead)
	}

	if limit > 0 {
		next = min(next, limit+1)
	}

	return int(next)
}

// OverBody reports whether n bytes, read through LimitBody, are more than
// MaxBody allows.
func OverBody(n int) bool {
	limit := MaxBody()

	return limit > 0 && int64(n) > limit
}
