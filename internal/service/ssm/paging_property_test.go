package ssm_test

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

// The property test of the ordered store and its cursors, from the second
// review of SSM (SQ-277). Random puts, overwrites, deletes, tag and label
// changes run against two providers that get the same writes; between them,
// six sessions page through listings, and every page is checked twice:
//
//   - against a replica that has a refused delete before every page, which
//     drops its cursors, so it cuts each page by walking from the start, as
//     moto does from a list it builds for every call;
//   - for the listings the model below can state, against a plain list of the
//     parameters in creation order: the page is the slice at the offset, and
//     the token is the offset plus the page when the page is full.

const (
	propLabelFilter = 3
	propPathA       = "/a/"
	propPathAB      = "/a/b"
	propTagKey      = "k"
	propLabel       = "l1"
	propNever       = "/zz/never"
	propSessions    = 6
	propReplays     = 60
	propNoToken     = ""
)

// propParam is what the model knows of a parameter.
type propParam struct {
	name  string
	typ   string
	tag   bool
	label bool
}

// propModel is the parameters in creation order.
type propModel struct {
	order []string
	byKey map[string]*propParam
}

// propKey is the key moto keeps a name under.
func propKey(name string) string {
	stripped := strings.TrimPrefix(name, "/")
	if strings.Contains(stripped, "/") {
		return name
	}

	return stripped
}

// propUnder is whether a key is under a path: one level down, or any level.
func propUnder(key, path string, recursive bool) bool {
	if path != "/" && !strings.HasPrefix(key, path) {
		return false
	}

	return recursive || len(key) <= len(path)+1 || !strings.Contains(key[len(path)+1:], "/")
}

func (m *propModel) remove(key string) {
	if m.byKey[key] == nil {
		return
	}

	delete(m.byKey, key)
	m.order = slices.DeleteFunc(m.order, func(k string) bool { return k == key })
}

// propQuery is a listing: the request, and the parameters the model says it
// matches, nil when the model does not say.
type propQuery struct {
	desc  string
	op    string
	body  obj
	model func(key string, p *propParam) bool
}

// propNameOf is the name of parameter i, spread over four paths.
func propNameOf(i int) string {
	groups := []string{propPathA, propPathAB + "/", "/c/", ""}

	return groups[i%len(groups)] + "p" + strconv.Itoa(i)
}

// propByPath draws a GetParametersByPath listing with the filter numbered
// filter: 1 is Type, 2 a tag, 3 Label, and none for any other.
func propByPath(r *rand.Rand, filter int) propQuery {
	typeFilter := obj{fieldKey: kType, fieldOption: "Equals", kValues: []string{kStringList}}
	tagFilter := obj{fieldKey: "tag:" + propTagKey}
	labelFilter := obj{fieldKey: "Label", kValues: []string{propLabel}}

	paths := []string{"/", "/a", propPathAB, "/c", propPathA}
	path := paths[r.IntN(len(paths))]
	recursive := r.IntN(2) == 0
	norm := strings.TrimRight(path, "/") + "/"
	body := obj{kPath: path, kRecursive: recursive}
	desc := "P " + path + " " + strconv.FormatBool(recursive)

	var extra func(p *propParam) bool

	switch filter {
	case 1:
		body[fieldFilters] = []obj{typeFilter}
		extra = func(p *propParam) bool { return p.typ == kStringList }
		desc += " type"
	case 2:
		body[fieldFilters] = []obj{tagFilter}
		extra = func(p *propParam) bool { return p.tag }
		desc += " tag"
	case 3:
		body[fieldFilters] = []obj{labelFilter}
		extra = func(p *propParam) bool { return p.label }
		desc += " label"
	default:
		desc += " all"
	}

	return propQuery{desc, kGetParametersByPath, body, func(key string, p *propParam) bool {
		return propUnder(key, norm, recursive) && (extra == nil || extra(p))
	}}
}

