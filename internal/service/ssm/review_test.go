package ssm_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// The tests here are the cases of the review of SQ-266 (feat/US-7-ssm at
// f8c8c66), each with the answer LocalStack gave it.

const (
	arnEx           = arnPrefix + "ex"
	fieldOption     = "Option"
	kInvalidFilter  = "InvalidFilterOption"
	kInvalidResID   = "InvalidResourceId"
	fieldDataType   = "DataType"
	kParameterVer   = "ParameterVersionNotFound"
	fieldNextToken  = "NextToken"
	fieldMaxResults = "MaxResults"
)

// badOption is the text of an option a filter key does not take.
const badOption = "The following filter option is not valid: Contains. Valid options include: [BeginsWith, Equals]."

// names reads the names of the parameters in a listing answer.
func names(out map[string]any) []string {
	items := list(out, kParameters)
	got := make([]string, 0, len(items))

	for _, item := range items {
		entry, _ := item.(map[string]any)
		name, _ := entry[kName].(string)
		got = append(got, name)
	}

	return got
}

// first is the first of the parameters in a listing answer.
func first(t *testing.T, out map[string]any) map[string]any {
	t.Helper()

	items := list(out, kParameters)
	require.NotEmpty(t, items)

	entry, ok := items[0].(map[string]any)
	require.True(t, ok)

	return entry
}

// token reads the NextToken of an answer, "" when there is none.
func token(out map[string]any) string {
	text, _ := out[fieldNextToken].(string)

	return text
}

// Test_putRefusesANameThatCleansToNothing: PutParameter "/" answered 200 and
// stored a parameter with no name, which Restore refused, and the engine moved
// the whole file aside as unloadable. LocalStack stores it too, and then
// answers 500 to every read of it and to every DescribeParameters.
func Test_putRefusesANameThatCleansToNothing(t *testing.T) {
	for _, name := range []string{"/", "", "//", arnPrefix} {
		t.Run(name, func(t *testing.T) {
			source := ssm.New()
			c := newClient(t, source)
			c.ok(kPutParameter, obj{kName: nameGoodOne, kValue: "v", kType: kString})

			msg := c.fails(kPutParameter, obj{kName: name, kValue: "v", kType: kString}, kValidationException)
			require.Contains(t, msg, "sub-paths divided by slash symbol")

			var file bytes.Buffer
			require.NoError(t, source.Snapshot(&file))

			target := ssm.New()
			require.NoError(t, target.Restore(bytes.NewReader(file.Bytes())))

			r := newClient(t, target)
			require.Equal(t, "v", param(r.ok(kGetParameter, obj{kName: nameGoodOne}))[kValue])
			require.Equal(t, []string{nameGoodOne}, names(r.ok(kDescribeParameters, obj{})))
		})
	}
}

// Test_snapshotKeepsEmptyText: a window named "" read back with no Name after a
// restart, because the snapshot left out a pointer to an empty string.
func Test_snapshotKeepsEmptyText(t *testing.T) {
	source := ssm.New()
	c := newClient(t, source)
	window := obj{kName: "", kSchedule: kRateDay, kDuration: 1, kCutoff: 0}
	id := c.ok(kCreateMaintenanceWindow, window)[fieldWindowID]
	want := c.ok(kGetMaintenanceWindow, obj{fieldWindowID: id})
	require.Contains(t, want, kName)

	var file bytes.Buffer
	require.NoError(t, source.Snapshot(&file))

	target := ssm.New()
	require.NoError(t, target.Restore(bytes.NewReader(file.Bytes())))
	require.Equal(t, want, newClient(t, target).ok(kGetMaintenanceWindow, obj{fieldWindowID: id}))
}

