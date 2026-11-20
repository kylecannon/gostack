package kms

import (
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// reasonLock is why a lock is not saved.
const reasonLock = "a lock"

// snapshotPlan says what a snapshot does with each field of an in-memory type:
// saved names the field of the snapshot type that holds it, skipped why it is
// left out. A field in neither fails the test, so a field added to a key, an
// alias, a grant or a State is not dropped on restore until someone decides.
type snapshotPlan struct {
	saved   map[string]string
	skipped map[string]string
}

// savedAs maps each field to the snapshot field of the same name with its first
// letter upper case, then adds renamed.
func savedAs(renamed map[string]string, fields ...string) map[string]string {
	out := maps.Clone(renamed)
	if out == nil {
		out = map[string]string{}
	}

	for _, f := range fields {
		out[f] = strings.ToUpper(f[:1]) + f[1:]
	}

	return out
}

func (plan snapshotPlan) check(t *testing.T, memory, data reflect.Type) {
	t.Helper()

	for field := range memory.Fields() {
		to, isSaved := plan.saved[field.Name]
		reason, isSkipped := plan.skipped[field.Name]

		switch {
		case isSaved:
			_, ok := data.FieldByName(to)
			assert.True(t, ok, "%s.%s is saved as %s.%s, which does not exist", memory.Name(), field.Name, data.Name(), to)
		case isSkipped:
			assert.NotEmpty(t, reason, "%s.%s is skipped without a reason", memory.Name(), field.Name)
		default:
			t.Errorf("%s.%s is not in %s and not on the skip list: a snapshot would drop it",
				memory.Name(), field.Name, data.Name())
		}
	}
}

func Test_snapshot_data_covers_every_field_of_a_key(t *testing.T) {
	snapshotPlan{
		saved: savedAs(map[string]string{"internalID": "InternalID"},
			"ID", "ARN", "Created", "Spec", "Usage", "Origin", "MultiRegion", "primary", "Material", "MaterialID",
			"previous", "pending", "PrivateKey", "PublicKey", "expirationModel", "validTo", "description", "state",
			"deletionDate", "manager", "policy", "tags", "rotation", "replicas"),
		skipped: map[string]string{
			"Account": "the account of the State that holds the key",
			"Region":  "the region of the State that holds the key",
			"mu":      reasonLock,
			"enabled": "true exactly when state is Enabled; setState sets it",
		},
	}.check(t, reflect.TypeFor[Key](), reflect.TypeFor[keyData]())
}

func Test_snapshot_data_covers_every_field_of_a_state(t *testing.T) {
	snapshotPlan{
		saved: savedAs(nil, "account", "region", "keys", "aliases", "imports", "grants"),
		skipped: map[string]string{
			"mu":         reasonLock,
			"keyOrder":   "the order of Keys",
			"aliasOrder": "the order of Aliases",
		},
	}.check(t, reflect.TypeFor[State](), reflect.TypeFor[stateData]())
}

func Test_snapshot_data_covers_every_field_of_the_rest(t *testing.T) {
	snapshotPlan{
		saved: savedAs(nil, "Name", "ARN", "TargetKeyID", "Created", "Updated"),
	}.check(t, reflect.TypeFor[Alias](), reflect.TypeFor[aliasData]())

	snapshotPlan{
		saved: savedAs(nil, "ID", "KeyID", "KeyARN", "Name", "Created", "GranteePrincipal", "RetiringPrincipal",
			"IssuingAccount", "Operations", "Constraints", "Tokens"),
	}.check(t, reflect.TypeFor[Grant](), reflect.TypeFor[grantData]())

	snapshotPlan{
		saved: map[string]string{"grants": "ID"},
		skipped: map[string]string{
			"mu":     reasonLock,
			"order":  "the order of the saved grants",
			"tokens": "made again from each grant's Tokens",
		},
	}.check(t, reflect.TypeFor[grantStore](), reflect.TypeFor[grantData]())

	snapshotPlan{
		saved: savedAs(map[string]string{"keyID": "KeyID"}, "algorithm", "wrapping", "validTo"),
	}.check(t, reflect.TypeFor[importParams](), reflect.TypeFor[importData]())

	snapshotPlan{
		saved: savedAs(nil, "enabled", "period", "nextDate"),
	}.check(t, reflect.TypeFor[rotation](), reflect.TypeFor[rotationData]())

	snapshotPlan{
		saved: savedAs(nil, "ARN", "Region"),
	}.check(t, reflect.TypeFor[regionKey](), reflect.TypeFor[regionKeyData]())
}