// propDescribe draws a DescribeParameters listing.
func propDescribe(r *rand.Rand) propQuery {
	typeFilter := obj{fieldKey: kType, fieldOption: "Equals", kValues: []string{kStringList}}
	tagFilter := obj{fieldKey: "tag:" + propTagKey}
	nameFilter := obj{fieldKey: kName, fieldOption: "BeginsWith", kValues: []string{"/a"}}
	everyone := func(string, *propParam) bool { return true }

	switch r.IntN(7) {
	case 0:
		return propQuery{
			"D type", kDescribeParameters,
			obj{fieldFilters: []obj{typeFilter}},
			func(_ string, p *propParam) bool { return p.typ == kStringList },
		}
	case 1:
		return propQuery{
			"D tag", kDescribeParameters,
			obj{fieldFilters: []obj{tagFilter}},
			func(_ string, p *propParam) bool { return p.tag },
		}
	case 2:
		return propQuery{
			"D tier", kDescribeParameters,
			obj{fieldFilters: []obj{{fieldKey: kTier, kValues: []string{kStandard}}}},
			everyone,
		}
	case 3:
		return propQuery{
			"D name", kDescribeParameters,
			obj{fieldFilters: []obj{nameFilter}},
			func(_ string, p *propParam) bool { return strings.HasPrefix("/"+strings.TrimLeft(p.name, "/"), "/a") },
		}
	case 4:
		return propQuery{
			"D path", kDescribeParameters,
			obj{fieldFilters: []obj{{fieldKey: kPath, fieldOption: kRecursive, kValues: []string{"/a"}}}},
			nil,
		}
	case 5:
		return propQuery{
			"D legacy", kDescribeParameters,
			obj{kFilters: []obj{{fieldKey: kName, kValues: []string{propPathAB}}}},
			nil,
		}
	}

	return propQuery{"D all", kDescribeParameters, obj{}, everyone}
}

// query draws a listing of either kind. In the puts-only mix it is a
// GetParametersByPath listing with the Label filter: a put that overwrites
// takes a label away, so that filter is the one a put can change the matches
// of.
func (m propMix) query(r *rand.Rand) propQuery {
	switch {
	case m.putsOnly:
		return propByPath(r, propLabelFilter)
	case r.IntN(2) == 0:
		return propByPath(r, r.IntN(4))
	default:
		return propDescribe(r)
	}
}

// propPage is one answer of a listing.
type propPage struct {
	code  int
	names []string
	token string
	raw   string
}

// pagedName and pagedAnswer are the members of a page the paging tests read.
type pagedName struct {
	Name string `json:"Name"`
}

type pagedAnswer struct {
	Parameters []pagedName `json:"Parameters"`
	NextToken  string      `json:"NextToken"`
}

func propPageOf(d direct, op string, body obj) propPage {
	code, out := d.do(op, body)

	var resp pagedAnswer

	_ = json.Unmarshal(out, &resp)

	names := make([]string, 0, len(resp.Parameters))
	for _, p := range resp.Parameters {
		names = append(names, p.Name)
	}

	return propPage{code, names, resp.NextToken, string(out)}
}

// propSession is a caller paging through a listing.
type propSession struct {
	q     propQuery
	token string
}

// propRun is one seed: the two providers, the model, and the sessions.
type propRun struct {
	t        *testing.T
	seed     int
	mix      propMix
	r        *rand.Rand
	primary  direct
	replica  direct
	model    *propModel
	pool     int
	sessions []*propSession
}

// propMix is what a run does with its steps.
type propMix struct {
	name string
	// seeds is how many runs of 3,000 steps the suite makes.
	seeds int
	// heavy makes nine steps in ten a page.
	heavy bool
	// putsOnly makes the steps pages, puts and a few labels, and no other write.
	putsOnly bool
}

func newPropRun(t *testing.T, seed int, mix propMix) *propRun {
	t.Helper()

	r := rand.New(rand.NewPCG(uint64(seed), 277))
	prim, repl := ssm.New(), ssm.New()
	run := &propRun{
		t: t, seed: seed, mix: mix, r: r,
		primary: direct{tb: t, h: handlerFor(t, prim, nil)},
		replica: direct{tb: t, h: handlerFor(t, repl, nil)},
		model:   &propModel{byKey: map[string]*propParam{}},
		pool:    40 + r.IntN(200),
	}

	for range propSessions {
		run.sessions = append(run.sessions, &propSession{q: mix.query(r)})
	}

	return run
}

// both sends a write to both providers and requires the same status.
func (run *propRun) both(op string, body obj) int {
	run.t.Helper()

	a, _ := run.primary.do(op, body)
	b, _ := run.replica.do(op, body)
	require.Equal(run.t, a, b, "seed %d: %s %v: primary %d replica %d", run.seed, op, body, a, b)

	return a
}

