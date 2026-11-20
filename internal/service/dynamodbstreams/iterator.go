package dynamodbstreams

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodbstreams"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
)

// iteratorLifetime is how long a shard iterator stays usable: 15 minutes, as
// DynamoDB Streams documents.
const iteratorLifetime = 15 * time.Minute

// iteratorMark sits between the stream ARN and the token, as LocalStack writes
// it (dynamodbstreams/provider.py, get_shard_iterator).
const iteratorMark = "|1|"

// iterator is a shard iterator. It is stateless: the position, the time it was
// issued and the shard it reads are all in the token, so the service keeps
// nothing per iterator and an iterator outlives nothing it refers to.
type iterator struct {
	arn    string
	pos    int
	issued time.Time
	// shard is the shard's creation time, which tells it from a later shard
	// of a stream with the same ARN.
	shard time.Time
}

// tokenLen is the token's bytes: the position, the issue time in
// milliseconds and the shard's creation time in nanoseconds.
const tokenLen = 3 * 8

// String is the iterator as clients see it: the stream ARN, "|1|", then the
// token in base64.
func (it iterator) String() string {
	var token [tokenLen]byte

	binary.BigEndian.PutUint64(token[0:], uint64(it.pos)) //nolint:gosec // a position is never negative
	binary.BigEndian.PutUint64(token[8:], uint64(it.issued.UnixMilli()))
	binary.BigEndian.PutUint64(token[16:], uint64(it.shard.UnixNano()))

	return it.arn + iteratorMark + base64.StdEncoding.EncodeToString(token[:])
}

// parseIterator reads an iterator String wrote. It is false for anything
// else.
func parseIterator(s string) (iterator, bool) {
	cut := strings.LastIndex(s, iteratorMark)
	if cut <= 0 {
		return iterator{}, false
	}

	token, err := base64.StdEncoding.DecodeString(s[cut+len(iteratorMark):])
	if err != nil || len(token) != tokenLen {
		return iterator{}, false
	}

	pos := binary.BigEndian.Uint64(token[0:])
	issued := binary.BigEndian.Uint64(token[8:])
	shard := binary.BigEndian.Uint64(token[16:])

	if pos > 1<<62 || issued > 1<<62 || shard > 1<<62 {
		return iterator{}, false
	}

	return iterator{
		arn:    s[:cut],
		pos:    int(pos),
		issued: time.UnixMilli(int64(issued)),
		shard:  time.Unix(0, int64(shard)),
	}, true
}

// shardID is the id of the shard created at created, in the form LocalStack
// gives clients (dynamodbstreams_api.py, shard_id): "shardId-", the creation
// time in seconds with its last five digits zeroed and three zeros after,
// padded to 20 digits, then "-" and the Kinesis shard's 12-digit number, which
// is 0 for a stream's one shard.
func shardID(created time.Time) string {
	const zeroed, after = 100000, 1000

	return fmt.Sprintf("shardId-%020d-000000000000", created.Unix()/zeroed*zeroed*after)
}

// positionError is the error for a position the shard refuses. seq is the
// sequence number it came from, "" when it came from an iterator.
func positionError(err error, seq string, info dynamodb.StreamInfo) error {
	switch {
	case errors.Is(err, stream.ErrTrimmed):
		return api.NewTrimmedDataAccessException(
			"The operation attempted to read past the oldest stream record in a shard.")
	case seq != "":
		return invalid("Invalid SequenceNumber " + seq + " for shard " + shardID(info.Shard.Created()))
	default:
		return invalid("Invalid ShardIterator")
	}
}
