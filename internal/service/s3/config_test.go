package s3_test

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cfgOwner = "75aa57f09aa0c8caeab4f8c24e99d10f8e7faeebf76c078efc7c6caea54ba06a"

	cfgInvalidTag      = "InvalidTag"
	cfgMalformedXML    = "MalformedXML"
	cfgInvalidArgument = "InvalidArgument"
	cfgMalformedACL    = "MalformedACLError"
	cfgInvalidRequest  = "InvalidRequest"
	cfgNoSuchConfig    = "NoSuchConfiguration"

	cfgHeaderACL    = "x-amz-acl"
	cfgHeaderGrant  = "x-amz-grant-write"
	cfgHeaderOwner  = "x-amz-expected-bucket-owner"
	cfgHeaderTagged = "x-amz-tagging"
	cfgHeaderExpiry = "X-Amz-Expiration"
	cfgHeaderMinSz  = "X-Amz-Transition-Default-Minimum-Object-Size"

	cfgPublicRead = "public-read"
	cfgFakeACL    = "fake-acl"
	cfgObjectPath = bucketAPath + "/obj"
	cfgLogGroup   = "http://acs.amazonaws.com/groups/s3/LogDelivery"
)

// cfgDoc is an S3 request document: root wraps inner in the S3 namespace.
func cfgDoc(root, inner string) string {
	return "<" + root + ` xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` + inner + "</" + root + ">"
}

func cfgPut(s *server, target, body string, header map[string]string) reply {
	return s.do(call{
		method: http.MethodPut, target: target, region: regionUSEast1, body: []byte(body), header: header,
	})
}

func cfgGet(s *server, target string) reply {
	return s.do(call{method: http.MethodGet, target: target, region: regionUSEast1})
}

func cfgDelete(s *server, target string) reply {
	return s.do(call{method: http.MethodDelete, target: target, region: regionUSEast1})
}

func cfgHead(s *server, target string) reply {
	return s.do(call{method: http.MethodHead, target: target, region: regionUSEast1})
}

// cfgBucket starts a server with bucket-a created.
func cfgBucket(t *testing.T) *server {
	t.Helper()

	s := newServer(t)
	got := cfgPut(s, bucketAPath, "", nil)
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	return s
}

func cfgExpect(t *testing.T, got reply, status int, code string) {
	t.Helper()

	require.Equal(t, status, got.status, string(got.body))

	if code != "" {
		assert.Equal(t, code, got.err(t).Code, string(got.body))
	}
}

// cfgTag is one tag document.
func cfgTag(key, value string) string {
	return "<Tag><Key>" + key + "</Key><Value>" + value + "</Value></Tag>"
}

// cfgTags is n tags with distinct keys.
func cfgTags(n int) string {
	var b strings.Builder

	for i := range n {
		b.WriteString(cfgTag(fmt.Sprintf("key%d", i), "v"))
	}

	return b.String()
}

func cfgTagging(inner string) string {
	return cfgDoc("Tagging", "<TagSet>"+inner+"</TagSet>")
}

type cfgTagSet struct {
	Tags []cfgTagEntry `xml:"TagSet>Tag"`
}

type cfgTagEntry struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

func TestBucketTaggingKeepsEmptyValuesAndOrder(t *testing.T) {
	s := cfgBucket(t)

	cfgExpect(t, cfgGet(s, bucketAPath+"?tagging"), http.StatusNotFound, "NoSuchTagSet")
	cfgExpect(t, cfgPut(s, bucketAPath+"?tagging", cfgTagging(cfgTag("tag2", "")+cfgTag("tag1", "v1")), nil),
		http.StatusNoContent, "")

	got := cfgGet(s, bucketAPath+"?tagging")
	cfgExpect(t, got, http.StatusOK, "")

	var tagging cfgTagSet

	require.NoError(t, xml.Unmarshal(got.body, &tagging), string(got.body))
	require.Len(t, tagging.Tags, 2)
	assert.Equal(t, cfgTagEntry{Key: "tag2"}, tagging.Tags[0], "the empty value is kept, in the order given")
	assert.Equal(t, cfgTagEntry{Key: "tag1", Value: "v1"}, tagging.Tags[1])

	// An empty tag set deletes the tags.
	cfgExpect(t, cfgPut(s, bucketAPath+"?tagging", cfgTagging(""), nil), http.StatusNoContent, "")
	cfgExpect(t, cfgGet(s, bucketAPath+"?tagging"), http.StatusNotFound, "NoSuchTagSet")
}

