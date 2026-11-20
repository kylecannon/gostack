package ssm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// The invariant these tests hold SSM to: every state a call can reach survives
// a Snapshot and a Restore. A script of calls drives a provider through the
// gateway, the provider is flushed through an engine in each PERSIST_FORMAT,
// and a second engine loads what was written. The loaded provider must answer
// every read as the first one does, and the engine must not have moved the
// file aside as unloadable, which is what it does when Restore refuses it.
//
// A script is bytes, read as a stream of choices among the pools below, so the
// same driver serves a seeded run (Test_everyStateSurvivesARestart), a fuzz
// target (FuzzEveryStateSurvivesARestart) and the corpus the fuzz target keeps.

// GOSTACK_SSM_SURVIVE_SCRIPTS sets how many seeded scripts the test runs; the
// default is enough to reach every name below within a second or two.
const surviveScriptsVar = "GOSTACK_SSM_SURVIVE_SCRIPTS"

const (
	defaultSurviveScripts = 60
	surviveScriptBytes    = 900
	maxScriptCalls        = 300
	arnPrefix             = "arn:aws:ssm:us-east-1:000000000000:parameter/"
)

// The pools a script chooses from. The names are the ones the review of SQ-266
// found, and the spellings that clean to each other or to nothing.
var (
	surviveNames = []string{
		"/", "", "//", arnPrefix, arnPrefix + "/", arnPrefix + "a", arnPrefix + "a/b", arnPrefix + "aws/x",
		"a", "/a", "a/", "/a/", "a/b", "/a/b", "/a/b/", "///a///b", "/a//b", "a:1", "a:ok", "/a/b:2", "x y",
		"ünï/çode", "tab\t", "nl\n", "nul\x00", "q'\"", "AWSx", "ssm", "/ssm/x", "/aws/x", "arn:aws:ssm:r:a:b",
		"/" + strings.Repeat("x/", 20), strings.Repeat("n", 3000),
	}
	surviveValues = []string{
		"v", "", " ", textUni, "a\nb", strings.Repeat("v", 5000), "kms:alias/aws/ssm:x", "null", `{"a":1}`, "\x00",
	}
	surviveTypes  = []string{"", kString, kStringList, kSecureString, kBogus}
	surviveKeyIDs = []string{"", "alias/x", "k"}
	surviveTexts  = []string{"", "d", "ü"}
	surviveTiers  = []string{"", kStandard, "Advanced", "Intelligent-Tiering", kBogus}
	surviveData   = []string{"", "text", kImageType, "bogus"}
	survivePolicy = []string{"", "[]", `[{"Type":"Expiration"}]`, "not json", `["a"]`, "{}"}
	surviveLabels = []string{
		"", "a", "ok", "AWSx", "ssm", "1x", "a b", textUni, strings.Repeat("l", 101), "l1", "l2", "l3", "l4", "l5", "l6",
		"l7", "l8", "l9", "l10", "l11",
	}
	surviveTagKeys  = []string{"", "k", "tag:x", textUni, "a b"}
	surviveTagVals  = []string{"", "v", "ü"}
	surviveVersions = []int{0, 1, 2, 3, 99, 101, -1}
)

// script reads a byte stream as choices. Past its end every choice is the first
// one, and done reports the end.
type script struct {
	data []byte
	at   int
}

func (s *script) done() bool { return s.at >= len(s.data) }

// pick is a choice among n.
func (s *script) pick(n int) int {
	if s.done() {
		return 0
	}

	s.at++

	return int(s.data[s.at-1]) % n
}

func pickOf[T any](s *script, pool []T) T { return pool[s.pick(len(pool))] }

func (s *script) yes() bool { return s.pick(2) == 1 }

// some is a few picks from pool, one to four of them.
func some[T any](s *script, pool []T) []T {
	out := make([]T, 1+s.pick(4))
	for i := range out {
		out[i] = pickOf(s, pool)
	}

	return out
}

func (s *script) tags() []obj {
	if !s.yes() {
		return nil
	}

	tags := make([]obj, 1+s.pick(3))
	for i := range tags {
		tags[i] = obj{fieldKey: pickOf(s, surviveTagKeys), kValue: pickOf(s, surviveTagVals)}
	}

	return tags
}

// optional sets key in body to value when the script says so and value is not
// empty.
func (s *script) optional(body obj, key, value string) {
	if s.yes() && value != "" {
		body[key] = value
	}
}

// putBody is a PutParameter body.
func (s *script) putBody() obj {
	body := obj{kName: pickOf(s, surviveNames), kValue: pickOf(s, surviveValues)}
	if t := pickOf(s, surviveTypes); t != "" {
		body[kType] = t
	}

	if s.yes() {
		body[kOverwrite] = true
	}

	s.optional(body, "KeyId", pickOf(s, surviveKeyIDs))
	s.optional(body, "Description", pickOf(s, surviveTexts))
	s.optional(body, "AllowedPattern", pickOf(s, surviveTexts))
	s.optional(body, kTier, pickOf(s, surviveTiers))
	s.optional(body, "DataType", pickOf(s, surviveData))
	s.optional(body, "Policies", pickOf(s, survivePolicy))

	if tags := s.tags(); tags != nil {
		body[kTags] = tags
	}

	return body
}

