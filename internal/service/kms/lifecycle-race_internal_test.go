package kms

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/require"
)

const (
	opEnable   = "EnableKey"
	opDisable  = "DisableKey"
	opSchedule = "ScheduleKeyDeletion"
	opCancel   = "CancelKeyDeletion"

	codeInvalidState = "KMSInvalidStateException"

	// raceRounds is the number of rounds of the race the review found: a key
	// scheduled for deletion while another call enables or disables it. 33 of
	// 12,000 rounds ended Enabled with a DeletionDate, when the check of the
	// state and the write of the next one were two holds of the key's lock.
	raceRounds = 20000
	// otherRounds is the number of rounds of each of the other pairs.
	otherRounds = 5000
)

// modelBreach is what is wrong with the metadata of a key, by the model of a
// key's lifecycle in key.go, or "" when it holds: the state is one of the four,
// Enabled is true exactly in the Enabled state, and DeletionDate is set exactly
// in the PendingDeletion state.
func modelBreach(meta *api.KeyMetadata) string {
	state, enabled, dated := deref(meta.KeyState), meta.Enabled != nil && *meta.Enabled, meta.DeletionDate != nil

	switch {
	case !slices.Contains([]string{stateEnabled, stateDisabled, statePendingDeletion, statePendingImport}, state):
		return "KeyState " + state
	case enabled != (state == stateEnabled):
		return fmt.Sprintf("KeyState %s with Enabled %t", state, enabled)
	case dated != (state == statePendingDeletion):
		return fmt.Sprintf("KeyState %s with DeletionDate set %t", state, dated)
	default:
		return ""
	}
}

// Test_modelBreach holds the invariant checker to the ways a key can break the
// model, since the race test passes on any key the checker calls sound.
func Test_modelBreach(t *testing.T) {
	yes, no := true, false
	date := &protocol.Time{Time: time.Now()}

	tests := []struct {
		name string
		give api.KeyMetadata
		want bool
	}{
		{"enabled", api.KeyMetadata{KeyState: new(stateEnabled), Enabled: &yes}, false},
		{"disabled", api.KeyMetadata{KeyState: new(stateDisabled), Enabled: &no}, false},
		{"pending import", api.KeyMetadata{KeyState: new(statePendingImport), Enabled: &no}, false},
		{"pending deletion", api.KeyMetadata{KeyState: new(statePendingDeletion), Enabled: &no, DeletionDate: date}, false},
		{"enabled with a date", api.KeyMetadata{KeyState: new(stateEnabled), Enabled: &yes, DeletionDate: date}, true},
		{"disabled yet enabled", api.KeyMetadata{KeyState: new(stateDisabled), Enabled: &yes}, true},
		{"pending deletion without a date", api.KeyMetadata{KeyState: new(statePendingDeletion), Enabled: &no}, true},
		{"a state no one models", api.KeyMetadata{KeyState: new("Creating"), Enabled: &no}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, modelBreach(&tt.give) != "")
		})
	}
}

// race runs two lifecycle operations on one key at the same moment and answers
// their errors, in the order the operations were given.
func race(ctx context.Context, p *Provider, a, b string, id *string) [2]error {
	var (
		wg   sync.WaitGroup
		errs [2]error
	)

	gate := make(chan struct{})

	for i, op := range []string{a, b} {
		wg.Go(func() {
			<-gate

			errs[i] = lifecycleCall(ctx, p, op, id)
		})
	}

	close(gate)
	wg.Wait()

	return errs
}

// lifecycleCall runs one of the four lifecycle operations on a key.
func lifecycleCall(ctx context.Context, p *Provider, op string, id *string) error {
	var err error

	switch op {
	case opEnable:
		_, err = p.EnableKey(ctx, &api.EnableKeyInput{KeyId: id})
	case opDisable:
		_, err = p.DisableKey(ctx, &api.DisableKeyInput{KeyId: id})
	case opSchedule:
		_, err = p.ScheduleKeyDeletion(ctx, &api.ScheduleKeyDeletionInput{KeyId: id})
	case opCancel:
		_, err = p.CancelKeyDeletion(ctx, &api.CancelKeyDeletionInput{KeyId: id})
	default:
		err = fmt.Errorf("no lifecycle operation %s", op)
	}

	return err
}

