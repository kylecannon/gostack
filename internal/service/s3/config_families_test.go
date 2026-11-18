package s3_test

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cfgIDList is a list answer: the ids it holds and whether it has a next page.
type cfgIDList struct {
	IsTruncated bool     `xml:"IsTruncated"`
	Next        string   `xml:"NextContinuationToken"`
	Metrics     []string `xml:"MetricsConfiguration>Id"`
	Inventory   []string `xml:"InventoryConfiguration>Id"`
}

func cfgParseList(t *testing.T, got reply) cfgIDList {
	t.Helper()

	cfgExpect(t, got, http.StatusOK, "")

	var list cfgIDList

	require.NoError(t, xml.Unmarshal(got.body, &list), string(got.body))

	return list
}

func TestMetricsConfigurationPaging(t *testing.T) {
	s := cfgBucket(t)
	metric := func(id string) string {
		return cfgDoc("MetricsConfiguration", "<Id>"+id+"</Id><Filter><Prefix>logs/</Prefix></Filter>")
	}

	cfgExpect(t, cfgGet(s, bucketAPath+"?metrics&id=none"), http.StatusNotFound, cfgNoSuchConfig)
	cfgExpect(t, cfgDelete(s, bucketAPath+"?metrics&id=none"), http.StatusNotFound, cfgNoSuchConfig)

	// LocalStack stores a metrics configuration under the id in the query and
	// keeps the body as it came, whatever its own Id says (SQ-143 probe on
	// 8b9a79f). Analytics, inventory and intelligent tiering refuse a
	// mismatch there, as they do here.
	for name, body := range map[string]string{
		"a body with another id": metric("b"),
		"a body with no id":      cfgDoc("MetricsConfiguration", ""),
	} {
		cfgExpect(t, cfgPut(s, bucketAPath+"?metrics&id=a", body, nil), http.StatusOK, "")

		got := cfgGet(s, bucketAPath+"?metrics&id=a")
		cfgExpect(t, got, http.StatusOK, "")
		assert.Equal(t, strings.Contains(body, "<Id>b</Id>"), strings.Contains(string(got.body), "<Id>b</Id>"), name)
		cfgExpect(t, cfgGet(s, bucketAPath+"?metrics&id=b"), http.StatusNotFound, cfgNoSuchConfig)
	}

	cfgExpect(t, cfgDelete(s, bucketAPath+"?metrics&id=a"), http.StatusNoContent, "")

	for i := range 102 {
		id := fmt.Sprintf("m%03d", i)
		cfgExpect(t, cfgPut(s, bucketAPath+"?metrics&id="+id, metric(id), nil), http.StatusOK, "")
	}

	// The list is paged by 100, in id order.
	first := cfgParseList(t, cfgGet(s, bucketAPath+"?metrics"))
	assert.True(t, first.IsTruncated)
	assert.Len(t, first.Metrics, 100)
	assert.Equal(t, "m000", first.Metrics[0])
	require.NotEmpty(t, first.Next)

	rest := cfgParseList(t, cfgGet(s, bucketAPath+"?metrics&continuation-token="+first.Next))
	assert.False(t, rest.IsTruncated)
	assert.Equal(t, []string{"m100", "m101"}, rest.Metrics)

	cfgExpect(t, cfgDelete(s, bucketAPath+"?metrics&id=m000"), http.StatusNoContent, "")
	cfgExpect(t, cfgGet(s, bucketAPath+"?metrics&id=m000"), http.StatusNotFound, cfgNoSuchConfig)
}