// A call is an operation and its body.
type call struct {
	op   string
	body obj
}

// next reads the next call. windows are the ids of the windows made so far.
func (s *script) next(windows []string) call {
	name := pickOf(s, surviveNames)

	switch s.pick(9) {
	case 0, 1:
		return call{kPutParameter, s.putBody()}
	case 2:
		return call{kDeleteParameter, obj{kName: name}}
	case 3:
		return call{kDeleteParameters, obj{fieldNames: some(s, surviveNames)}}
	case 4:
		body := obj{kName: name, fieldLabels: some(s, surviveLabels)}
		if s.yes() {
			body[fieldVersion] = pickOf(s, surviveVersions)
		}

		return call{kLabelParameterVersion, body}
	case 5:
		body := obj{kName: name, fieldLabels: some(s, surviveLabels)}
		if s.yes() {
			body[fieldVersion] = pickOf(s, surviveVersions)
		}

		return call{kUnlabelParameterVersion, body}
	case 6:
		return s.tagCall(name, windows)
	case 7:
		return call{kCreateMaintenanceWindow, obj{
			kName: pickOf(s, surviveTexts), kSchedule: kRateDay, kDuration: s.pick(4), kCutoff: s.pick(2),
			kAllowUnassociated: s.yes(), kTags: s.tags(),
		}}
	default:
		id := "mw-none"
		if len(windows) > 0 && s.yes() {
			id = pickOf(s, windows)
		}

		return call{kDeleteMaintenanceWindow, obj{fieldWindowID: id}}
	}
}

// tagCall adds tags to or removes them from a parameter or a window.
func (s *script) tagCall(name string, windows []string) call {
	body := obj{kResourceType: resourceParameter, kResourceID: name}
	if len(windows) > 0 && s.yes() {
		body[kResourceType], body[kResourceID] = "MaintenanceWindow", pickOf(s, windows)
	}

	if s.yes() {
		body[kTags] = s.tags()

		return call{kAddTagsToResource, body}
	}

	body["TagKeys"] = some(s, surviveTagKeys)

	return call{kRemoveTagsFromResource, body}
}

// windowRef is the id of a window in an answer.
type windowRef struct {
	WindowID string `json:"WindowId"`
}

// direct sends calls to a handler with no network.
type direct struct {
	tb testing.TB
	h  http.Handler
}

// do sends one call and returns the status and the body.
func (d direct) do(op string, body any) (int, []byte) {
	d.tb.Helper()

	raw, err := json.Marshal(body)
	require.NoError(d.tb, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, benchEndpoint, bytes.NewReader(raw))
	require.NoError(d.tb, err)

	req.Header = benchHeader(op)

	rec := httptest.NewRecorder()
	d.h.ServeHTTP(rec, req)

	return rec.Code, rec.Body.Bytes()
}

// loadEngine loads SSM from dir through an engine that writes format.
func loadEngine(t *testing.T, dir string, format persist.Format) (*persist.Engine, *ssm.Provider) {
	t.Helper()

	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Formats: []persist.Format{format},
		MountDetector: func(string) bool { return false },
	})

	p := ssm.New()
	eng.Register(ssmName, p.Registration().Persist)
	require.NoError(t, eng.Load(t.Context()))

	return eng, p
}

// runScript drives a provider with the calls data spells and requires every
// call to answer below 500.
func runScript(t *testing.T, d direct, data []byte) {
	t.Helper()

	s := &script{data: data}

	var windows []string

	for n := 0; !s.done() && n < maxScriptCalls; n++ {
		c := s.next(windows)
		status, out := d.do(c.op, c.body)
		require.Less(t, status, http.StatusInternalServerError, "%s %v answered %d: %s", c.op, c.body, status, out)

		if c.op == kCreateMaintenanceWindow && status == http.StatusOK {
			var made windowRef
			require.NoError(t, json.Unmarshal(out, &made))

			windows = append(windows, made.WindowID)
		}
	}
}

// walk reads every page of a listing and returns every answer as one text, its
// NextTokens included: a restored store numbers its parameters from 0, but a
// token is the offset of its page in the matches, so it is the live store's.
func (d direct) walk(op string, body obj) string {
	d.tb.Helper()

	var all strings.Builder

	for token, pages := "", 0; pages < 2000; pages++ {
		status, out := d.do(op, with(body, "NextToken", token))
		all.WriteString(strconv.Itoa(status) + string(out))

		var page struct {
			NextToken string `json:"NextToken"`
		}

		if json.Unmarshal(out, &page) != nil || page.NextToken == "" || status != http.StatusOK {
			break
		}

		token = page.NextToken
	}

	return all.String()
}

