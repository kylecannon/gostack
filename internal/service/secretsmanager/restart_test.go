package secretsmanager_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A restart changes no answer: whatever the API can reach, a snapshot saves it
// and a load reads it back to the same answers. The property test drives random
// calls, resets included, through the gateway, restarts the engine on the file
// it wrote, and compares every read before and after. The named tests below it
// are the cases the property test found, one each.

const (
	// bypass is the tag that lets CreateSecret skip its name check.
	bypass = "BYPASS_SECRET_ID_VALIDATION"
	// restartSteps is how many calls one run makes before the restart.
	restartSteps = 80
	// restartSeedsDefault is how many runs a mix makes per format. The
	// SM_RESTART_SEEDS variable raises it for a longer search.
	restartSeedsDefault = 8
	// firstAccount is the account a call is made as when it names none.
	firstAccount = "000000000000"
	// restartTokens is how many distinct client tokens the generator makes.
	restartTokens = 5
	// arnSuffix is the length of the random suffix of an ARN, with its dash.
	arnSuffix = 7
	// maxARNs is how many ARNs the generator keeps to name secrets by.
	maxARNs = 12

	// regionAP is a third region, next to primaryRegion and replicaRegion.
	regionAP = "ap-southeast-2"

	stagePrevious = "AWSPREVIOUS"
	stagePending  = "AWSPENDING"
	stageCustom   = "custom"

	// The members of the requests the tests make.
	keySecretID = "SecretId"
	keyToken    = "ClientRequestToken"
	keyReplicas = "AddReplicaRegions"
	keyRegion   = "Region"
	keyString   = "SecretString"
	keyTagKey   = "Key"
	keyTagValue = "Value"
)

var (
	restartAccounts = []string{firstAccount, "111111111111"}
	restartRegions  = []string{primaryRegion, replicaRegion, regionAP}
	restartStages   = []string{awsCurrent, stagePrevious, stagePending, stageCustom, "other"}
	// plainNames are valid names, few, so that calls meet each other's secrets.
	plainNames = []string{"s1", "s2", "s1-AbCdEf", "a/b", "db"}
	// arnLikeNames start with arn:. Each is checked by nothing but its prefix, and
	// each is either another service's ARN, malformed, or a partial Secrets
	// Manager ARN of a plain name.
	arnLikeNames = []string{
		"arn:", "arn:x", "arn:aws:sqs:x:y:p-odd", "arn:aws:sqs:eu-west-1:111111111111:q",
		"arn:aws:secretsmanager:s1", "arn:aws:secretsmanager:s2",
		"arn:aws:secretsmanager:us-east-1:000000000000:secret:s1",
		"arn:aws:secretsmanager:eu-west-1:000000000000:secret:s2",
	}
)

// restartPlace is the account and region of a call.
type restartPlace struct{ account, region string }

// restartMix is what a run may do beyond the plain calls. A test names the
// ingredients of the case it covers.
type restartMix struct {
	// emptyName adds the empty name, which the bypass tag lets CreateSecret store.
	emptyName bool
	// arnNames adds the names that start with arn:, and the ARN of another secret.
	arnNames bool
	// resets adds the reset of one account and region between calls.
	resets bool
}

// restartDriver makes random calls and keeps what it needs to aim later ones.
type restartDriver struct {
	t     *testing.T
	h     http.Handler
	reset func(account, region string)
	rng   *rand.Rand
	mix   restartMix
	// arns and versions are what answers have returned so far, for calls that
	// name a secret by ARN or a version by id.
	arns     []string
	versions []string
	log      []string
}

// sendAs makes one call as place and returns the status and the body with its
// keys in order.
func sendAs(t *testing.T, h http.Handler, at restartPlace, operation string, body any) (int, string) {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager."+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+at.account+"/20130524/"+at.region+
		"/secretsmanager/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	answer, err := io.ReadAll(rec.Result().Body)
	require.NoError(t, err)

	var decoded any
	if json.Unmarshal(answer, &decoded) != nil {
		return rec.Code, string(answer)
	}

	again, err := json.Marshal(decoded)
	require.NoError(t, err)

	return rec.Code, string(again)
}