func TestBucketTaggingValidation(t *testing.T) {
	s := cfgBucket(t)

	for name, tc := range map[string]struct{ tags, code string }{
		"duplicate key":  {cfgTag("k", "1") + cfgTag("k", "2"), cfgInvalidTag},
		"aws prefix":     {cfgTag("aws:k", "1"), cfgInvalidTag},
		"bad key":        {cfgTag("a,b", "1"), cfgInvalidTag},
		"bad value":      {cfgTag("a", "1,2"), cfgInvalidTag},
		"missing key":    {"<Tag><Value>1</Value></Tag>", cfgMalformedXML},
		"missing value":  {"<Tag><Key>a</Key></Tag>", cfgMalformedXML},
		"key too long":   {cfgTag(strings.Repeat("k", 129), "1"), cfgInvalidTag},
		"value too long": {cfgTag("a", strings.Repeat("v", 257)), cfgInvalidTag},
		"too many":       {cfgTags(51), cfgInvalidTag},
	} {
		t.Run(name, func(t *testing.T) {
			cfgExpect(t, cfgPut(s, bucketAPath+"?tagging", cfgTagging(tc.tags), nil), http.StatusBadRequest, tc.code)
		})
	}
}

// TestTagCharactersFollowPythonWhitespace: LocalStack's TAG_REGEX is
// ^[\w\s.:/=+\-@]*$ over a str, whose \s is every Unicode space: a no-break
// space is a character a tag may hold, a zero-width space is not.
func TestTagCharactersFollowPythonWhitespace(t *testing.T) {
	s := cfgBucket(t)

	for name, tc := range map[string]struct {
		char rune
		want int
	}{
		"no-break space":    {0xa0, http.StatusNoContent},
		"em space":          {0x2003, http.StatusNoContent},
		"ideographic space": {0x3000, http.StatusNoContent},
		"line separator":    {0x2028, http.StatusNoContent},
		"next line":         {0x85, http.StatusNoContent},
		"zero-width space":  {0x200b, http.StatusBadRequest},
		"comma":             {',', http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			char := string(tc.char)
			got := cfgPut(s, bucketAPath+"?tagging", cfgTagging(cfgTag("k"+char+"x", "a"+char+"b")), nil)
			require.Equal(t, tc.want, got.status, string(got.body))
		})
	}
}

// TestXMLBodyIsCappedAtMaxBody: the generated reader of an XML payload stops at
// rest.MaxBody (16 MiB), as the readers of json bodies do. Decoded whole, this
// body would reach the 50-tag limit and answer InvalidTag.
func TestXMLBodyIsCappedAtMaxBody(t *testing.T) {
	s := cfgBucket(t)

	tag := cfgTag("k", "v")
	over := cfgTagging(strings.Repeat(tag, rest.MaxBody/len(tag)+1000))
	require.Greater(t, len(over), rest.MaxBody)

	cfgExpect(t, cfgPut(s, bucketAPath+"?tagging", over, nil), http.StatusBadRequest, cfgMalformedXML)

	// A document that fits is decoded as before.
	cfgExpect(t, cfgPut(s, bucketAPath+"?tagging", cfgTagging(cfgTags(51)), nil), http.StatusBadRequest, cfgInvalidTag)
}

