package persist

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The services of the order tests.
const (
	ddb       = "dynamodb"
	ddbStream = "dynamodbstreams"
	sqsName   = "sqs"
	follower  = "a-follower"
	other     = "m-other"
	leader    = "z-leader"
)

// orderLog records the order services were saved and restored in.
type orderLog struct {
	mu   sync.Mutex
	seen []string
}

func (o *orderLog) add(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.seen = append(o.seen, name)
}

// logged is a Snapshotter that records its saves and restores in an orderLog,
// and follows leader when it is not "".
type logged struct {
	name, leader string
	log          *orderLog
}

func (l logged) Snapshot(w io.Writer) error {
	l.log.add(l.name)

	_, err := io.WriteString(w, l.name)

	return err //nolint:wrapcheck // a test double
}

func (l logged) Restore(io.Reader) error {
	l.log.add(l.name)

	return nil
}

// leading is logged with Follows, for a service that follows another.
type leading struct{ logged }

func (l leading) Follows() string { return l.leader }

// Test_saveOrder_followerRightAfterItsLeader: dynamodbstreams follows
// dynamodb. However long each last took, a flush saves dynamodbstreams right
// after dynamodb, so the streams file is never the older of the two; a
// follower whose leader is not pending keeps its place. The pair's place among
// the other services is by the larger of the two times; see
// Test_saveOrder_PairGoesByTheSlowerOfLeaderAndFollower.
func Test_saveOrder_followerRightAfterItsLeader(t *testing.T) {
	log := &orderLog{}
	streams := leading{logged{name: ddbStream, leader: ddb, log: log}}
	pending := []pendingService{
		{name: ddb, snap: logged{name: ddb, log: log}, took: 5 * time.Second},
		{name: ddbStream, snap: streams},
		{name: sqsName, snap: logged{name: sqsName, log: log}, took: time.Second},
	}

	names := func(ps []pendingService) []string {
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = p.name
		}

		return out
	}

	require.Equal(t, []string{sqsName, ddb, ddbStream}, names(saveOrder(pending)))
	require.Equal(t, []string{ddbStream, sqsName}, names(saveOrder(pending[1:])))
}

// Test_Engine_Load_RestoresALeaderBeforeItsFollower: a follower whose name
// sorts before its leader's is still restored after it, and a flush saves the
// two back to back, leader first.
func Test_Engine_Load_RestoresALeaderBeforeItsFollower(t *testing.T) {
	dir := t.TempDir()
	log := &orderLog{}

	register := func(e *Engine) {
		e.Register(follower, leading{logged{name: follower, leader: leader, log: log}})
		e.Register(other, logged{name: other, log: log})
		e.Register(leader, logged{name: leader, log: log})
	}

	e := New(quietConfig(dir, time.Hour))
	register(e)

	for _, name := range []string{follower, other, leader} {
		e.MarkDirty(name)
	}

	require.NoError(t, e.Flush(t.Context()))
	require.Equal(t, []string{other, leader, follower}, log.seen)

	log.seen = nil
	loaded := New(quietConfig(dir, time.Hour))
	register(loaded)
	require.NoError(t, loaded.Load(t.Context()))
	require.Equal(t, []string{other, leader, follower}, log.seen)
}

// Test_Engine_Flush_SavesAFollowerWithItsLeader: a flush that saves the
// leader saves its follower right after it although only the leader is
// dirty, as DynamoDB Streams' file must be of the cut DynamoDB's save took. A
// dirty follower is never saved alone: it takes its clean leader with it.
func Test_Engine_Flush_SavesAFollowerWithItsLeader(t *testing.T) {
	log := &orderLog{}
	e := New(quietConfig(t.TempDir(), time.Hour))
	e.Register(follower, leading{logged{name: follower, leader: leader, log: log}})
	e.Register(other, logged{name: other, log: log})
	e.Register(leader, logged{name: leader, log: log})

	e.MarkDirty(leader)
	require.NoError(t, e.Flush(t.Context()))
	require.Equal(t, []string{leader, follower}, log.seen)

	log.seen = nil
	e.MarkDirty(follower)
	require.NoError(t, e.Flush(t.Context()))
	require.Equal(t, []string{leader, follower}, log.seen)
}

// Test_Engine_Register_RefusesAChainOfFollowers: SQ-370 S1. A follower of a
// follower would never be saved alone, and at PERSIST_FREQUENCY 0 would fire
// the timer for flushes that take nothing; a service that follows itself the
// same. Register refuses either, in whichever order the services come.
func Test_Engine_Register_RefusesAChainOfFollowers(t *testing.T) {
	// The chain: other follows follower, which follows leader.
	log := &orderLog{}
	snaps := map[string]Snapshotter{
		leader:   logged{name: leader, log: log},
		follower: leading{logged{name: follower, leader: leader, log: log}},
		other:    leading{logged{name: other, leader: follower, log: log}},
	}

	for _, order := range [][]string{{leader, follower, other}, {other, leader, follower}, {leader, other, follower}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			e := New(quietConfig(t.TempDir(), Immediate))
			register := func(name string) func() { return func() { e.Register(name, snaps[name]) } }

			require.NotPanics(t, register(order[0]))
			require.NotPanics(t, register(order[1]))
			require.Panics(t, register(order[2]), "the third service makes a chain")
		})
	}

	t.Run("self", func(t *testing.T) {
		e := New(quietConfig(t.TempDir(), Immediate))
		require.Panics(t, func() { e.Register(other, leading{logged{name: other, leader: other, log: log}}) })
	})

	t.Run("two followers of one leader", func(t *testing.T) {
		e := New(quietConfig(t.TempDir(), Immediate))
		require.NotPanics(t, func() {
			e.Register(leader, snaps[leader])
			e.Register(follower, snaps[follower])
			e.Register(other, leading{logged{name: other, leader: leader, log: log}})
		})
	})
}