// Test_tagOperationsTakeAParameterARN: ListTagsForResource with an ARN answered
// 400 InvalidResourceId, and Add and Remove too. LocalStack answers 200 and
// reads and changes the tags of the parameter the ARN names.
func Test_tagOperationsTakeAParameterARN(t *testing.T) {
	c := newClient(t, ssm.New())
	c.ok(kPutParameter, obj{kName: "ex", kValue: "v", kType: kString, kTags: []obj{{fieldKey: "a", kValue: "1"}}})
	c.ok(kPutParameter, obj{
		kName: "/e/x", kValue: "v", kType: kString, kTags: []obj{{fieldKey: "b", kValue: "2"}},
	})

	byARN := func(arn string) obj { return obj{kResourceType: resourceParameter, kResourceID: arn} }
	tagsOf := func(id string) []any {
		return list(c.ok(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: id}), kTagList)
	}

	require.Equal(t, []any{obj{fieldKey: "a", kValue: "1"}}, tagsOf(arnEx))
	require.Equal(t, []any{obj{fieldKey: "b", kValue: "2"}}, tagsOf(arnPrefix+"e/x"))

	c.ok(kAddTagsToResource, with(byARN(arnEx), kTags, []obj{{fieldKey: "c", kValue: "3"}}))
	require.Equal(t, []any{obj{fieldKey: "a", kValue: "1"}, obj{fieldKey: "c", kValue: "3"}}, tagsOf("ex"))

	c.ok(kRemoveTagsFromResource, with(byARN(arnEx), "TagKeys", []string{"a"}))
	require.Equal(t, []any{obj{fieldKey: "c", kValue: "3"}}, tagsOf("ex"))

	c.fails(kListTagsForResource, byARN(arnPrefix+"nope"), kInvalidResID)
}

// Test_tagsOfAVersionThatIsNotThere: ListTagsForResource "tg:9" answered
// InvalidResourceId, and LocalStack answers ParameterVersionNotFound.
func Test_tagsOfAVersionThatIsNotThere(t *testing.T) {
	c := newClient(t, ssm.New())
	c.ok(kPutParameter, obj{kName: "tg", kValue: "v", kType: kString})

	msg := c.fails(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: "tg:9"}, kParameterVer)
	require.Equal(t, "Systems Manager could not find version 9 of tg. Verify the version and try again.", msg)
	tags := c.ok(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: "tg:1"})
	require.Empty(t, list(tags, kTagList))
}

// Test_tierFilterMatches: a Tier filter matched nothing, because moto compares
// no tier. The tier is compared here.
func Test_tierFilterMatches(t *testing.T) {
	c := newClient(t, ssm.New())

	for i := range 3 {
		c.ok(kPutParameter, obj{kName: "/t/s" + strconv.Itoa(i), kValue: "v", kType: kString})
	}

	c.ok(kPutParameter, obj{kName: nameAdvanced, kValue: "v", kType: kString, kTier: "Advanced"})

	tier := func(option string, values ...string) []string {
		f := obj{fieldKey: kTier, kValues: values}
		if option != "" {
			f[fieldOption] = option
		}

		return names(c.ok(kDescribeParameters, obj{fieldFilters: []obj{f}}))
	}

	require.Equal(t, []string{"/t/s0", "/t/s1", "/t/s2"}, tier("", kStandard))
	require.Equal(t, []string{nameAdvanced}, tier("", "Advanced"))
	require.Equal(t, []string{nameAdvanced}, tier("BeginsWith", "Advanced"))
	require.Empty(t, tier("", "Intelligent-Tiering"))
	require.Equal(t, []string{"/t/s0", "/t/s1", "/t/s2", nameAdvanced}, tier("", kStandard, "Advanced"))

	msg := c.fails(kDescribeParameters, obj{fieldFilters: []obj{
		{fieldKey: kTier, fieldOption: "Contains", kValues: []string{kStandard}},
	}}, kInvalidFilter)
	require.Equal(t, badOption, msg)
}

// Test_overwriteResetsDataType: an overwrite without DataType kept the old one.
// LocalStack resets it to text.
func Test_overwriteResetsDataType(t *testing.T) {
	c := newClient(t, ssm.New())
	c.ok(kPutParameter, obj{kName: "dt", kValue: "ami-1", kType: kString, fieldDataType: kImageType})
	require.Equal(t, kImageType, param(c.ok(kGetParameter, obj{kName: "dt"}))[fieldDataType])

	c.ok(kPutParameter, obj{kName: "dt", kValue: "ami-2", kOverwrite: true, fieldDataType: kImageType})
	require.Equal(t, kImageType, param(c.ok(kGetParameter, obj{kName: "dt"}))[fieldDataType])

	c.ok(kPutParameter, obj{kName: "dt", kValue: "x", kOverwrite: true})
	require.Equal(t, "text", param(c.ok(kGetParameter, obj{kName: "dt"}))[fieldDataType])
}