func TestCreateBucketWithTagsAndACL(t *testing.T) {
	s := newServer(t)

	body := cfgDoc("CreateBucketConfiguration", "<Tags>"+cfgTag("team", "a")+"</Tags>")
	cfgExpect(t, cfgPut(s, "/tagged", body, map[string]string{cfgHeaderACL: cfgPublicRead}), http.StatusOK, "")

	got := cfgGet(s, "/tagged?tagging")
	cfgExpect(t, got, http.StatusOK, "")
	assert.Contains(t, string(got.body), "<Key>team</Key>")

	got = cfgGet(s, "/tagged?acl")
	cfgExpect(t, got, http.StatusOK, "")
	assert.Contains(t, string(got.body), "http://acs.amazonaws.com/groups/global/AllUsers")

	// Creating it again with tags is a conflict; without them it succeeds in us-east-1.
	cfgExpect(t, cfgPut(s, "/tagged", body, nil), http.StatusConflict, "BucketAlreadyOwnedByYou")
	cfgExpect(t, cfgPut(s, "/tagged", "", nil), http.StatusOK, "")

	reserved := cfgDoc("CreateBucketConfiguration", "<Tags>"+cfgTag("aws:x", "a")+"</Tags>")
	cfgExpect(t, cfgPut(s, "/badtag", reserved, nil), http.StatusBadRequest, cfgInvalidTag)

	cfgExpect(t, cfgPut(s, "/badacl", "", map[string]string{cfgHeaderACL: cfgFakeACL}),
		http.StatusBadRequest, cfgInvalidArgument)
	// The failed CreateBucket must not have taken the name.
	cfgExpect(t, cfgPut(s, "/badacl", "", nil), http.StatusOK, "")
}

func TestBucketACL(t *testing.T) {
	s := cfgBucket(t)

	got := cfgGet(s, bucketAPath+"?acl")
	cfgExpect(t, got, http.StatusOK, "")
	assert.Contains(t, string(got.body), cfgOwner)
	assert.Contains(t, string(got.body), "FULL_CONTROL")

	// A new bucket has BucketOwnerEnforced, which refuses an ACL that grants more than the owner.
	cfgExpect(t, cfgPut(s, bucketAPath+"?acl", "", map[string]string{cfgHeaderACL: cfgPublicRead}),
		http.StatusBadRequest, "AccessControlListNotSupported")
	cfgExpect(t, cfgPut(s, bucketAPath+"?acl", "", map[string]string{cfgHeaderACL: "bucket-owner-full-control"}),
		http.StatusOK, "")

	cfgExpect(t, cfgDelete(s, bucketAPath+"?ownershipControls"), http.StatusNoContent, "")
	cfgExpect(t, cfgPut(s, bucketAPath+"?acl", "", map[string]string{cfgHeaderACL: "log-delivery-write"}),
		http.StatusOK, "")

	got = cfgGet(s, bucketAPath+"?acl")
	cfgExpect(t, got, http.StatusOK, "")
	assert.Contains(t, string(got.body), cfgLogGroup)
	assert.Contains(t, string(got.body), "READ_ACP")
	assert.Contains(t, string(got.body), "WRITE")

	grant := map[string]string{"x-amz-grant-read": `uri="` + cfgLogGroup + `"`}
	cfgExpect(t, cfgPut(s, bucketAPath+"?acl", "", grant), http.StatusOK, "")
}

func TestBucketACLErrors(t *testing.T) {
	s := cfgBucket(t)

	owner := "<Owner><ID>" + cfgOwner + "</ID></Owner>"
	group := func(uri, permission string) string {
		return `<AccessControlList><Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" ` +
			`xsi:type="Group"><URI>` + uri + "</URI></Grantee><Permission>" + permission + "</Permission></Grant>" +
			"</AccessControlList>"
	}
	policy := func(inner string) string { return cfgDoc("AccessControlPolicy", inner) }
	fakeGroup := "http://acs.amazonaws.com/groups/s3/Fake"
	header := func(name, value string) map[string]string { return map[string]string{name: value} }

	for name, tc := range map[string]struct {
		header map[string]string
		body   string
		code   string
	}{
		"fake canned":     {header(cfgHeaderACL, cfgFakeACL), "", cfgInvalidArgument},
		"fake group":      {header(cfgHeaderGrant, `uri="`+fakeGroup+`"`), "", cfgInvalidArgument},
		"fake grant type": {header(cfgHeaderGrant, `fakekey="1234"`), "", cfgInvalidArgument},
		"wrong id":        {header(cfgHeaderGrant, `id="wrong-id"`), "", cfgInvalidArgument},
		"nothing":         {nil, "", "MissingSecurityHeader"},
		"empty policy":    {nil, policy(""), cfgMalformedACL},
		"no owner":        {nil, policy(group(cfgLogGroup, "WRITE")), cfgMalformedACL},
		"no list":         {nil, policy(owner), cfgMalformedACL},
		"bad permission":  {nil, policy(owner + group(cfgLogGroup, "NOPE")), cfgMalformedACL},
		"bad owner id": {
			nil, policy("<Owner><ID>wrong-id</ID></Owner>" + group(cfgLogGroup, "WRITE")), cfgInvalidArgument,
		},
		"bad group": {nil, policy(owner + group(fakeGroup, "WRITE")), cfgInvalidArgument},
		"canned and grant": {
			map[string]string{cfgHeaderACL: "private", "x-amz-grant-read": `uri="` + cfgLogGroup + `"`},
			"",
			cfgInvalidRequest,
		},
		"canned and body": {header(cfgHeaderACL, "private"), policy(owner), "UnexpectedContent"},
	} {
		t.Run(name, func(t *testing.T) {
			cfgExpect(t, cfgPut(s, bucketAPath+"?acl", tc.body, tc.header), http.StatusBadRequest, tc.code)
		})
	}
}