func (d *restartDriver) pick(from []string) string {
	return from[d.rng.IntN(len(from))]
}

// chance is true one time in n.
func (d *restartDriver) chance(n int) bool {
	return d.rng.IntN(n) == 0
}

func (d *restartDriver) place() restartPlace {
	account := firstAccount
	if d.chance(4) {
		account = d.pick(restartAccounts)
	}

	return restartPlace{account: account, region: d.pick(restartRegions)}
}

// name is a name for CreateSecret.
func (d *restartDriver) name() string {
	names := plainNames
	if d.mix.arnNames {
		names = slices.Concat(plainNames[:2], arnLikeNames, d.arns)
	}

	if d.mix.emptyName {
		names = append(slices.Clone(names), "", "")
	}

	return d.pick(names)
}

// secretID names a secret for any other call: a name, an ARN seen so far, or
// such an ARN without its suffix.
func (d *restartDriver) secretID() string {
	if len(d.arns) > 0 && d.chance(3) {
		arn := d.pick(d.arns)
		if d.chance(3) && len(arn) > arnSuffix {
			return arn[:len(arn)-arnSuffix]
		}

		return arn
	}

	names := plainNames
	if d.mix.arnNames {
		names = slices.Concat(plainNames[:2], arnLikeNames)
	}

	return d.pick(names)
}

func (d *restartDriver) token() string {
	if d.chance(restartTokens) {
		return ""
	}

	n := d.rng.IntN(restartTokens)

	return strings.Repeat(strconv.Itoa(n+1), 8) + "-1111-1111-1111-111111111111"
}

func (d *restartDriver) versionID() string {
	if len(d.versions) > 0 && !d.chance(5) {
		return d.pick(d.versions)
	}

	return d.token()
}

func (d *restartDriver) addValue(body map[string]any) {
	switch d.rng.IntN(5) {
	case 0:
		body[keyString] = "v" + strconv.Itoa(d.rng.IntN(3))
	case 1:
		body["SecretBinary"] = "AAH/"
	case 2:
		body[keyString], body["SecretBinary"] = "x", ""
	case 3:
	default:
		body[keyString] = "w" + strconv.Itoa(d.rng.IntN(3))
	}
}

// replicas is AddReplicaRegions, one or two regions.
func (d *restartDriver) replicas() []map[string]string {
	if d.chance(3) {
		return regionsOf(d.pick(restartRegions), d.pick(restartRegions))
	}

	return regionsOf(d.pick(restartRegions))
}

// regionsOf is an AddReplicaRegions list.
func regionsOf(regions ...string) []map[string]string {
	out := make([]map[string]string, 0, len(regions))
	for _, region := range regions {
		out = append(out, map[string]string{keyRegion: region})
	}

	return out
}

// tagList is a Tags list of one tag.
func tagList(key, value string) []map[string]string {
	return []map[string]string{{keyTagKey: key, keyTagValue: value}}
}

// do makes a call and notes the ARNs and version ids it returns.
func (d *restartDriver) do(at restartPlace, operation string, body map[string]any) {
	d.t.Helper()

	code, answer := sendAs(d.t, d.h, at, operation, body)
	d.log = append(d.log, fmt.Sprintf("%s %s %s %s -> %d %.200s",
		at.account, at.region, operation, mustJSON(body), code, answer))

	assert.False(d.t, code >= http.StatusInternalServerError && code != http.StatusNotImplemented,
		"%s %v: %d %s", operation, body, code, answer)

	var out map[string]any

	if json.Unmarshal([]byte(answer), &out) != nil || code != http.StatusOK {
		return
	}

	if arn, ok := out["ARN"].(string); ok && !slices.Contains(d.arns, arn) && len(d.arns) < maxARNs {
		d.arns = append(d.arns, arn)
	}

	if id, ok := out["VersionId"].(string); ok && !slices.Contains(d.versions, id) {
		d.versions = append(d.versions, id)
	}
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}

	return string(raw)
}