// Test_lifecycle_race holds two lifecycle calls on one key, started together,
// to the outcomes some order of them allows. When a call checked a key's state
// in one hold of its lock and wrote the next in another, Schedule and Enable
// both answered 200 and the key ended Enabled, or Disabled, with a DeletionDate.
func Test_lifecycle_race(t *testing.T) {
	tests := []struct {
		name       string
		giveOrigin string
		giveFirst  string
		giveSecond string
		giveRounds int
		wantStates []string
		// wantRefused are the operations that must answer a KMSInvalidStateException
		// in every round.
		wantRefused []string
	}{
		{
			// Enable first gives an enabled key, which Schedule then moves; Schedule
			// first leaves Enable refused. Either way the key is pending deletion.
			name: "schedule and enable", giveFirst: opSchedule, giveSecond: opEnable, giveRounds: raceRounds,
			wantStates: []string{statePendingDeletion},
		},
		{
			name: "schedule and disable", giveFirst: opSchedule, giveSecond: opDisable, giveRounds: raceRounds,
			wantStates: []string{statePendingDeletion},
		},
		{
			// Cancel first is refused; Schedule first lets Cancel leave the key disabled.
			name: "schedule and cancel", giveFirst: opSchedule, giveSecond: opCancel, giveRounds: otherRounds,
			wantStates: []string{statePendingDeletion, stateDisabled},
		},
		{
			name: "enable and disable", giveFirst: opEnable, giveSecond: opDisable, giveRounds: otherRounds,
			wantStates: []string{stateEnabled, stateDisabled},
		},
		{
			name: "schedule and enable of an external key", giveOrigin: originExternal,
			giveFirst: opSchedule, giveSecond: opEnable, giveRounds: otherRounds,
			wantStates: []string{statePendingDeletion}, wantRefused: []string{opEnable},
		},
		{
			name: "schedule and disable of an external key", giveOrigin: originExternal,
			giveFirst: opSchedule, giveSecond: opDisable, giveRounds: otherRounds,
			wantStates: []string{statePendingDeletion}, wantRefused: []string{opDisable},
		},
		{
			name: "enable and disable of an external key", giveOrigin: originExternal,
			giveFirst: opEnable, giveSecond: opDisable, giveRounds: otherRounds,
			wantStates: []string{statePendingImport}, wantRefused: []string{opEnable, opDisable},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(nil)
			rc := &request.Context{AccountID: "000000000000", Region: testRegion}
			ctx := request.With(t.Context(), rc)
			odd := map[string]int{}

			for range tt.giveRounds {
				made, err := p.CreateKey(ctx, &api.CreateKeyInput{Origin: new(tt.giveOrigin)})
				require.NoError(t, err)

				id := made.KeyMetadata.KeyId
				errs := race(ctx, p, tt.giveFirst, tt.giveSecond, id)

				described, err := p.DescribeKey(ctx, &api.DescribeKeyInput{KeyId: id})
				require.NoError(t, err)

				meta := described.KeyMetadata
				if breach := modelBreach(meta); breach != "" {
					odd[breach]++
				}

				if state := deref(meta.KeyState); !slices.Contains(tt.wantStates, state) {
					odd["ended "+state]++
				}

				for i, op := range []string{tt.giveFirst, tt.giveSecond} {
					if slices.Contains(tt.wantRefused, op) && !isInvalidState(errs[i]) {
						odd[fmt.Sprintf("%s answered %v", op, errs[i])]++
					}
				}
			}

			require.Empty(t, odd, "outcomes no order of the two calls allows, by number of rounds")
		})
	}
}

func isInvalidState(err error) bool {
	var awsErr *awserr.Error

	return errors.As(err, &awsErr) && awsErr.Code == codeInvalidState
}