// TestBucketACLWithAnEmptyList: an AccessControlList with no grants is a list,
// which LocalStack's validate_acl_acp takes (it checks that "Grants" is there),
// unlike a policy with no list at all.
func TestBucketACLWithAnEmptyList(t *testing.T) {
	s := cfgBucket(t)

	// A bucket that takes ACLs, which ObjectWriter ownership does and the default does not.
	cfgExpect(t, cfgDelete(s, bucketAPath+"?ownershipControls"), http.StatusNoContent, "")

	owner := "<Owner><ID>" + cfgOwner + "</ID></Owner>"
	cfgExpect(t, cfgPut(s, bucketAPath+"?acl", cfgDoc("AccessControlPolicy", owner), nil),
		http.StatusBadRequest, cfgMalformedACL)
	cfgExpect(t, cfgPut(s, bucketAPath+"?acl", cfgDoc("AccessControlPolicy", owner+"<AccessControlList/>"), nil),
		http.StatusOK, "")
}

func TestBucketPolicy(t *testing.T) {
	s := cfgBucket(t)

	cfgExpect(t, cfgGet(s, bucketAPath+"?policy"), http.StatusNotFound, "NoSuchBucketPolicy")

	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`
	cfgExpect(t, cfgPut(s, bucketAPath+"?policy", policy, nil), http.StatusNoContent, "")

	got := cfgGet(s, bucketAPath+"?policy")
	cfgExpect(t, got, http.StatusOK, "")
	assert.JSONEq(t, policy, string(got.body))

	for _, body := range []string{"", "invalid json", "{}"} {
		cfgExpect(t, cfgPut(s, bucketAPath+"?policy", body, nil), http.StatusBadRequest, "MalformedPolicy")
	}

	// ExpectedBucketOwner: the owner passes, another account is 403, a malformed id is 400.
	withOwner := func(owner string) reply {
		return s.do(call{
			method: http.MethodGet, target: bucketAPath + "?policy", region: regionUSEast1,
			header: map[string]string{cfgHeaderOwner: owner},
		})
	}
	cfgExpect(t, withOwner("000000000000"), http.StatusOK, "")
	cfgExpect(t, withOwner("000000000002"), http.StatusForbidden, "AccessDenied")
	cfgExpect(t, withOwner("abcd"), http.StatusBadRequest, "InvalidBucketOwnerAWSAccountID")

	cfgExpect(t, cfgDelete(s, bucketAPath+"?policy"), http.StatusNoContent, "")
	cfgExpect(t, cfgDelete(s, bucketAPath+"?policy"), http.StatusNoContent, "")
	cfgExpect(t, cfgGet(s, bucketAPath+"?policy"), http.StatusNotFound, "NoSuchBucketPolicy")
}

func TestPublicAccessBlockAndOwnershipControls(t *testing.T) {
	s := cfgBucket(t)
	pab := bucketAPath + "?publicAccessBlock"
	controls := bucketAPath + "?ownershipControls"

	// A new bucket blocks everything.
	got := cfgGet(s, pab)
	cfgExpect(t, got, http.StatusOK, "")
	assert.Contains(t, string(got.body), "<BlockPublicAcls>true</BlockPublicAcls>")

	// Settings the request leaves out are false.
	partial := cfgDoc("PublicAccessBlockConfiguration", "<BlockPublicAcls>false</BlockPublicAcls>")
	cfgExpect(t, cfgPut(s, pab, partial, nil), http.StatusOK, "")
	assert.Contains(t, string(cfgGet(s, pab).body), "<RestrictPublicBuckets>false</RestrictPublicBuckets>")

	cfgExpect(t, cfgDelete(s, pab), http.StatusNoContent, "")
	cfgExpect(t, cfgGet(s, pab), http.StatusNotFound, "NoSuchPublicAccessBlockConfiguration")

	// A new bucket enforces its owner.
	assert.Contains(t, string(cfgGet(s, controls).body), "BucketOwnerEnforced")

	rule := func(ownership string) string {
		return "<Rule><ObjectOwnership>" + ownership + "</ObjectOwnership></Rule>"
	}
	two := cfgDoc("OwnershipControls", rule("ObjectWriter")+rule("BucketOwnerPreferred"))
	cfgExpect(t, cfgPut(s, controls, two, nil), http.StatusBadRequest, cfgMalformedXML)
	cfgExpect(t, cfgPut(s, controls, cfgDoc("OwnershipControls", rule("Random")), nil),
		http.StatusBadRequest, cfgMalformedXML)
	cfgExpect(t, cfgPut(s, controls, cfgDoc("OwnershipControls", ""), nil), http.StatusBadRequest, cfgMalformedXML)
	cfgExpect(t, cfgPut(s, controls, cfgDoc("OwnershipControls", rule("ObjectWriter")), nil), http.StatusOK, "")
	assert.Contains(t, string(cfgGet(s, controls).body), "ObjectWriter")

	cfgExpect(t, cfgDelete(s, controls), http.StatusNoContent, "")
	cfgExpect(t, cfgGet(s, controls), http.StatusNotFound, "OwnershipControlsNotFoundError")
}

func TestAccelerateRequestPaymentAndEncryption(t *testing.T) {
	s := cfgBucket(t)
	accelerate := bucketAPath + "?accelerate"
	payment := bucketAPath + "?requestPayment"
	encryption := bucketAPath + "?encryption"

	status := func(value string) string { return cfgDoc("AccelerateConfiguration", "<Status>"+value+"</Status>") }
	payer := func(value string) string { return cfgDoc("RequestPaymentConfiguration", "<Payer>"+value+"</Payer>") }

	assert.NotContains(t, string(cfgGet(s, accelerate).body), "Status")
	cfgExpect(t, cfgPut(s, accelerate, status("Enabled"), nil), http.StatusOK, "")
	assert.Contains(t, string(cfgGet(s, accelerate).body), "<Status>Enabled</Status>")
	cfgExpect(t, cfgPut(s, accelerate, status(lowerEnabled), nil), http.StatusBadRequest, cfgMalformedXML)

	assert.Contains(t, string(cfgGet(s, payment).body), "BucketOwner")
	cfgExpect(t, cfgPut(s, payment, payer("Requester"), nil), http.StatusOK, "")
	assert.Contains(t, string(cfgGet(s, payment).body), "Requester")
	cfgExpect(t, cfgPut(s, payment, payer("Random"), nil), http.StatusBadRequest, cfgMalformedXML)

	assert.Contains(t, string(cfgGet(s, encryption).body), "AES256")

	rule := func(inner string) string {
		return cfgDoc("ServerSideEncryptionConfiguration",
			"<Rule><ApplyServerSideEncryptionByDefault>"+inner+"</ApplyServerSideEncryptionByDefault></Rule>")
	}
	withKey := "<KMSMasterKeyID>k</KMSMasterKeyID>"
	cfgExpect(t, cfgPut(s, encryption, rule("<SSEAlgorithm>AES256</SSEAlgorithm>"+withKey), nil),
		http.StatusBadRequest, cfgInvalidArgument)
	cfgExpect(t, cfgPut(s, encryption, rule("<SSEAlgorithm>nope</SSEAlgorithm>"), nil),
		http.StatusBadRequest, cfgMalformedXML)
	cfgExpect(t, cfgPut(s, encryption, cfgDoc("ServerSideEncryptionConfiguration", ""), nil),
		http.StatusBadRequest, cfgMalformedXML)
	cfgExpect(t, cfgPut(s, encryption, rule("<SSEAlgorithm>aws:kms</SSEAlgorithm>"+withKey), nil), http.StatusOK, "")
	assert.Contains(t, string(cfgGet(s, encryption).body), withKey)

	cfgExpect(t, cfgDelete(s, encryption), http.StatusNoContent, "")
	assert.Contains(t, string(cfgGet(s, encryption).body), "AES256", "a deleted configuration falls back to SSE-S3")
}

func TestAccelerateRefusesDottedBucket(t *testing.T) {
	s := newServer(t)
	cfgExpect(t, cfgPut(s, "/test.bucket.dotted", "", nil), http.StatusOK, "")

	body := cfgDoc("AccelerateConfiguration", "<Status>random</Status>")
	cfgExpect(t, cfgPut(s, "/test.bucket.dotted?accelerate", body, nil), http.StatusBadRequest, cfgInvalidRequest)
}

func TestLifecycleConfiguration(t *testing.T) {
	s := cfgBucket(t)
	target := bucketAPath + "?lifecycle"

	cfgExpect(t, cfgGet(s, target), http.StatusNotFound, "NoSuchLifecycleConfiguration")

	rule := func(inner string) string { return cfgDoc("LifecycleConfiguration", "<Rule>"+inner+"</Rule>") }
	days := "<Expiration><Days>7</Days></Expiration>"
	valid := rule("<ID>wholebucket</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status>" + days)
	cfgExpect(t, cfgPut(s, target, valid, nil), http.StatusOK, "")

	got := cfgGet(s, target)
	cfgExpect(t, got, http.StatusOK, "")
	assert.Contains(t, string(got.body), "<ID>wholebucket</ID>")
	assert.Equal(t, "all_storage_classes_128K", got.header.Get(cfgHeaderMinSz))

	cfgExpect(t, cfgPut(s, target, valid, map[string]string{cfgHeaderMinSz: "varies_by_storage_class"}),
		http.StatusOK, "")
	assert.Equal(t, "varies_by_storage_class", cfgGet(s, target).header.Get(cfgHeaderMinSz))
	cfgExpect(t, cfgPut(s, target, valid, map[string]string{cfgHeaderMinSz: "value"}),
		http.StatusBadRequest, cfgInvalidRequest)

	enabled := "<Status>Enabled</Status>"
	for name, tc := range map[string]struct{ rule, code string }{
		"missing id":       {"<Filter></Filter>" + enabled + days, cfgMalformedXML},
		"missing filter":   {"<ID>r</ID>" + enabled + days, cfgMalformedXML},
		"missing status":   {"<ID>r</ID><Filter></Filter>" + days, cfgMalformedXML},
		"empty noncurrent": {"<ID>r</ID><Filter></Filter>" + enabled + "<NoncurrentVersionExpiration/>", cfgMalformedXML},
		"and plus prefix": {
			"<ID>r</ID><Filter><And><Prefix>t</Prefix></And><Prefix></Prefix></Filter>" + enabled + days, cfgMalformedXML,
		},
		"size plus prefix": {
			"<ID>r</ID><Filter><ObjectSizeGreaterThan>5</ObjectSizeGreaterThan><Prefix></Prefix></Filter>" + enabled + days,
			cfgMalformedXML,
		},
		"marker plus days": {
			"<ID>r</ID><Filter></Filter>" + enabled +
				"<Expiration><Days>1</Days><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>",
			cfgMalformedXML,
		},
		"not midnight": {
			"<ID>r</ID><Filter></Filter>" + enabled + "<Expiration><Date>2023-01-01T02:02:00.000Z</Date></Expiration>",
			cfgInvalidArgument,
		},
		"duplicate tag keys": {
			"<ID>r</ID><Filter><And>" + cfgTag("a", "1") + cfgTag("a", "2") + "</And></Filter>" + enabled + days,
			cfgInvalidRequest,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfgExpect(t, cfgPut(s, target, rule(tc.rule), nil), http.StatusBadRequest, tc.code)
		})
	}

	date := "<Expiration><Date>2023-01-01T00:00:00.000Z</Date></Expiration>"
	cfgExpect(t, cfgPut(s, target, rule("<ID>r</ID><Filter></Filter>"+enabled+date), nil), http.StatusOK, "")

	cfgExpect(t, cfgDelete(s, target), http.StatusNoContent, "")
	cfgExpect(t, cfgDelete(s, target), http.StatusNoContent, "")
	cfgExpect(t, cfgGet(s, target), http.StatusNotFound, "NoSuchLifecycleConfiguration")
}

func TestObjectExpirationHeaderAndTags(t *testing.T) {
	s := cfgBucket(t)

	rule := func(id, filter, status, days string) string {
		return "<Rule><ID>" + id + "</ID><Filter>" + filter + "</Filter><Status>" + status + "</Status>" +
			"<Expiration><Days>" + days + "</Days></Expiration></Rule>"
	}
	lifecycle := cfgDoc("LifecycleConfiguration",
		rule("tagged", "<Tag><Key>expire</Key><Value>yes</Value></Tag>", "Enabled", "7")+
			rule("prefixed", "<Prefix>logs/</Prefix>", "Enabled", "3")+
			rule("off", "<Prefix>off/</Prefix>", "Disabled", "3"))
	cfgExpect(t, cfgPut(s, bucketAPath+"?lifecycle", lifecycle, nil), http.StatusOK, "")

	// A rule matches by tag, taken from the x-amz-tagging header.
	put := cfgPut(s, cfgObjectPath, "data", map[string]string{cfgHeaderTagged: "expire=yes&other=1"})
	cfgExpect(t, put, http.StatusOK, "")
	assert.Contains(t, put.header.Get(cfgHeaderExpiry), `rule-id="tagged"`)

	head := cfgHead(s, cfgObjectPath)
	assert.Contains(t, head.header.Get(cfgHeaderExpiry), `rule-id="tagged"`)
	assert.Equal(t, "2", head.header.Get("X-Amz-Tagging-Count"))

	// A day count runs from the midnight UTC after the last modification: whole days, at 00:00:00.
	date, err := http.ParseTime(strings.SplitN(head.header.Get(cfgHeaderExpiry), `"`, 3)[1])
	require.NoError(t, err)
	assert.Zero(t, date.Hour()+date.Minute()+date.Second())
	assert.WithinDuration(t, time.Now().Add(8*24*time.Hour), date, 2*24*time.Hour)

	// No tag, no prefix, a disabled rule: no header.
	for _, key := range []string{"plain", "off/x"} {
		assert.Empty(t, cfgPut(s, bucketAPath+"/"+key, "data", nil).header.Get(cfgHeaderExpiry), key)
	}

	logs := cfgPut(s, bucketAPath+"/logs/a", "data", nil)
	assert.Contains(t, logs.header.Get(cfgHeaderExpiry), `rule-id="prefixed"`)

	// Naming a version leaves the header out.
	assert.Empty(t, cfgHead(s, bucketAPath+"/logs/a?versionId=null").header.Get(cfgHeaderExpiry))

	// Object tagging replaces the set and the header follows it.
	other := cfgTagging(cfgTag("other", "2"))
	cfgExpect(t, cfgPut(s, cfgObjectPath+"?tagging", other, nil), http.StatusOK, "")

	got := cfgGet(s, cfgObjectPath+"?tagging")
	cfgExpect(t, got, http.StatusOK, "")
	assert.Contains(t, string(got.body), "<Key>other</Key>")
	assert.NotContains(t, string(got.body), "expire")
	assert.Empty(t, cfgHead(s, cfgObjectPath).header.Get(cfgHeaderExpiry))

	cfgExpect(t, cfgDelete(s, cfgObjectPath+"?tagging"), http.StatusNoContent, "")
	assert.Contains(t, string(cfgGet(s, cfgObjectPath+"?tagging").body), "TagSet")
	assert.Empty(t, cfgHead(s, cfgObjectPath).header.Get("X-Amz-Tagging-Count"))

	// Overwriting an object drops its tags.
	cfgExpect(t, cfgPut(s, cfgObjectPath+"?tagging", other, nil), http.StatusOK, "")
	cfgExpect(t, cfgPut(s, cfgObjectPath, "again", nil), http.StatusOK, "")
	assert.NotContains(t, string(cfgGet(s, cfgObjectPath+"?tagging").body), "<Key>")
}