// restartOps are the calls a run draws from; one listed twice is drawn twice as often.
var restartOps = []func(d *restartDriver, at restartPlace){
	createOp, createOp, createOp, createOp, putValueOp, putValueOp, updateOp, stageOp, stageOp, stageOp,
	deleteOp, deleteOp, restoreOp, tagOp, untagOp, policyOp, rotateOp, replicateOp, replicateOp, unreplicateOp,
	readOp, resetOp,
}

func createOp(d *restartDriver, at restartPlace) {
	name := d.name()
	body := map[string]any{"Name": name, keyToken: d.token()}
	d.addValue(body)

	switch {
	case name == "" || d.chance(6):
		body["Tags"] = tagList(bypass, "1")
	case d.chance(5):
		body["Tags"] = tagList("k", "v")
	case d.chance(3):
		body[keyReplicas] = d.replicas()
		body["ForceOverwriteReplicaSecret"] = d.chance(2)
	case d.chance(4):
		body["KmsKeyId"], body["Description"] = "alias/x", "d"
	default:
		// No more members.
	}

	d.do(at, "CreateSecret", body)
}

func putValueOp(d *restartDriver, at restartPlace) {
	body := map[string]any{keySecretID: d.secretID(), keyToken: d.versionID()}
	d.addValue(body)

	switch d.rng.IntN(5) {
	case 0:
		body["VersionStages"] = []string{d.pick(restartStages)}
	case 1:
		body["VersionStages"] = []string{d.pick(restartStages), d.pick(restartStages)}
	case 2:
		body["VersionStages"] = []string{}
	default:
		// The default stage.
	}

	d.do(at, "PutSecretValue", body)
}

func updateOp(d *restartDriver, at restartPlace) {
	body := map[string]any{keySecretID: d.secretID(), keyToken: d.versionID()}
	d.addValue(body)

	if d.chance(3) {
		body["Description"], body["KmsKeyId"] = "d2", "alias/y"
	}

	d.do(at, "UpdateSecret", body)
}

func stageOp(d *restartDriver, at restartPlace) {
	body := map[string]any{keySecretID: d.secretID(), "VersionStage": d.pick(restartStages)}
	if !d.chance(3) {
		body["RemoveFromVersionId"] = d.versionID()
	}

	if !d.chance(3) {
		body["MoveToVersionId"] = d.versionID()
	}

	d.do(at, "UpdateSecretVersionStage", body)
}

func deleteOp(d *restartDriver, at restartPlace) {
	body := map[string]any{keySecretID: d.secretID()}

	switch d.rng.IntN(4) {
	case 0:
		body["ForceDeleteWithoutRecovery"] = true
	case 1:
		body["RecoveryWindowInDays"] = 7
	default:
		// A scheduled deletion with the default window.
	}

	d.do(at, "DeleteSecret", body)
}

func restoreOp(d *restartDriver, at restartPlace) {
	d.do(at, "RestoreSecret", map[string]any{keySecretID: d.secretID()})
}

func tagOp(d *restartDriver, at restartPlace) {
	tags := tagList("t"+strconv.Itoa(d.rng.IntN(2)), "v")
	d.do(at, "TagResource", map[string]any{keySecretID: d.secretID(), "Tags": tags})
}

func untagOp(d *restartDriver, at restartPlace) {
	d.do(at, "UntagResource", map[string]any{keySecretID: d.secretID(), "TagKeys": []string{"t0", "t1", "k"}})
}