// Test_filterValidation: the key pattern was not anchored, and a Type filter
// took any option. The texts are LocalStack's, which are moto's.
func Test_filterValidation(t *testing.T) {
	c := newClient(t, ssm.New())
	c.ok(kPutParameter, obj{kName: "/f/a", kValue: "v", kType: kString})

	describe := func(filters ...obj) obj { return obj{fieldFilters: filters} }
	byPath := func(filters ...obj) obj { return obj{kPath: "/f", fieldFilters: filters} }

	xName := obj{fieldKey: "xName", kValues: []string{"x"}}
	for op, body := range map[string]obj{kDescribeParameters: describe(xName), kGetParametersByPath: byPath(xName)} {
		msg := c.fails(op, body, kValidationException)
		require.Equal(t, `1 validation error detected: Value "xName" at "parameterFilters.1.member.key" failed to `+
			"satisfy constraint: Member must satisfy regular expression pattern: tag:.+|Name|Type|KeyId|Path|Label|Tier", msg)
	}

	// moto matches the pattern from the start of the key only, so a key may run
	// on after one of the names; such a filter matches nothing.
	require.Empty(t, names(c.ok(kDescribeParameters, describe(obj{fieldKey: "NameFoo", kValues: []string{"x"}}))))

	contains := obj{fieldKey: kType, fieldOption: "Contains", kValues: []string{kString}}
	msg := c.fails(kGetParametersByPath, byPath(contains), kInvalidFilter)
	require.Equal(t, badOption, msg)

	// Both texts name the Name filter, whichever key the request has.
	msg = c.fails(kDescribeParameters, describe(obj{fieldKey: kType}), "InvalidFilterValue")
	require.Equal(t, "The following filter values are missing : null for filter key Name.", msg)

	msg = c.fails(kDescribeParameters, describe(
		obj{fieldKey: kType, kValues: []string{kString}}, obj{fieldKey: kType, kValues: []string{kString}},
	), "InvalidFilterKey")
	require.Equal(t, "The following filter is duplicated in the request: Name. "+
		"A request can contain only one occurrence of a specific filter.", msg)

	// A value over the limit is written as Python writes a list of strings.
	long := strings.Repeat("v", 1025)
	tooLong := obj{fieldKey: "Name", kValues: []string{long, "it's"}}
	msg = c.fails(kDescribeParameters, describe(tooLong), kValidationException)
	require.Equal(t, `1 validation error detected: Value "['`+long+`', "it's"]" at "parameterFilters.1.member.values" `+
		"failed to satisfy constraint: [Member must have length less than or equal to 1024, "+
		"Member must have length greater than or equal to 1]", msg)
}

// Test_aParameterWithoutAType: Overwrite on a new name stores a parameter with
// no type, which every read answered as kType:"". LocalStack leaves the member
// out.
func Test_aParameterWithoutAType(t *testing.T) {
	c := newClient(t, ssm.New())
	c.ok(kPutParameter, obj{kName: nameNoType, kValue: "v", kOverwrite: true})

	require.NotContains(t, param(c.ok(kGetParameter, obj{kName: nameNoType})), kType)
	require.NotContains(t, first(t, c.ok(kGetParameters, obj{fieldNames: []string{nameNoType}})), kType)
	require.NotContains(t, first(t, c.ok(kDescribeParameters, obj{})), kType)
	require.NotContains(t, first(t, c.ok(kGetParametersByPath, obj{kPath: "/"})), kType)
	require.NotContains(t, first(t, c.ok(kGetParameterHistory, obj{kName: nameNoType})), kType)
}

// Test_emptyTextIsLeftOut: Description "" came back as "Description":"", and
// LocalStack leaves it out.
func Test_emptyTextIsLeftOut(t *testing.T) {
	c := newClient(t, ssm.New())
	c.ok(kPutParameter, obj{
		kName: "/e/emp", kValue: "v", kType: kString, "Description": "", "AllowedPattern": "", "KeyId": "",
	})

	for _, out := range []map[string]any{
		c.ok(kDescribeParameters, obj{}), c.ok(kGetParameterHistory, obj{kName: "/e/emp"}),
	} {
		entry := first(t, out)
		require.NotContains(t, entry, "Description")
		require.NotContains(t, entry, "AllowedPattern")
		require.NotContains(t, entry, "KeyId")
	}
}