// dump is the answer of every read SSM has, to every name a script uses.
func (d direct) dump() string {
	d.tb.Helper()

	var all strings.Builder

	add := func(op string, body any) {
		status, out := d.do(op, body)
		all.WriteString(op + " " + strconv.Itoa(status) + " " + string(out) + "\n")
	}

	for _, name := range surviveNames {
		for _, selector := range []string{"", ":1", ":2", ":ok", ":l1"} {
			add(kGetParameter, obj{kName: name + selector, kWithDecryption: true})
		}

		add(kGetParameter, obj{kName: name})
		add(kGetParameters, obj{fieldNames: []string{name, name + ":1"}})
		add(kGetParameterHistory, obj{kName: name, kWithDecryption: true})
		add(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: name})
	}

	all.WriteString(d.walk(kDescribeParameters, obj{}))
	all.WriteString(d.walk(kDescribeParameters, obj{fieldFilters: []obj{{fieldKey: kTier, kValues: []string{kStandard}}}}))
	all.WriteString(d.walk(kGetParametersByPath, obj{kPath: "/", kRecursive: true, kWithDecryption: true}))
	all.WriteString(d.walk(kGetParametersByPath, obj{kPath: "/a", kRecursive: true}))
	all.WriteString(d.walk(kGetParametersByPath, obj{kPath: "/a/b"}))

	_, windows := d.do("DescribeMaintenanceWindows", obj{})
	all.Write(windows)

	var ids struct {
		WindowIdentities []windowRef `json:"WindowIdentities"`
	}

	require.NoError(d.tb, json.Unmarshal(windows, &ids))

	for _, w := range ids.WindowIdentities {
		add(kGetMaintenanceWindow, obj{fieldWindowID: w.WindowID})
		add(kListTagsForResource, obj{kResourceType: "MaintenanceWindow", kResourceID: w.WindowID})
	}

	return all.String()
}

// survives runs data against a provider and requires its state to survive a
// restart in format, and a Snapshot and a Restore on their own.
func survives(t *testing.T, data []byte, format persist.Format) {
	t.Helper()

	dir := t.TempDir()
	eng, p := loadEngine(t, dir, format)
	live := direct{tb: t, h: handlerFor(t, p, eng)}

	runScript(t, live, data)
	require.NoError(t, eng.Flush(t.Context()))

	_, loaded := loadEngine(t, dir, format)
	// A store with nothing in it writes no file.
	entries, err := os.ReadDir(filepath.Join(dir, ssmName))
	if !errors.Is(err, fs.ErrNotExist) {
		require.NoError(t, err)
	}

	for _, entry := range entries {
		require.NotContains(t, entry.Name(), "unloadable", "the engine could not load what SSM wrote")
	}

	want := live.dump()
	require.Equal(t, want, direct{tb: t, h: handlerFor(t, loaded, nil)}.dump(), "a restart changes what SSM answers")

	var file bytes.Buffer
	require.NoError(t, p.Snapshot(&file))

	restored := ssm.New()
	require.NoError(t, restored.Restore(bytes.NewReader(file.Bytes())))

	var again bytes.Buffer
	require.NoError(t, restored.Snapshot(&again))
	require.Equal(t, file.String(), again.String(), "Restore then Snapshot writes the bytes it was given")
}

// Test_everyStateSurvivesARestart runs seeded scripts against SSM, in each
// PERSIST_FORMAT. Without the refusal of a name that cleans to nothing, a seed
// reaches PutParameter "/" and the second engine finds the file unloadable.
func Test_everyStateSurvivesARestart(t *testing.T) {
	scripts := defaultSurviveScripts

	if text := os.Getenv(surviveScriptsVar); text != "" {
		n, err := strconv.Atoi(text)
		require.NoError(t, err)

		scripts = n
	}

	for _, format := range []persist.Format{persist.FormatJSON, persist.FormatBinary} {
		t.Run(string(format), func(t *testing.T) {
			for seed := range scripts {
				rng := rand.New(rand.NewPCG(uint64(seed), 269))
				data := make([]byte, surviveScriptBytes)

				for i := range data {
					data[i] = byte(rng.UintN(256))
				}

				t.Run("seed="+strconv.Itoa(seed), func(t *testing.T) { survives(t, data, format) })
			}
		})
	}
}

// FuzzEveryStateSurvivesARestart is the same check with the script chosen by
// the fuzzer. Its corpus holds the scripts that reached a state Restore had
// refused.
func FuzzEveryStateSurvivesARestart(f *testing.F) {
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > surviveScriptBytes {
			data = data[:surviveScriptBytes]
		}

		survives(t, data, persist.FormatJSON)
	})
}