func policyOp(d *restartDriver, at restartPlace) {
	if d.chance(2) {
		d.do(at, "PutResourcePolicy", map[string]any{keySecretID: d.secretID(), "ResourcePolicy": "{}"})

		return
	}

	d.do(at, "DeleteResourcePolicy", map[string]any{keySecretID: d.secretID()})
}

func rotateOp(d *restartDriver, at restartPlace) {
	id := d.secretID()
	body := map[string]any{keySecretID: id, keyToken: d.versionID(), "RotationLambdaARN": functionARN}

	if d.chance(2) {
		body["RotationRules"] = map[string]any{"AutomaticallyAfterDays": 3}
	}

	if d.chance(3) {
		body["RotateImmediately"] = false
	}

	d.do(at, "RotateSecret", body)

	if d.chance(2) {
		d.do(at, "CancelRotateSecret", map[string]any{keySecretID: id})
	}
}

func replicateOp(d *restartDriver, at restartPlace) {
	body := map[string]any{keySecretID: d.secretID(), keyReplicas: d.replicas()}
	if d.chance(2) {
		body["ForceOverwriteReplicaSecret"] = true
	}

	d.do(at, "ReplicateSecretToRegions", body)
}

func unreplicateOp(d *restartDriver, at restartPlace) {
	d.do(at, "RemoveRegionsFromReplication",
		map[string]any{keySecretID: d.secretID(), "RemoveReplicaRegions": []string{d.pick(restartRegions)}})
}

func readOp(d *restartDriver, at restartPlace) {
	d.do(at, "GetSecretValue", map[string]any{keySecretID: d.secretID()})
}

// resetOp empties one account and region, as the admin endpoint does, when the
// mix has resets.
func resetOp(d *restartDriver, at restartPlace) {
	if !d.mix.resets {
		return
	}

	d.reset(at.account, at.region)
	d.log = append(d.log, fmt.Sprintf("RESET %s %s", at.account, at.region))
}

// ids is every SecretId a dump reads: the names the run could use, and the
// ARNs it saw, whole and without their suffix.
func (d *restartDriver) ids() []string {
	ids := slices.Clone(plainNames)
	if d.mix.arnNames {
		ids = slices.Concat(ids, arnLikeNames)
	}

	for _, arn := range d.arns {
		ids = append(ids, arn)

		if len(arn) > arnSuffix {
			ids = append(ids, arn[:len(arn)-arnSuffix])
		}
	}

	return ids
}

// dump reads everything every account and region answers, keyed by the call.
// A list is sorted: a replica record is made again after the secrets of its
// region, so it may move within the list.
func (d *restartDriver) dump() map[string]string {
	out := map[string]string{}

	read := func(at restartPlace, operation string, body map[string]any) string {
		code, answer := sendAs(d.t, d.h, at, operation, body)
		call := fmt.Sprintf("%s %s %s %s", at.account, at.region, operation, mustJSON(body))
		out[call] = fmt.Sprintf("%d %s", code, answer)

		return answer
	}

	for _, account := range restartAccounts {
		for _, region := range restartRegions {
			at := restartPlace{account: account, region: region}
			out[fmt.Sprintf("%s %s ListSecrets", account, region)] = d.listed(at)

			for _, id := range d.ids() {
				read(at, "DescribeSecret", map[string]any{keySecretID: id})
				read(at, "GetResourcePolicy", map[string]any{keySecretID: id})

				versions := read(at, "ListSecretVersionIds", map[string]any{keySecretID: id, "IncludeDeprecated": true})
				for _, v := range versionIDsIn(versions) {
					read(at, "GetSecretValue", map[string]any{keySecretID: id, "VersionId": v})
				}

				for _, stage := range []string{"", stagePrevious, stagePending, stageCustom} {
					body := map[string]any{keySecretID: id}
					if stage != "" {
						body["VersionStage"] = stage
					}

					read(at, "GetSecretValue", body)
				}
			}
		}
	}

	return out
}