// Test_labelAndSelectorQuirks are the small differences of the review, each
// answered as LocalStack does.
func Test_labelAndSelectorQuirks(t *testing.T) {
	c := newClient(t, ssm.New())
	c.ok(kPutParameter, obj{kName: namePlain, kValue: "v", kType: kString})
	c.ok(kPutParameter, obj{kName: namePlain, kValue: "v2", kOverwrite: true})

	// moto reads version 0 as no version and labels the latest.
	out := c.ok(kLabelParameterVersion, obj{kName: namePlain, fieldVersion: 0, fieldLabels: []string{"zero"}})
	require.InDelta(t, 2, out[fieldVersion], 0)
	require.InDelta(t, 2, param(c.ok(kGetParameter, obj{kName: "plain:zero"}))[kVersion], 0)

	// A label sent twice is stored twice.
	c.ok(kLabelParameterVersion, obj{kName: namePlain, fieldVersion: 1, fieldLabels: []string{"ok", "ok"}})
	history := first(t, c.ok(kGetParameterHistory, obj{kName: namePlain}))
	require.Equal(t, []any{"ok", "ok"}, history[fieldLabels])

	// moto compares the text of the version, so "01" is not version 1.
	msg := c.fails(kGetParameter, obj{kName: "plain:01"}, kParameterVer)
	require.Equal(t, "Systems Manager could not find version 01 of plain. Verify the version and try again.", msg)
	require.InDelta(t, 1, param(c.ok(kGetParameter, obj{kName: "plain:1"}))[kVersion], 0)

	msg = c.fails(kUnlabelParameterVersion, obj{kName: namePlain, fieldLabels: []string{"ok"}}, kValidationException)
	require.Equal(t, "1 validation error detected: Value 'null' at 'parameterVersion' failed to satisfy constraint: "+
		"Member must not be null", msg)

	// A legacy filter of a key moto does not know passes nothing.
	require.Empty(t, names(c.ok(kDescribeParameters, obj{kFilters: []obj{{fieldKey: kBogus, kValues: []string{"x"}}}})))
}

// Test_pagingTokens: the NextToken is moto's, the offset of the next match in
// the list the store has when the page is asked for.
func Test_pagingTokens(t *testing.T) {
	c := newClient(t, ssm.New())

	for i := 1; i <= 25; i++ {
		c.ok(kPutParameter, obj{kName: "/p/a" + strconv.Itoa(i), kValue: "v", kType: kString})
	}

	c.ok(kPutParameter, obj{kName: "/p/sub/x", kValue: "v", kType: kString})

	first := c.ok(kGetParametersByPath, obj{kPath: "/p"})
	require.Len(t, names(first), 10)
	require.Equal(t, "10", token(first))

	// Reusing a token gives the same page.
	second := c.ok(kGetParametersByPath, obj{kPath: "/p", fieldNextToken: "10"})
	require.Equal(t, second, c.ok(kGetParametersByPath, obj{kPath: "/p", fieldNextToken: "10"}))
	require.Equal(t, "/p/a11", names(second)[0])
	require.Equal(t, "20", token(second))

	last := c.ok(kGetParametersByPath, obj{kPath: "/p", fieldNextToken: "20"})
	require.Equal(t, []string{"/p/a21", "/p/a22", "/p/a23", "/p/a24", "/p/a25"}, names(last))
	require.Empty(t, token(last), "a page that is not full gives no token")

	// Removing a parameter ahead of the token moves the offset onto the one
	// after it, as it does in moto.
	c.ok(kDeleteParameter, obj{kName: "/p/a1"})
	require.Equal(t, "/p/a12", names(c.ok(kGetParametersByPath, obj{kPath: "/p", fieldNextToken: "10"}))[0])

	// A full last page gives a token, and the page after it is empty. The 25
	// parameters are 24 now.
	full := c.ok(kGetParametersByPath, obj{kPath: "/p", fieldNextToken: "19", fieldMaxResults: 5})
	require.Equal(t, "24", token(full))
	require.Empty(t, names(c.ok(kGetParametersByPath, obj{kPath: "/p", fieldNextToken: "24"})))
	require.Empty(t, names(c.ok(kGetParametersByPath, obj{kPath: "/p", fieldNextToken: "99999"})))

	c.fails(kGetParametersByPath, obj{kPath: "/p", fieldNextToken: "-1"}, "InvalidNextToken")
	c.fails(kGetParametersByPath, obj{kPath: "/p", fieldNextToken: "abc"}, "InvalidNextToken")
}