func TestObjectTaggingErrors(t *testing.T) {
	s := cfgBucket(t)

	missing := cfgGet(s, bucketAPath+"/nokey?tagging")
	cfgExpect(t, missing, http.StatusNotFound, "NoSuchKey")
	assert.Equal(t, "bucket-a/nokey", missing.err(t).Key, "GetObjectTagging names the key with the bucket, as S3 does")

	other := cfgPut(s, bucketAPath+"/nokey?tagging", cfgTagging(""), nil)
	cfgExpect(t, other, http.StatusNotFound, "NoSuchKey")
	assert.Equal(t, "nokey", other.err(t).Key)

	cfgExpect(t, cfgDelete(s, bucketAPath+"/nokey?tagging"), http.StatusNotFound, "NoSuchKey")
	cfgExpect(t, cfgGet(s, "/nobucket/key?tagging"), http.StatusNotFound, "NoSuchBucket")

	tagged := func(value string) map[string]string { return map[string]string{cfgHeaderTagged: value} }
	cfgExpect(t, cfgPut(s, cfgObjectPath, "x", tagged("k=1&k=2")), http.StatusBadRequest, cfgInvalidArgument)
	cfgExpect(t, cfgPut(s, cfgObjectPath, "x", tagged("k=1,j=2")), http.StatusBadRequest, cfgInvalidArgument)

	// A query string with a bare word is a tag with an empty value.
	cfgExpect(t, cfgPut(s, cfgObjectPath, "x", tagged("wrongquery&wrongagain")), http.StatusOK, "")

	got := cfgGet(s, cfgObjectPath+"?tagging")
	assert.Contains(t, string(got.body), "<Key>wrongquery</Key>")
	assert.Contains(t, string(got.body), "<Key>wrongagain</Key>")

	// Object tags are limited to ten.
	cfgExpect(t, cfgPut(s, cfgObjectPath+"?tagging", cfgTagging(cfgTags(11)), nil), http.StatusBadRequest, cfgInvalidTag)

	bad := cfgPut(s, cfgObjectPath+"?tagging", cfgTagging(cfgTag("aws:x", "1")), nil)
	cfgExpect(t, bad, http.StatusBadRequest, cfgInvalidTag)
	assert.Equal(t, "Your TagKey cannot be prefixed with aws:", bad.err(t).Message)
}

