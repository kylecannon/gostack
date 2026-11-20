package protocol

import (
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

// endRoom is ReadBody's room past a stated length: a read into it that finds
// the end shows the body kept to its length without growing the buffer.
const endRoom = 1

// ReadBody reads r through LimitBody, in buffers sized by the bytes that
// arrive and never by the length the client states alone: the first holds
// FreeBody bytes at most, and each next one as many as have arrived so far, but
// no more than the stated length still to come (plus one byte, to see the end)
// while the body keeps to it, and never past one byte more than MaxBody. So
// what it holds while reading is at most twice what arrived: a client that
// states 16 MiB and sends one byte holds FreeBody. A body that fills more than
// the first buffer is copied once, at its end, into one of its own length, so
// a body allocates about twice its length in all, as io.ReadAll does. Check
// the result with OverBody.
func ReadBody(r io.Reader, length int64) ([]byte, error) {
	limit := MaxBody()
	if length < 0 || (limit > 0 && length > limit) {
		length = -1
	}

	first := int64(FreeBody)
	if length >= 0 {
		first = min(first, length+endRoom)
	}

	buf := make([]byte, 0, first)
	r = LimitBody(r)

	var (
		full [][]byte // the buffers filled before buf
		have int      // the bytes in full
	)

	for {
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]

		if errors.Is(err, io.EOF) {
			return joinBody(full, have, buf), nil
		}

		if err != nil {
			return joinBody(full, have, buf), err
		}

		if len(buf) < cap(buf) {
			continue
		}

		if limit > 0 && int64(have+len(buf)) > limit {
			return joinBody(full, have, buf), nil
		}

		full = append(full, buf)
		have += len(buf)
		buf = make([]byte, 0, nextSize(have, length, limit))
	}
}

// nextSize is the capacity of the buffer ReadBody reads into once have bytes
// have filled the ones before it: have again, but no more than the stated
// length still to come while the body is shorter, and never past one byte more
// than limit in all.
func nextSize(have int, length, limit int64) int {
	next := int64(have)
	if length >= 0 && int64(have) < length {
		next = min(next, length-int64(have)+endRoom)
	}

	if limit > 0 {
		next = min(next, limit+1-int64(have))
	}

	return int(next)
}

// joinBody is the body read into full, which hold have bytes, and then last:
// last itself when nothing came before it.
func joinBody(full [][]byte, have int, last []byte) []byte {
	if len(full) == 0 {
		return last
	}

	body := make([]byte, 0, have+len(last))
	for _, b := range full {
		body = append(body, b...)
	}

	return append(body, last...)
}

// OverBody reports whether n bytes, read through LimitBody, are more than
// MaxBody allows.
func OverBody(n int) bool {
	limit := MaxBody()

	return limit > 0 && int64(n) > limit
}