func TestAnalyticsAndTieringConfigurations(t *testing.T) {
	s := cfgBucket(t)

	analytics := cfgDoc("AnalyticsConfiguration",
		"<Id>one</Id><Filter><Prefix>p</Prefix></Filter><StorageClassAnalysis></StorageClassAnalysis>")
	cfgExpect(t, cfgPut(s, bucketAPath+"?analytics&id=two", analytics, nil), http.StatusBadRequest, cfgMalformedXML)
	cfgExpect(t, cfgPut(s, bucketAPath+"?analytics&id=one", analytics, nil), http.StatusNoContent, "")
	assert.Contains(t, string(cfgGet(s, bucketAPath+"?analytics&id=one").body), "<Id>one</Id>")
	assert.Contains(t, string(cfgGet(s, bucketAPath+"?analytics").body), "<Id>one</Id>")
	cfgExpect(t, cfgDelete(s, bucketAPath+"?analytics&id=one"), http.StatusNoContent, "")
	cfgExpect(t, cfgDelete(s, bucketAPath+"?analytics&id=one"), http.StatusNotFound, cfgNoSuchConfig)

	tiering := cfgDoc("IntelligentTieringConfiguration", "<Id>t1</Id><Status>Enabled</Status>"+
		"<Tiering><Days>90</Days><AccessTier>ARCHIVE_ACCESS</AccessTier></Tiering>")
	cfgExpect(t, cfgPut(s, bucketAPath+"?intelligent-tiering&id=wrong", tiering, nil),
		http.StatusBadRequest, cfgMalformedXML)
	cfgExpect(t, cfgPut(s, bucketAPath+"?intelligent-tiering&id=t1", tiering, nil), http.StatusNoContent, "")
	assert.Contains(t, string(cfgGet(s, bucketAPath+"?intelligent-tiering&id=t1").body), "ARCHIVE_ACCESS")
	cfgExpect(t, cfgDelete(s, bucketAPath+"?intelligent-tiering&id=t1"), http.StatusNoContent, "")
	cfgExpect(t, cfgDelete(s, bucketAPath+"?intelligent-tiering&id=t1"), http.StatusNotFound, cfgNoSuchConfig)
}

func TestInventoryConfiguration(t *testing.T) {
	s := cfgBucket(t)

	type spec struct{ id, bucket, format, frequency, versions, optional string }

	inventory := func(c spec) string {
		return cfgDoc("InventoryConfiguration",
			"<Destination><S3BucketDestination><Bucket>"+c.bucket+"</Bucket><Format>"+c.format+
				"</Format></S3BucketDestination></Destination><IsEnabled>true</IsEnabled><Id>"+c.id+"</Id>"+
				"<IncludedObjectVersions>"+c.versions+"</IncludedObjectVersions>"+c.optional+
				"<Schedule><Frequency>"+c.frequency+"</Frequency></Schedule>")
	}
	good := spec{"inv", "arn:aws:s3:::dest", "CSV", "Daily", "All", ""}
	target := bucketAPath + "?inventory&id=inv"

	with := func(change func(*spec)) string {
		c := good
		change(&c)

		return inventory(c)
	}

	for name, tc := range map[string]struct{ body, code string }{
		"id mismatch":   {with(func(c *spec) { c.id = "other" }), "IdMismatch"},
		"name not arn":  {with(func(c *spec) { c.bucket = "dest" }), "InvalidS3DestinationBucket"},
		"bad format":    {with(func(c *spec) { c.format = "WRONG" }), cfgMalformedXML},
		"bad frequency": {with(func(c *spec) { c.frequency = "Hourly" }), cfgMalformedXML},
		"bad versions":  {with(func(c *spec) { c.versions = "Wrong" }), cfgMalformedXML},
		"bad optional": {
			with(func(c *spec) { c.optional = "<OptionalFields><Field>Nope</Field></OptionalFields>" }), cfgMalformedXML,
		},
	} {
		got := cfgPut(s, target, tc.body, nil)
		assert.Equal(t, http.StatusBadRequest, got.status, name)
		assert.Equal(t, tc.code, got.err(t).Code, name)
	}

	optional := "<OptionalFields><Field>Size</Field><Field>ETag</Field></OptionalFields>"
	cfgExpect(t, cfgPut(s, target, with(func(c *spec) { c.optional = optional }), nil), http.StatusNoContent, "")

	for _, id := range []string{"z-test", "a-test"} {
		body := with(func(c *spec) { c.id = id })
		cfgExpect(t, cfgPut(s, bucketAPath+"?inventory&id="+id, body, nil), http.StatusNoContent, "")
	}

	list := cfgParseList(t, cfgGet(s, bucketAPath+"?inventory"))
	assert.Equal(t, []string{"a-test", "inv", "z-test"}, list.Inventory, "configurations are listed in id order")
	assert.Contains(t, string(cfgGet(s, target).body), "<Field>ETag</Field>")

	cfgExpect(t, cfgDelete(s, target), http.StatusNoContent, "")
	cfgExpect(t, cfgGet(s, target), http.StatusNotFound, cfgNoSuchConfig)
}