// walkNames reads every page of op and returns the names in order.
func walkNames(c *client, op string, body obj) []string {
	var all []string

	for next, pages := "", 0; pages < 1000; pages++ {
		out := c.ok(op, with(body, fieldNextToken, next))
		all = append(all, names(out)...)

		if next = token(out); next == "" {
			return all
		}
	}

	panic("a listing that does not end")
}

// Test_pagingAfterManyRemovals: removals leave empty places in the order that
// are dropped once there are many; every listing still returns what is left,
// once each, in the order it was made.
func Test_pagingAfterManyRemovals(t *testing.T) {
	c := newClient(t, ssm.New())

	// Every third parameter is a StringList.
	var want, lists []string

	for i := range 300 {
		name, kind := "/c/n"+strconv.Itoa(i), kString
		if i%3 == 0 {
			kind = "StringList"
		}

		c.ok(kPutParameter, obj{kName: name, kValue: "v", kType: kind})

		if i < 200 {
			continue
		}

		want = append(want, name)

		if kind == kStringList {
			lists = append(lists, name)
		}
	}

	for i := range 200 {
		c.ok(kDeleteParameter, obj{kName: "/c/n" + strconv.Itoa(i)})
	}

	c.ok(kPutParameter, obj{kName: "/c/new", kValue: "v", kType: kString})
	want = append(want, "/c/new")

	require.Equal(t, want, walkNames(c, kGetParametersByPath, obj{kPath: "/c"}))
	require.Equal(t, want, walkNames(c, kDescribeParameters, obj{}))

	// A filter that passes some of what is left still pages from the token.
	filter := obj{fieldFilters: []obj{{fieldKey: kType, kValues: []string{kStringList}}}}
	require.Equal(t, lists, walkNames(c, kDescribeParameters, filter))
	require.Equal(t, lists, walkNames(c, kGetParametersByPath, with(filter, kPath, "/c")))
}

// walkPages reads every page of op and returns each page's names and token.
// before runs ahead of each page; a call that changes the store makes the
// listing walk from the start for that page, as a store with no cursor does.
func walkPages(c *client, op string, body obj, before func()) [][]string {
	var pages [][]string

	for next, n := "", 0; n < 1000; n++ {
		before()

		out := c.ok(op, with(body, fieldNextToken, next))
		pages = append(pages, append(names(out), "token="+token(out)))

		if next = token(out); next == "" {
			return pages
		}
	}

	panic("a listing that does not end")
}

