package sqs

import (
	"reflect"
	"testing"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fieldPlan says what a snapshot does with each field of an in-memory type:
// saved maps a field to the field of the snapshot type that holds it, and
// skipped maps a field to the reason it is left out. A field of the in-memory
// type that is in neither fails the test, which is the point: a field added to
// message, entry or fifoState is dropped on restore until someone decides.
type fieldPlan struct {
	saved   map[string]string
	skipped map[string]string
	// converted names the saved fields whose snapshot field has another type,
	// because the snapshot holds a name or a nested snapshot type for them.
	converted map[string]bool
}

// check walks every field of memory, exported or not, and every field of data
// that the plan names.
func (plan fieldPlan) check(t *testing.T, memory, data reflect.Type) {
	t.Helper()

	seen := make(map[string]bool, memory.NumField())

	for field := range memory.Fields() {
		seen[field.Name] = true

		to, isSaved := plan.saved[field.Name]
		reason, isSkipped := plan.skipped[field.Name]

		switch {
		case isSaved && isSkipped:
			t.Errorf("%s.%s is both saved and skipped", memory.Name(), field.Name)
		case isSkipped:
			assert.NotEmpty(t, reason, "%s.%s is skipped without a reason", memory.Name(), field.Name)
		case isSaved:
			held, ok := data.FieldByName(to)
			if !assert.True(t, ok, "%s.%s is saved as %s.%s, which does not exist", memory.Name(), field.Name, data.Name(), to) {
				continue
			}

			if !plan.converted[field.Name] {
				assert.Equal(t, field.Type, held.Type,
					"%s.%s and %s.%s hold different types", memory.Name(), field.Name, data.Name(), to)
			}
		default:
			t.Errorf("%s.%s is not in %s and is not on the skip list: a snapshot would drop it. "+
				"Add it to %s, or skip it here with the reason", memory.Name(), field.Name, data.Name(), data.Name())
		}
	}

	for name := range plan.saved {
		assert.True(t, seen[name], "the plan saves %s.%s, which no longer exists", memory.Name(), name)
	}

	for name := range plan.skipped {
		assert.True(t, seen[name], "the plan skips %s.%s, which no longer exists", memory.Name(), name)
	}
}

func Test_snapshot_data_covers_every_field_of_a_message(t *testing.T) {
	fieldPlan{
		saved: map[string]string{
			"id": "ID", "body": "Body", "bodyMD5": "BodyMD5", "attributes": "Attributes",
			"attributesMD5": "AttributesMD5", "system": "System", "sent": "Sent",
			"groupID": "GroupID", "dedupID": "DedupID", "sequence": "Sequence", "receiveBase": "ReceiveBase",
		},
	}.check(t, reflect.TypeFor[message](), reflect.TypeFor[messageData]())
}

func Test_snapshot_data_covers_every_field_of_an_entry(t *testing.T) {
	fieldPlan{
		saved: map[string]string{
			"msg": "Message", "seq": "Seq", "state": "State", "deadline": "Deadline",
			"receiveCount": "ReceiveCount", "firstReceived": "FirstReceived", "handles": "Handles",
		},
		converted: map[string]bool{"msg": true, "state": true},
		skipped: map[string]string{
			"visibleAt":  "the position in the visible heap, which restore sets by pushing the entry",
			"deadlineAt": "the position in the deadline heap, which restore sets by pushing the entry",
			"fifo": "a FIFO entry's group comes from the message's groupID and the order of its group's " +
				"in-flight messages from the order of the snapshot's entries; a deduplication record is dedupData",
		},
	}.check(t, reflect.TypeFor[entry](), reflect.TypeFor[entryData]())
}

func Test_snapshot_data_covers_every_field_of_a_deduplication_record(t *testing.T) {
	fieldPlan{
		saved: map[string]string{"dedupKey": "Key", "original": "Original", "sequence": "Sequence", "expires": "Expires"},
		skipped: map[string]string{
			"group": "the group of a message, found again from the message's groupID",
			"prev":  "a link of the group's in-flight list, rebuilt from the order of the snapshot's entries",
			"next":  "a link of the group's in-flight list, rebuilt from the order of the snapshot's entries",
		},
	}.check(t, reflect.TypeFor[fifoState](), reflect.TypeFor[dedupData]())
}

func Test_snapshot_data_covers_every_field_of_a_move_task(t *testing.T) {
	fieldPlan{
		saved: map[string]string{
			"id": "ID", "source": "Source", "destination": "Destination", "rate": "Rate", "started": "Started",
			"order": "Order", "status": "Status", "moved": "Moved", "toMove": "ToMove", "failure": "Failure",
		},
		skipped: map[string]string{
			"cancel": "stops a goroutine, and a restored task has none",
			"mu":     "a lock",
		},
	}.check(t, reflect.TypeFor[moveTask](), reflect.TypeFor[moveTaskData]())
}

// populatedMessage is a message with every field set, so that a field the
// sample does not set cannot hide a snapshot that drops it.
func populatedMessage() *message {
	return &message{
		id: "id", body: "body", bodyMD5: "md5",
		attributes:    map[string]sqsapi.MessageAttributeValue{"a": {DataType: new("String"), StringValue: new("v")}},
		attributesMD5: "attrs-md5", system: map[string]string{sysSenderID: "000000000000"},
		sent:    time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC),
		groupID: "group", dedupID: "dedup", sequence: "18850000000000000001", receiveBase: 3,
	}
}

func Test_a_message_survives_its_snapshot_whole(t *testing.T) {
	sample := populatedMessage()

	for field := range reflect.TypeFor[message]().Fields() {
		assert.False(t, reflect.ValueOf(*sample).FieldByName(field.Name).IsZero(),
			"populatedMessage leaves %s unset, so this test cannot see it lost", field.Name)
	}

	data := sample.data()
	assert.Equal(t, sample, data.message())
}

func Test_a_move_task_survives_its_snapshot_whole(t *testing.T) {
	rate := int32(5)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	for _, tc := range []struct {
		name                      string
		saved, restored           string
		savedFailure, wantFailure string
	}{
		{"a finished task is as it was", moveCompleted, moveCompleted, "", ""},
		{"a failed task keeps its reason", moveFailed, moveFailed, "QueueDoesNotExist", "QueueDoesNotExist"},
		{"a cancelled task is as it was", moveCancelled, moveCancelled, "", ""},
		{"a running task has no goroutine, so it failed", moveRunning, moveFailed, "", failureRestarted},
		{"a task being cancelled was cancelled", moveCancelling, moveCancelled, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := moveTaskData{
				ID: "task", Source: "arn:src", Destination: "arn:dst", Rate: &rate, Started: started, Order: 7,
				Status: tc.saved, Moved: 4, ToMove: 9, Failure: tc.savedFailure,
			}

			task := saved.task()
			require.NotNil(t, task.cancel, "State.cancelMoveTasks calls it for every task")

			task.cancel()

			state := newState("", "")
			state.moveTasks[task.id] = task
			assert.Equal(t, []moveTaskData{{
				ID: "task", Source: "arn:src", Destination: "arn:dst", Rate: &rate, Started: started, Order: 7,
				Status: tc.restored, Moved: 4, ToMove: 9, Failure: tc.wantFailure,
			}}, state.saveMoveTasks())
		})
	}
}