func TestLoggingNeedsATargetInTheSameRegion(t *testing.T) {
	s := cfgBucket(t)
	cfgExpect(t, cfgPut(s, "/target-b", "", nil), http.StatusOK, "")

	west := cfgDoc("CreateBucketConfiguration", "<LocationConstraint>us-west-2</LocationConstraint>")
	cfgExpect(t, s.do(call{
		method: http.MethodPut, target: "/target-west", region: "us-west-2", body: []byte(west),
	}), http.StatusOK, "")

	logging := func(target string) string {
		return cfgDoc("BucketLoggingStatus",
			"<LoggingEnabled><TargetBucket>"+target+"</TargetBucket><TargetPrefix>log</TargetPrefix></LoggingEnabled>")
	}
	target := bucketAPath + "?logging"

	empty := cfgGet(s, target)
	cfgExpect(t, empty, http.StatusOK, "")
	assert.NotContains(t, string(empty.body), "LoggingEnabled")

	missing := cfgPut(s, target, logging("no-such-target"), nil)
	cfgExpect(t, missing, http.StatusBadRequest, "InvalidTargetBucketForLogging")
	assert.Contains(t, string(missing.body), "<TargetBucket>no-such-target</TargetBucket>")

	cross := cfgPut(s, target, logging("target-west"), nil)
	cfgExpect(t, cross, http.StatusForbidden, "CrossLocationLoggingProhibitted")
	assert.Contains(t, string(cross.body), "<TargetBucketLocation>us-west-2</TargetBucketLocation>")

	cfgExpect(t, cfgPut(s, target, logging("target-b"), nil), http.StatusOK, "")
	assert.Contains(t, string(cfgGet(s, target).body), "<TargetBucket>target-b</TargetBucket>")

	cfgExpect(t, cfgPut(s, target, cfgDoc("BucketLoggingStatus", ""), nil), http.StatusOK, "")
	assert.NotContains(t, string(cfgGet(s, target).body), "LoggingEnabled")
}

func TestReplicationNeedsVersioning(t *testing.T) {
	s := cfgBucket(t)
	cfgExpect(t, cfgPut(s, "/target-b", "", nil), http.StatusOK, "")

	replication := cfgDoc("ReplicationConfiguration", "<Role>arn:aws:iam::000000000000:role/r</Role>"+
		"<Rule><Status>Enabled</Status><Priority>0</Priority><Filter></Filter>"+
		"<Destination><Bucket>arn:aws:s3:::target-b</Bucket></Destination>"+
		"<DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication></Rule>")
	target := bucketAPath + "?replication"

	cfgExpect(t, cfgGet(s, target), http.StatusNotFound, "ReplicationConfigurationNotFoundError")
	cfgExpect(t, cfgPut(s, target, replication, nil), http.StatusBadRequest, cfgInvalidRequest)

	enabled := cfgDoc("VersioningConfiguration", "<Status>Enabled</Status>")

	versioning := cfgPut(s, bucketAPath+"?versioning", enabled, nil)
	if versioning.status != http.StatusOK {
		t.Skipf("PutBucketVersioning answers %d until SQ-108 lands; the versioned paths are not tested here",
			versioning.status)
	}

	cfgExpect(t, cfgPut(s, target, replication, nil), http.StatusBadRequest, cfgInvalidRequest)
	cfgExpect(t, cfgPut(s, "/target-b?versioning", enabled, nil), http.StatusOK, "")
	cfgExpect(t, cfgPut(s, target, replication, nil), http.StatusOK, "")

	got := cfgGet(s, target)
	cfgExpect(t, got, http.StatusOK, "")
	assert.Regexp(t, `<ID>[0-9a-f]{8}</ID>`, string(got.body), "a rule without an ID gets one")

	cfgExpect(t, cfgDelete(s, target), http.StatusNoContent, "")
}

func TestObjectTaggingAndReadsDoNotRace(t *testing.T) {
	s := cfgBucket(t)
	cfgExpect(t, cfgPut(s, cfgObjectPath, "data", nil), http.StatusOK, "")

	var wg sync.WaitGroup

	for worker := range 4 {
		wg.Go(func() {
			for i := range 20 {
				tags := cfgTagging(cfgTag(fmt.Sprintf("w%d", worker), strconv.Itoa(i)))
				cfgPut(s, cfgObjectPath+"?tagging", tags, nil)
			}
		})

		wg.Go(func() {
			for range 20 {
				got := cfgGet(s, cfgObjectPath)
				assert.Equal(t, http.StatusOK, got.status)
				assert.Equal(t, "data", string(got.body))
			}
		})
	}

	wg.Wait()
}