// existing is the name of a parameter the model has, and the parameter.
func (run *propRun) existing() (string, *propParam) {
	if len(run.model.order) == 0 {
		return "", nil
	}

	p := run.model.byKey[run.model.order[run.r.IntN(len(run.model.order))]]

	return p.name, p
}

// existingOrNot is existing, now and then replaced by a name the model may not have.
func (run *propRun) existingOrNot(oneIn int) string {
	name, p := run.existing()
	if p == nil || run.r.IntN(oneIn) == 0 {
		name = propNameOf(run.r.IntN(run.pool))
	}

	return name
}

// put makes a parameter, or writes over one, or is refused.
func (run *propRun) put(step int) {
	r := run.r

	// A fourth of the puts are on a parameter the model has, which is often
	// one with a label, that an overwrite takes away.
	if r.IntN(4) == 0 {
		if name, p := run.existing(); p != nil {
			run.overwrite(name, p, step)

			return
		}
	}

	i := r.IntN(run.pool)
	name := propNameOf(i)

	if i%4 == 3 && r.IntN(2) == 0 {
		name = "/" + name
	}

	key := propKey(name)
	if p := run.model.byKey[key]; p != nil {
		run.overwrite(name, p, step)

		return
	}

	typ := []string{kString, kStringList}[r.IntN(2)]
	body := obj{kName: name, kValue: "v", kType: typ}
	tag := r.IntN(3) == 0

	if tag {
		body[kTags] = []obj{{fieldKey: propTagKey, kValue: "v"}}
	}

	require.Equal(run.t, 200, run.both(kPutParameter, body), "seed %d put %v", run.seed, body)

	made := &propParam{name: strings.TrimPrefix(name, "/"), typ: typ, tag: tag}
	if strings.Count(name, "/") > 1 {
		made.name = name
	}

	run.model.byKey[key] = made
	run.model.order = append(run.model.order, key)
}

// overwrite puts to a parameter the model has: a fifth of the time without
// Overwrite, which is refused, and otherwise with, which makes a version that
// has none of the labels.
func (run *propRun) overwrite(name string, p *propParam, step int) {
	if run.r.IntN(5) == 0 {
		run.both(kPutParameter, obj{kName: name, kValue: "x", kType: kString})

		return
	}

	code := run.both(kPutParameter, obj{kName: name, kValue: "v" + strconv.Itoa(step), kOverwrite: true})
	if code == 200 {
		p.label = false
	}
}

func (run *propRun) deleteOne() {
	name := run.existingOrNot(10)
	if run.both(kDeleteParameter, obj{kName: name}) == 200 {
		run.model.remove(propKey(name))
	}
}

func (run *propRun) deleteMany() {
	names := make([]string, 0, 5)

	for range 1 + run.r.IntN(5) {
		names = append(names, run.existingOrNot(4))
	}

	run.both(kDeleteParameters, obj{fieldNames: names})

	for _, n := range names {
		run.model.remove(propKey(n))
	}
}

func (run *propRun) tag() {
	name, p := run.existing()
	if p == nil {
		return
	}

	if run.r.IntN(2) == 0 {
		run.both(kAddTagsToResource, obj{
			kResourceType: resourceParameter, kResourceID: name, kTags: []obj{{fieldKey: propTagKey, kValue: "v"}},
		})

		p.tag = true

		return
	}

	run.both(kRemoveTagsFromResource, obj{
		kResourceType: resourceParameter, kResourceID: name, fieldTagKeys: []string{propTagKey},
	})

	p.tag = false
}

func (run *propRun) label() {
	name, p := run.existing()
	if p == nil {
		return
	}

	run.both(kLabelParameterVersion, obj{kName: name, fieldLabels: []string{propLabel}})

	p.label = true
}

