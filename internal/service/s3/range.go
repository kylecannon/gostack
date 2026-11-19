package s3

import (
	"io"
	"strconv"
	"strings"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

const rangeUnsatisfiable = "The requested range is not satisfiable"

// byteRange is the slice of an object a ranged GET or HEAD answers.
type byteRange struct {
	begin, end int64
	// contentRange is the Content-Range header: "bytes begin-end/size".
	contentRange string
}

func (r *byteRange) length() int64 {
	return r.end - r.begin + 1
}

// parseRange reads a Range header against an object of size bytes
// (parse_range_header). It returns nil when S3 answers the whole object: a
// header it cannot read, several ranges, or one that works out empty. A range
// that starts past the end of the object, and "bytes=-0", are 416.
func parseRange(header string, size int64) (*byteRange, error) {
	last := size - 1

	_, spec, ok := strings.Cut(header, "=")
	if !ok || strings.Contains(spec, "=") || strings.Contains(spec, ",") {
		return nil, nil //nolint:nilnil // nil is the answer: serve the whole object.
	}

	first, second, ok := strings.Cut(spec, "-")
	if !ok || strings.Contains(second, "-") {
		return nil, nil //nolint:nilnil // see above.
	}

	begin, hasBegin, okBegin := rangeBound(first)

	end, hasEnd, okEnd := rangeBound(second)
	if !okBegin || !okEnd {
		return nil, nil //nolint:nilnil // see above.
	}

	if (!hasBegin && hasEnd && end == 0) || (hasBegin && begin > last) {
		return nil, (&s3api.InvalidRange{ActualObjectSize: &size, RangeRequested: &header}).Err(rangeUnsatisfiable)
	}

	switch {
	case hasBegin:
		if !hasEnd {
			end = last
		} else {
			end = min(end, last)
		}
	case hasEnd:
		begin = size - min(end, size)
		end = last
	default:
		return nil, nil //nolint:nilnil // see above.
	}

	if begin > min(end, last) {
		return nil, nil //nolint:nilnil // see above.
	}

	return newByteRange(begin, end, size), nil
}

// rangeBound reads one side of a range: its number, whether it has one, and
// whether it is a number at all.
func rangeBound(s string) (int64, bool, bool) {
	if s == "" {
		return 0, false, true
	}

	n, err := strconv.ParseInt(s, 10, 64)

	return n, err == nil, err == nil
}

func newByteRange(begin, end, size int64) *byteRange {
	return &byteRange{
		begin: begin, end: end,
		contentRange: "bytes " + strconv.FormatInt(begin, 10) + "-" + strconv.FormatInt(end, 10) + "/" +
			strconv.FormatInt(size, 10),
	}
}

// partNumberRange is the range of a GET or HEAD with partNumber (get_part_range):
// the part's place in the body. An object written by one PutObject has one
// part, the whole object. Any part the object does not have is InvalidPartNumber.
func partNumberRange(v *Version, number int32) (*byteRange, error) {
	if number < 1 || number > maxPartNumber {
		n := strconv.Itoa(int(number))

		return nil, invalidArgument("Part number must be an integer between 1 and 10000, inclusive", "partNumber", &n)
	}

	part, ok := v.partRange(number)
	if !ok {
		count := max(int32(1), int32(len(v.Parts))) //nolint:gosec // a multipart object has at most 10000 parts.

		return nil, (&s3api.InvalidPartNumber{PartNumberRequested: &number, ActualPartCount: &count}).Err(
			"The requested partnumber is not satisfiable")
	}

	if part.Size == 0 {
		return nil, nil //nolint:nilnil // an empty part has no range to answer with.
	}

	return newByteRange(part.Offset, part.Offset+part.Size-1, v.Size), nil
}

// requestedRange is the range a GET or HEAD asks for with Range or partNumber,
// or nil for the whole object. Both at once are refused.
func requestedRange(v *Version, header *string, part *int32) (*byteRange, error) {
	switch {
	case deref(header) != "" && part != nil:
		return nil, s3api.NewInvalidRequest("Cannot specify both Range header and partNumber query parameter")
	case deref(header) != "":
		return parseRange(*header, v.Size)
	case part != nil:
		return partNumberRange(v, *part)
	}

	return nil, nil //nolint:nilnil // no range was asked for.
}

// rangeBody reads a slice of a body and closes the body.
type rangeBody struct {
	io.Reader
	io.Closer
}

// openRange positions body at the range and limits it to the range's length.
func openRange(body io.ReadSeekCloser, r *byteRange) (io.ReadCloser, error) {
	_, err := body.Seek(r.begin, io.SeekStart)
	if err != nil {
		_ = body.Close()

		return nil, err
	}

	return rangeBody{Reader: io.LimitReader(body, r.length()), Closer: body}, nil
}