// Test_aCursorGivesThePageAWalkFromTheStartGives: the next page of a listing
// starts where the last one ended, and the page is the one moto would cut,
// with the token it would give, whether or not the store remembers where.
func Test_aCursorGivesThePageAWalkFromTheStartGives(t *testing.T) {
	c := newClient(t, ssm.New())

	for i := range 230 {
		prefix, kind := "/x/", kString
		if i%4 == 0 {
			prefix, kind = "/y/", kStringList
		}

		c.ok(kPutParameter, obj{kName: prefix + "n" + strconv.Itoa(i), kValue: "v", kType: kind})
	}

	// Removals leave empty places in the order, and a label and a tag change
	// what the filters see.
	for i := 0; i < 230; i += 3 {
		prefix := "/x/"
		if i%4 == 0 {
			prefix = "/y/"
		}

		c.ok(kDeleteParameter, obj{kName: prefix + "n" + strconv.Itoa(i)})
	}

	c.ok(kLabelParameterVersion, obj{kName: "/x/n1", fieldLabels: []string{"l"}})
	c.ok(kAddTagsToResource, obj{
		kResourceType: resourceParameter, kResourceID: "/x/n5", kTags: []obj{{fieldKey: "k", kValue: "v"}},
	})

	for _, listing := range []struct {
		op   string
		body obj
	}{
		{kGetParametersByPath, obj{kPath: "/x"}},
		{kGetParametersByPath, obj{kPath: "/", kRecursive: true, fieldMaxResults: 7}},
		{kGetParametersByPath, obj{kPath: "/y", fieldFilters: []obj{{fieldKey: kType, kValues: []string{kStringList}}}}},
		{kDescribeParameters, obj{}},
		{kDescribeParameters, obj{fieldFilters: []obj{{fieldKey: kType, kValues: []string{kStringList}}}}},
		{kDescribeParameters, obj{kFilters: []obj{{fieldKey: "Name", kValues: []string{"/y"}}}}},
	} {
		remembered := walkPages(c, listing.op, listing.body, func() {})
		require.Greater(t, len(remembered), 1)

		fromTheStart := walkPages(c, listing.op, listing.body, func() {
			c.fails(kDeleteParameter, obj{kName: "/no/such"}, kParameterNotFound)
		})
		require.Equal(t, remembered, fromTheStart)
	}
}

// Test_listingsWhileWriting: pages read by several callers, whose cursors are
// shared, while another call removes and makes parameters. The race detector
// is the assertion.
func Test_listingsWhileWriting(t *testing.T) {
	h := handlerFor(t, ssm.New(), nil)
	w := newSink()

	for i := range 300 {
		require.NoError(t, putNumbered(h, w, i, "v"))
	}

	err := fanOut(5, func(worker int) error {
		w := newSink()
		w.body = &bytes.Buffer{}

		for round := range 25 {
			var err error

			if worker == 0 {
				err = removeNumbered(h, w, round)
				if err == nil {
					err = putNumbered(h, w, round, "w")
				}
			} else {
				err = pageThrough(h, w, []string{kDescribeParameters, kGetParametersByPath}[worker%2])
			}

			if err != nil {
				return err
			}
		}

		return nil
	})
	require.NoError(t, err)
}

// removeNumbered removes the parameter numbered i.
func removeNumbered(h http.Handler, w *sink, i int) error {
	raw, err := json.Marshal(obj{kName: paramName(i)})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	return send(h, w, kDeleteParameter, raw)
}

// putNumbered makes the parameter numbered i, or overwrites it.
func putNumbered(h http.Handler, w *sink, i int, value string) error {
	raw, err := json.Marshal(obj{kName: paramName(i), kValue: value, kType: kString, kOverwrite: true})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	return send(h, w, kPutParameter, raw)
}

// pageThrough reads every page of op over the benchmark path.
func pageThrough(h http.Handler, w *sink, op string) error {
	for token := ""; ; {
		var err error

		token, err = nextToken(h, w, op, obj{kPath: benchPath}, token)
		if err != nil {
			return err
		}

		if token == "" {
			return nil
		}
	}
}

// Test_removedParametersAreNotSnapshotted: what was removed is not in the
// snapshot, and a restored store pages and removes as a new one does.
func Test_removedParametersAreNotSnapshotted(t *testing.T) {
	source := ssm.New()
	c := newClient(t, source)

	for i := range 150 {
		c.ok(kPutParameter, obj{kName: "/r/n" + strconv.Itoa(i), kValue: "v", kType: kString})
	}

	for i := range 100 {
		c.ok(kDeleteParameter, obj{kName: "/r/n" + strconv.Itoa(i)})
	}

	var file bytes.Buffer
	require.NoError(t, source.Snapshot(&file))

	target := ssm.New()
	require.NoError(t, target.Restore(bytes.NewReader(file.Bytes())))

	r := newClient(t, target)
	require.Equal(t, walkNames(c, kDescribeParameters, obj{}), walkNames(r, kDescribeParameters, obj{}))
	require.Len(t, walkNames(r, kGetParametersByPath, obj{kPath: "/r"}), 50)

	r.ok(kDeleteParameter, obj{kName: "/r/n149"})
	r.ok(kPutParameter, obj{kName: "/r/z", kValue: "v", kType: kString})
	require.Len(t, walkNames(r, kDescribeParameters, obj{}), 50)
}