func TestObjectACL(t *testing.T) {
	s := cfgBucket(t)
	acl := cfgObjectPath + "?acl"
	canned := func(value string) map[string]string { return map[string]string{cfgHeaderACL: value} }

	cfgExpect(t, cfgPut(s, cfgObjectPath, "x", nil), http.StatusOK, "")

	got := cfgGet(s, acl)
	cfgExpect(t, got, http.StatusOK, "")
	assert.Contains(t, string(got.body), "FULL_CONTROL")
	assert.NotContains(t, string(got.body), "AllUsers")

	// BucketOwnerEnforced refuses a grant to everyone, until the controls go.
	cfgExpect(t, cfgPut(s, acl, "", canned(cfgPublicRead)), http.StatusBadRequest, "AccessControlListNotSupported")
	cfgExpect(t, cfgDelete(s, bucketAPath+"?ownershipControls"), http.StatusNoContent, "")
	cfgExpect(t, cfgPut(s, acl, "", canned(cfgPublicRead)), http.StatusOK, "")
	assert.Contains(t, string(cfgGet(s, acl).body), "AllUsers")

	cfgExpect(t, cfgPut(s, acl, "", canned(cfgFakeACL)), http.StatusBadRequest, cfgInvalidArgument)
	cfgExpect(t, cfgPut(s, acl, "", nil), http.StatusBadRequest, "MissingSecurityHeader")

	// PutObject takes an ACL too, and a bad one stops the write.
	cfgExpect(t, cfgPut(s, bucketAPath+"/other", "x", canned("authenticated-read")), http.StatusOK, "")
	assert.Contains(t, string(cfgGet(s, bucketAPath+"/other?acl").body), "AuthenticatedUsers")
	cfgExpect(t, cfgPut(s, bucketAPath+"/third", "x", canned(cfgFakeACL)), http.StatusBadRequest, cfgInvalidArgument)
	cfgExpect(t, cfgGet(s, bucketAPath+"/third"), http.StatusNotFound, "NoSuchKey")

	cfgExpect(t, cfgPut(s, bucketAPath+"/nokey?acl", "", canned("private")), http.StatusNotFound, "NoSuchKey")
}
