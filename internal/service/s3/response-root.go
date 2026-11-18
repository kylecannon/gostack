package s3

import (
	"bytes"
)

// responseRoots renames the root element of an S3 XML response. S3 does not
// follow its own model on the root name for 41 of its 44 operations that have a
// body, and LocalStack's S3ResponseSerializer carries the table; the generated
// encoder writes the model's name, the output shape's.
var responseRoots = map[string]string{
	"CompleteMultipartUploadOutput":                  "CompleteMultipartUploadResult",
	"CopyObjectOutput":                               "CopyObjectResult",
	"CreateMultipartUploadOutput":                    "InitiateMultipartUploadResult",
	"DeleteObjectsOutput":                            "DeleteResult",
	"GetBucketAccelerateConfigurationOutput":         "AccelerateConfiguration",
	"GetBucketAclOutput":                             "AccessControlPolicy",
	"GetBucketAnalyticsConfigurationOutput":          "AnalyticsConfiguration",
	"GetBucketCorsOutput":                            "CORSConfiguration",
	"GetBucketEncryptionOutput":                      "ServerSideEncryptionConfiguration",
	"GetBucketIntelligentTieringConfigurationOutput": "IntelligentTieringConfiguration",
	"GetBucketInventoryConfigurationOutput":          "InventoryConfiguration",
	"GetBucketLifecycleOutput":                       "LifecycleConfiguration",
	"GetBucketLifecycleConfigurationOutput":          "LifecycleConfiguration",
	"GetBucketLoggingOutput":                         "BucketLoggingStatus",
	"GetBucketMetricsConfigurationOutput":            "MetricsConfiguration",
	"NotificationConfigurationDeprecated":            "NotificationConfiguration",
	"GetBucketOwnershipControlsOutput":               "OwnershipControls",
	"GetBucketPolicyStatusOutput":                    "PolicyStatus",
	"GetBucketReplicationOutput":                     "ReplicationConfiguration",
	"GetBucketRequestPaymentOutput":                  "RequestPaymentConfiguration",
	"GetBucketTaggingOutput":                         "Tagging",
	"GetBucketVersioningOutput":                      "VersioningConfiguration",
	"GetBucketWebsiteOutput":                         "WebsiteConfiguration",
	"GetObjectAclOutput":                             "AccessControlPolicy",
	"GetObjectLegalHoldOutput":                       "LegalHold",
	"GetObjectLockConfigurationOutput":               "ObjectLockConfiguration",
	"GetObjectRetentionOutput":                       "Retention",
	"GetObjectTaggingOutput":                         "Tagging",
	"GetObjectAttributesOutput":                      "GetObjectAttributesResponse",
	"GetPublicAccessBlockOutput":                     "PublicAccessBlockConfiguration",
	"ListBucketAnalyticsConfigurationsOutput":        "ListBucketAnalyticsConfigurationResult",
	"ListBucketInventoryConfigurationsOutput":        "ListInventoryConfigurationsResult",
	"ListBucketMetricsConfigurationsOutput":          "ListMetricsConfigurationsResult",
	"ListBucketsOutput":                              "ListAllMyBucketsResult",
	"ListMultipartUploadsOutput":                     "ListMultipartUploadsResult",
	"ListObjectsOutput":                              "ListBucketResult",
	"ListObjectsV2Output":                            "ListBucketResult",
	"ListObjectVersionsOutput":                       "ListVersionsResult",
	"ListPartsOutput":                                "ListPartsResult",
	"UploadPartCopyOutput":                           "CopyPartResult",
}

const (
	xmlDeclEnd = "?>"

	// s3Namespace is the namespace of every S3 XML response root. It is an
	// identifier, and S3 spells it http.
	s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/" //nolint:revive // see above.
)

// rootSplit is an XML body cut around its root element's name: the bytes up to
// and including the opening tag's name, the rest, and the root's closing tag.
type rootSplit struct {
	head, rest, tail []byte
	name             string
}

// splitRoot finds the root element of an XML document written as the generated
// encoder writes it: an optional declaration, the opening tag, the content and
// the closing tag at the very end. It reports false when the body is not that.
func splitRoot(body []byte) (rootSplit, bool) {
	start := 0

	if bytes.HasPrefix(body, []byte("<?xml")) {
		end := bytes.Index(body, []byte(xmlDeclEnd))
		if end < 0 {
			return rootSplit{}, false
		}

		start = end + len(xmlDeclEnd)
		for start < len(body) && (body[start] == '\n' || body[start] == '\r' || body[start] == ' ') {
			start++
		}
	}

	if start >= len(body) || body[start] != '<' {
		return rootSplit{}, false
	}

	nameEnd := start + 1
	for nameEnd < len(body) && body[nameEnd] != ' ' && body[nameEnd] != '>' && body[nameEnd] != '/' {
		nameEnd++
	}

	name := string(body[start+1 : nameEnd])
	trimmed := bytes.TrimRight(body, "\n\r ")

	closing := []byte("</" + name + ">")
	if !bytes.HasSuffix(trimmed, closing) {
		// A root with no content closes itself in its opening tag.
		return rootSplit{head: body[:start+1], rest: body[nameEnd:], name: name}, true
	}

	return rootSplit{
		head: body[:start+1], rest: body[nameEnd : len(trimmed)-len(closing)], name: name,
		tail: body[len(trimmed)-len(closing):],
	}, true
}

// renamedRoot returns body as parts to write in order, with the root element
// renamed, and whether it was. The parts alias body, so a large listing is
// not copied.
func renamedRoot(body []byte) ([][]byte, bool) {
	split, ok := splitRoot(body)
	if !ok {
		return nil, false
	}

	renamed, found := responseRoots[split.name]
	if !found {
		return nil, false
	}

	// S3's serializer puts its namespace on every root; the generated encoder
	// writes none, so a root without one gets it.
	var namespace []byte

	openTag, _, _ := bytes.Cut(split.rest, []byte(">"))
	if !bytes.Contains(openTag, []byte(" xmlns=")) {
		namespace = []byte(` xmlns="` + s3Namespace + `"`)
	}

	parts := [][]byte{split.head, []byte(renamed), namespace, split.rest}

	if split.tail != nil {
		closing := []byte("</" + renamed + ">")
		parts = append(parts, closing, split.tail[len("</"+split.name+">"):])
	}

	return parts, true
}
