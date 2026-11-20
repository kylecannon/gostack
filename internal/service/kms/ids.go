package kms

import (
	"crypto/sha1" //nolint:gosec // UUID version 5 is defined over SHA-1; nothing secret depends on it
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	// uuidShape is a UUID's text with x for each hex digit.
	uuidShape = "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
	mrkPrefix = "mrk-"
	// arnFields is the number of colon-separated fields in an ARN, the last
	// being the resource, and arnRegion the index of its region.
	arnFields    = 6
	arnRegion    = 3
	mrkHexLen    = 32
	aliasPrefix  = "alias/"
	keyResource  = "key/"
	arnPrefix    = "arn:"
	accountIDLen = 12
)

// partitions are the ARN partitions LocalStack knows (PARTITION_NAMES), and
// regionPartitions the region prefixes of the ones other than aws.
var (
	partitions       = []string{"aws", "aws-cn", "aws-eusc", "aws-iso", "aws-iso-b", "aws-us-gov"}
	regionPartitions = [...]struct{ prefix, name string }{
		{"cn-", "aws-cn"},
		{"us-gov-", "aws-us-gov"},
		{"us-iso-", "aws-iso"},
		{"us-isob-", "aws-iso-b"},
		{"eusc-", "aws-eusc"},
	}
)

// partition is the ARN partition of a region (get_partition).
func partition(region string) string {
	for _, p := range regionPartitions {
		if strings.HasPrefix(region, p.prefix) {
			return p.name
		}
	}

	return "aws"
}

// keyARN and aliasARN are kms_key_arn and kms_alias_arn: a name that holds a
// colon is taken to be an ARN already.
func keyARN(account, region, id string) string {
	if strings.Contains(id, ":") {
		return id
	}

	return "arn:" + partition(region) + ":kms:" + region + ":" + account + ":key/" + id
}

func aliasARN(account, region, name string) string {
	if !strings.HasPrefix(name, aliasPrefix) {
		name = aliasPrefix + name
	}

	if strings.Contains(name, ":") {
		return name
	}

	return "arn:" + partition(region) + ":kms:" + region + ":" + account + ":" + name
}

// parsedARN is a KMS key or alias ARN that LocalStack's KMS_KEY_ARN_PATTERN
// matches.
type parsedARN struct {
	region  string
	account string
	// keyID is the key id of a key ARN and the alias name, alias/ included,
	// of an alias ARN.
	keyID string
}

// parseKeyARN is is_valid_key_arn and parse_key_arn: a known partition, kms, a
// region, a 12-digit account, then key/ and an id or alias/ and a name, with
// no colon after the account.
func parseKeyARN(s string) (parsedARN, bool) {
	const partitionField, serviceField, accountField, resourceField = 1, 2, 4, 5

	parts := strings.SplitN(s, ":", arnFields)
	if len(parts) != arnFields || parts[0] != "arn" || parts[serviceField] != "kms" || parts[arnRegion] == "" {
		return parsedARN{}, false
	}

	resource := parts[resourceField]
	if !slices.Contains(partitions, parts[partitionField]) || !isDigits(parts[accountField], accountIDLen) ||
		strings.Contains(resource, ":") {
		return parsedARN{}, false
	}

	switch {
	case strings.HasPrefix(resource, keyResource) && len(resource) > len(keyResource):
		resource = resource[len(keyResource):]
	case strings.HasPrefix(resource, aliasPrefix):
	default:
		return parsedARN{}, false
	}

	return parsedARN{region: parts[arnRegion], account: parts[accountField], keyID: resource}, true
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}

// isKeyID reports whether id has the form of a key id: a UUID (PATTERN_UUID)
// or mrk- and 32 hex digits (MULTI_REGION_PATTERN).
func isKeyID(id string) bool {
	if rest, ok := strings.CutPrefix(id, mrkPrefix); ok {
		return len(rest) == mrkHexLen && isHex(rest)
	}

	if len(id) != len(uuidShape) {
		return false
	}

	for i := range len(id) {
		if uuidShape[i] == '-' {
			if id[i] != '-' {
				return false
			}
		} else if !isHex(id[i : i+1]) {
			return false
		}
	}

	return true
}

func isHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}

	return true
}

// newUUID is a random UUID, version 4.
func newUUID(random io.Reader) (string, error) {
	var b [16]byte

	_, err := io.ReadFull(random, b[:])
	if err != nil {
		return "", fmt.Errorf("read uuid: %w", err)
	}

	setUUIDVersion(&b, uuidRandom)

	return formatUUID(b), nil
}

// uuidRandom and uuidSHA1 are the version nibbles of UUID versions 4 and 5.
const (
	uuidRandom = 0x40
	uuidSHA1   = 0x50
)

// setUUIDVersion writes a UUID's version nibble and the RFC 4122 variant bits.
func setUUIDVersion(b *[16]byte, version byte) {
	const versionByte, variantByte, versionMask, variantMask, variant = 6, 8, 0x0f, 0x3f, 0x80

	b[versionByte] = b[versionByte]&versionMask | version
	b[variantByte] = b[variantByte]&variantMask | variant
}

// uuid5 is Python's uuid.uuid5(namespace, name).
func uuid5(namespace [16]byte, name []byte) [16]byte {
	h := sha1.New() //nolint:gosec // see the import
	h.Write(namespace[:])
	h.Write(name)

	var b [16]byte
	copy(b[:], h.Sum(nil))
	setUUIDVersion(&b, uuidSHA1)

	return b
}

func formatUUID(b [16]byte) string {
	s := hex.EncodeToString(b[:])

	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// materialID is CurrentKeyMaterialId: the hex of uuid5 over the key's
// internal id and its material, twice (generate_key_material_id).
func materialID(internal [16]byte, material []byte) string {
	b := uuid5(internal, material)
	s := hex.EncodeToString(b[:])

	return s + s
}