// listed is ListSecrets with its entries sorted.
func (d *restartDriver) listed(at restartPlace) string {
	code, answer := sendAs(d.t, d.h, at, "ListSecrets", map[string]any{"IncludePlannedDeletion": true})

	var out struct {
		SecretList []any `json:"SecretList"`
	}

	if json.Unmarshal([]byte(answer), &out) != nil {
		return fmt.Sprintf("%d %s", code, answer)
	}

	entries := make([]string, 0, len(out.SecretList))
	for _, entry := range out.SecretList {
		entries = append(entries, mustJSON(entry))
	}

	slices.Sort(entries)

	return fmt.Sprintf("%d %s", code, strings.Join(entries, "\n  "))
}

// versionList is the part of a ListSecretVersionIds answer a dump follows.
type versionList struct {
	Versions []versionEntry `json:"Versions"`
}

type versionEntry struct {
	ID string `json:"VersionId"`
}

// versionIDsIn is the version ids of a ListSecretVersionIds answer.
func versionIDsIn(answer string) []string {
	var out versionList

	_ = json.Unmarshal([]byte(answer), &out)

	ids := make([]string, 0, len(out.Versions))
	for _, v := range out.Versions {
		ids = append(ids, v.ID)
	}

	return ids
}

// runRestart makes restartSteps random calls, flushes, restarts on the file,
// and reports every read that answers differently. It returns false on the
// first run that does.
func runRestart(t *testing.T, seed uint64, format persist.Format, mix restartMix) bool {
	t.Helper()

	dir := t.TempDir()
	eng, p, h := startProvider(t, dir, format)
	d := &restartDriver{
		t: t, h: h, reset: p.Registration().Reset, mix: mix,
		rng: rand.New(rand.NewPCG(seed, seed)),
	}

	for range restartSteps {
		restartOps[d.rng.IntN(len(restartOps))](d, d.place())
	}

	eng.MarkDirty(serviceName)
	require.NoError(t, eng.Flush(t.Context()))

	before := d.dump()

	_, _, d.h = startProvider(t, dir, format)
	after := d.dump()

	var differ []string

	for _, call := range slices.Sorted(maps.Keys(before)) {
		if now, ok := after[call]; !ok || now != before[call] {
			differ = append(differ, "call:   "+call+"\nbefore: "+before[call]+"\nafter:  "+now)
		}
	}

	for _, call := range slices.Sorted(maps.Keys(after)) {
		if _, ok := before[call]; !ok {
			differ = append(differ, "call:   "+call+"\nbefore: (not made)\nafter:  "+after[call])
		}
	}

	if len(differ) == 0 {
		return true
	}

	t.Errorf("seed %d, format %s: %d reads differ after the restart; first:\n%s\n--- calls ---\n%s",
		seed, format, len(differ), strings.Join(differ[:min(len(differ), 3)], "\n"), strings.Join(d.log, "\n"))

	return false
}

// restartSeeds is how many runs a mix makes per format.
func restartSeeds(t *testing.T) uint64 {
	t.Helper()

	if testing.Short() {
		return 3
	}

	if v := os.Getenv("SM_RESTART_SEEDS"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		require.NoError(t, err)

		return n
	}

	return restartSeedsDefault
}

func Test_a_restart_changes_no_answer(t *testing.T) {
	mixes := map[string]restartMix{
		"plain":       {},
		"empty names": {emptyName: true},
		"resets":      {resets: true},
		"arn names":   {arnNames: true},
		"everything":  {emptyName: true, arnNames: true, resets: true},
	}

	formats := []persist.Format{persist.FormatJSON, persist.FormatBinary}

	for name, mix := range mixes {
		t.Run(name, func(t *testing.T) {
			// The runs take the two formats in turn.
			for seed := range restartSeeds(t) {
				if !runRestart(t, seed, formats[seed%uint64(len(formats))], mix) {
					return
				}
			}
		})
	}
}
