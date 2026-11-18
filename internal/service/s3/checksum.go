package s3

import (
	"crypto/sha1" //nolint:gosec // S3's SHA1 checksum is an integrity check the client picks, not a security use.
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"hash/crc32"
	"hash/crc64"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

// The checksum algorithms S3 accepts, by their names in the API, and the type
// of a checksum over a whole object.
const (
	algorithmCRC32     = "CRC32"
	algorithmCRC32C    = "CRC32C"
	algorithmCRC64NVME = "CRC64NVME"
	algorithmSHA1      = "SHA1"
	algorithmSHA256    = "SHA256"

	checksumFullObject = "FULL_OBJECT"

	// crc64NVMEPolynomial is the CRC-64/NVME polynomial 0xad93d23594c93659 in
	// the bit-reversed form hash/crc64 takes. crc64 starts from and ends with
	// all ones, as CRC-64/NVME does.
	crc64NVMEPolynomial = 0x9a6c9329ac4bc9b5
)

var (
	crc32cTable    = crc32.MakeTable(crc32.Castagnoli)
	crc64NVMETable = crc64.MakeTable(crc64NVMEPolynomial)
)

// newChecksum returns the hash of algorithm. An object stored without one gets
// CRC64NVME, LocalStack's S3Object default.
func newChecksum(algorithm string) hash.Hash {
	switch algorithm {
	case algorithmCRC32:
		return crc32.NewIEEE()
	case algorithmCRC32C:
		return crc32.New(crc32cTable)
	case algorithmSHA1:
		return sha1.New() //nolint:gosec // see the import.
	case algorithmSHA256:
		return sha256.New()
	default:
		return crc64.New(crc64NVMETable)
	}
}

// checksumSize is the length in bytes of a checksum of algorithm, which
// validate_checksum_value holds a client's value to.
func checksumSize(algorithm string) int {
	switch algorithm {
	case algorithmCRC32, algorithmCRC32C:
		return crc32.Size
	case algorithmSHA1:
		return sha1.Size
	case algorithmSHA256:
		return sha256.Size
	default:
		return crc64.Size
	}
}

// encodeChecksum is the wire form of a checksum: base64 of its big-endian bytes.
func encodeChecksum(h hash.Hash) string {
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// validChecksum reports whether value decodes to a checksum of algorithm's size.
func validChecksum(algorithm, value string) bool {
	raw, err := base64.StdEncoding.DecodeString(value)

	return err == nil && len(raw) == checksumSize(algorithm)
}

// requestChecksum returns the one x-amz-checksum-* value a PutObject carries,
// and its algorithm, as get_s3_checksum_algorithm_from_request finds it. It is
// empty when there is none.
func requestChecksum(in *s3api.PutObjectInput) (string, string, error) {
	var algorithm, value string

	found := 0

	for _, candidate := range []struct {
		algorithm string
		value     *string
	}{
		{algorithmSHA1, in.ChecksumSHA1},
		{algorithmSHA256, in.ChecksumSHA256},
		{algorithmCRC32, in.ChecksumCRC32},
		{algorithmCRC32C, in.ChecksumCRC32C},
		{algorithmCRC64NVME, in.ChecksumCRC64NVME},
	} {
		if candidate.value != nil && *candidate.value != "" {
			algorithm, value = candidate.algorithm, *candidate.value
			found++
		}
	}

	if found > 1 {
		return "", "", s3api.NewInvalidRequest(
			"Expecting a single x-amz-checksum- header. Multiple checksum Types are not allowed.")
	}

	return algorithm, value, nil
}

// checksumFields holds a checksum in the member of its algorithm, as every
// output with checksums spells them.
type checksumFields struct {
	crc32, crc32c, crc64nvme, sha1, sha256, kind *string
}

// checksumsOf returns the checksum of v in the member of its algorithm.
func checksumsOf(v *Version) checksumFields {
	var fields checksumFields

	if v.ChecksumAlgorithm == "" {
		return fields
	}

	value, kind := v.ChecksumValue, v.ChecksumType
	fields.kind = &kind

	switch v.ChecksumAlgorithm {
	case algorithmCRC32:
		fields.crc32 = &value
	case algorithmCRC32C:
		fields.crc32c = &value
	case algorithmSHA1:
		fields.sha1 = &value
	case algorithmSHA256:
		fields.sha256 = &value
	default:
		fields.crc64nvme = &value
	}

	return fields
}