// page reads one page for a session from both providers, and checks it.
func (run *propRun) page() {
	r := run.r
	s := run.sessions[r.IntN(len(run.sessions))]
	token := s.token
	replay := r.IntN(15) == 0

	if replay {
		token = strconv.Itoa(r.IntN(propReplays))
	}

	body := with(s.q.body, fieldMaxResults, 1+r.IntN(10))
	if token != propNoToken {
		body = with(body, fieldNextToken, token)
	}

	got := propPageOf(run.primary, s.q.op, body)

	// A refused delete: the replica's cursors are dropped, so it walks from the start.
	run.replica.do(kDeleteParameter, obj{kName: propNever})

	want := propPageOf(run.replica, s.q.op, body)

	// Not the raw answers: the two providers made their parameters at different times.
	require.Equal(run.t, want.code, got.code, "seed %d %s token %q: %s", run.seed, s.q.desc, token, got.raw)
	require.Equal(run.t, want.names, got.names, "seed %d %s token %q: cursor against a walk from the start",
		run.seed, s.q.desc, token)
	require.Equal(run.t, want.token, got.token, "seed %d %s token %q: cursor against a walk from the start",
		run.seed, s.q.desc, token)
	require.Equal(run.t, 200, got.code, "seed %d %s: %s", run.seed, s.q.desc, got.raw)

	run.checkModel(s.q, body, token, got)

	if !replay {
		s.token = got.token
		if s.token == propNoToken {
			s.q = run.mix.query(r)
		}
	}
}

// checkModel checks a page against the list of parameters in creation order.
func (run *propRun) checkModel(q propQuery, body obj, token string, got propPage) {
	if q.model == nil {
		return
	}

	var all []string

	for _, key := range run.model.order {
		if p := run.model.byKey[key]; q.model(key, p) {
			all = append(all, p.name)
		}
	}

	offset, _ := strconv.Atoi(token)
	limit := 10

	if q.op == kGetParametersByPath {
		limit, _ = body[fieldMaxResults].(int)
	}

	want := []string{}
	if offset < len(all) {
		want = all[offset:min(offset+limit, len(all))]
	}

	wantToken := propNoToken
	if len(want) == limit {
		wantToken = strconv.Itoa(offset + limit)
	}

	require.Equal(run.t, want, got.names, "seed %d %s token %q limit %d", run.seed, q.desc, token, limit)
	require.Equal(run.t, wantToken, got.token, "seed %d %s token %q limit %d", run.seed, q.desc, token, limit)
}

// step does one random thing.
func (run *propRun) step(step int) {
	k := run.r.IntN(100)
	if run.mix.heavy && run.r.IntN(10) != 0 {
		k = 99
	}

	if run.mix.putsOnly {
		// Nothing but puts and a few labels between the pages, so a cursor
		// lives long enough to be used after a put, which is the only write
		// that may leave it in place.
		switch {
		case k < 15:
			run.put(step)
		case k < 17:
			run.label()
		default:
			run.page()
		}

		return
	}

	switch {
	case k < 30:
		run.put(step)
	case k < 45:
		run.deleteOne()
	case k < 50:
		run.deleteMany()
	case k < 55:
		run.tag()
	case k < 60:
		run.label()
	default:
		run.page()
	}
}

// propSeedsEnv scales the number of seeds of the test below, for a longer run
// than the suite's: GOSTACK_SSM_PAGING_SEEDS=5 runs five times as many.
const propSeedsEnv = "GOSTACK_SSM_PAGING_SEEDS"

// propSteps is how many steps a run takes.
const propSteps = 3000

// Test_pagesMatchAModelAndAWalkFromTheStart: see the comment at the top of the
// file. The default mix is a third writes; the page-heavy mix is nine pages in
// ten steps, so most pages are cut from a cursor; the third has no write but a
// put, so the cursors a put keeps are the ones that are used. The review ran
// 20 seeds of the default mix and 6 of the page-heavy one, which take 30 s
// under the race detector; the suite runs a fifth of that.
func Test_pagesMatchAModelAndAWalkFromTheStart(t *testing.T) {
	scale, err := strconv.Atoi(os.Getenv(propSeedsEnv))
	if err != nil || scale < 1 {
		scale = 1
	}

	for _, mix := range []propMix{
		{name: "default mix", seeds: 4 * scale},
		{name: "page-heavy mix", seeds: 2 * scale, heavy: true},
		{name: "puts and pages", seeds: 2 * scale, putsOnly: true},
	} {
		t.Run(mix.name, func(t *testing.T) {
			for seed := range mix.seeds {
				run := newPropRun(t, seed, mix)

				for step := range propSteps {
					run.step(step)
				}
			}
		})
	}
}
