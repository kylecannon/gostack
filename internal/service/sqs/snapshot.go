package sqs

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// snapshotVersion is the version of the snapshot envelope: {"v":1,"data":...}.
// Restore refuses any other.
const snapshotVersion = 1

// snapshotOptions are the json settings of a snapshot, on both sides of it. A
// map is written in key order, so the same state gives the same bytes. A string
// that is not valid UTF-8, which a tag or an attribute could hold, goes through
// as it is instead of failing the whole save.
var snapshotOptions = json.JoinOptions(
	json.Deterministic(true),
	jsontext.AllowInvalidUTF8(true),
)

// persistentContents is a Contents that can be saved and restored. Every
// Contents the Provider makes has to be one, because a queue whose messages
// cannot be saved would come back empty: Snapshot fails for it instead.
//
// The queue calls both methods with its own lock held.
type persistentContents interface {
	Contents
	// snapshot copies the messages and everything that orders or times them,
	// deadlines as absolute times. It only copies: the queue's lock is held, so
	// it sorts and encodes nothing, and what it returns shares nothing the queue
	// goes on to change.
	snapshot() contentsData
	// restore fills a Contents that was just made from what snapshot returned,
	// and arms its timers.
	restore(data *contentsData) error
}

// snapshotFile is the envelope of a snapshot.
type snapshotFile struct {
	Version int          `json:"v"`
	Data    snapshotData `json:"data"`
}

// snapshotData is the SQS state of every account and region.
type snapshotData struct {
	States []stateData `json:"states"`
}

// stateData is a State: the queues of one account and region, the names deleted
// lately, the next creation sequence number and the message move tasks.
type stateData struct {
	Account   string               `json:"account"`
	Region    string               `json:"region"`
	Next      uint64               `json:"next"`
	Deleted   map[string]time.Time `json:"deleted,omitempty"`
	Queues    []queueData          `json:"queues"`
	MoveTasks []moveTaskData       `json:"moveTasks,omitempty"`
	MoveSeq   uint64               `json:"moveSeq,omitzero"`
}

// queueData is a Queue: what makes it up besides its name and place, and the
// contents' own data.
type queueData struct {
	Name     string            `json:"name"`
	FIFO     bool              `json:"fifo,omitzero"`
	Seq      uint64            `json:"seq"`
	Attrs    map[string]string `json:"attributes"`
	Tags     map[string]string `json:"tags,omitempty"`
	PurgedAt time.Time         `json:"purgedAt,omitzero"`
	Contents contentsData      `json:"contents"`
}

// Snapshot writes the state of every account and region to w as json. It
// implements persist.Snapshotter.
//
// It reads in two steps. First it copies every queue into memory, holding the
// provider's transfer lock for writing, so that no message is moving between
// two queues, and each queue's lock only for its own copy. Then, with every lock
// released, it sorts the copy and encodes it straight to w. The snapshot is one
// moment of the state: a message that a dead-letter move or a move task is
// carrying is in its source or in its destination, never in both or neither,
// and a call that runs meanwhile is in the snapshot or not, whole.
func (p *Provider) Snapshot(w io.Writer) error {
	file, err := p.capture()
	if err != nil {
		return err
	}

	file.sort()

	err = json.MarshalEncode(jsontext.NewEncoder(w, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return fmt.Errorf("write sqs snapshot: %w", err)
	}

	return nil
}

// capture copies the state of every account and region.
//
// The transfer lock covers every state, because a move can name a queue of
// another account or region by ARN.
func (p *Provider) capture() (snapshotFile, error) {
	file := snapshotFile{Version: snapshotVersion, Data: snapshotData{States: []stateData{}}}

	p.transfer.Lock()
	defer p.transfer.Unlock()

	var failure error

	p.states.Range(func(account, region string, state *State) bool {
		saved, err := state.save(account, region)
		if err != nil {
			failure = err

			return false
		}

		if saved.Next > 0 || len(saved.Deleted) > 0 || len(saved.Queues) > 0 || len(saved.MoveTasks) > 0 {
			file.Data.States = append(file.Data.States, saved)
		}

		return true
	})

	return file, failure
}

// sort puts the copy in the order the file keeps it, which takes more time than
// the queues' locks should be held for.
func (f *snapshotFile) sort() {
	slices.SortFunc(f.Data.States, func(a, b stateData) int {
		return cmp.Or(strings.Compare(a.Account, b.Account), strings.Compare(a.Region, b.Region))
	})

	for i := range f.Data.States {
		for j := range f.Data.States[i].Queues {
			f.Data.States[i].Queues[j].Contents.sort()
		}
	}
}

// save copies one State.
func (s *State) save(account, region string) (stateData, error) {
	s.mu.RLock()
	saved := stateData{
		Account: account, Region: region, Next: s.next, Deleted: maps.Clone(s.deleted),
		MoveTasks: s.saveMoveTasks(), MoveSeq: s.moveSeq,
	}
	s.mu.RUnlock()

	for _, q := range s.list("") {
		one, err := q.save()
		if err != nil {
			return stateData{}, err
		}

		saved.Queues = append(saved.Queues, one)
	}

	return saved, nil
}

// save copies one queue under its lock.
func (q *Queue) save() (queueData, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	contents, ok := q.contents.(persistentContents)
	if !ok {
		return queueData{}, fmt.Errorf("queue %s (%s): its contents cannot be saved", q.name, q.arn)
	}

	return queueData{
		Name: q.name, FIFO: q.kind == kindFIFO, Seq: q.seq,
		Attrs: maps.Clone(q.attrs), Tags: maps.Clone(q.tags), PurgedAt: q.purgedAt, Contents: contents.snapshot(),
	}, nil
}

// Restore replaces the state of every account and region with what r holds. It
// implements persist.Snapshotter. The snapshot is read and checked whole before
// anything is replaced, and a snapshot that is not valid leaves SQS empty, as
// the Snapshotter contract says.
//
// A message move task is a goroutine that puts messages on queues it looks up
// by ARN. Restore holds the transfer lock for writing from the moment it stops
// them until the new state is in. No dead-letter move runs meanwhile, no task
// can start or be cancelled, and none of the stopped ones puts a message on a
// restored queue. The wait is bounded: a task ends after the message it is
// moving, and nothing starts a new one.
//
// A call that only looks a queue up, as every operation does first, takes no
// transfer lock. Each State goes from the old contents to the restored ones in
// one step, so such a call finds the old queue or the restored one, never none.
//
// Whatever goes wrong, a panic included, SQS is left empty and usable, and every
// queue that was built is closed.
func (p *Provider) Restore(r io.Reader) error {
	built, err := p.build(r)

	p.transfer.Lock()
	defer p.transfer.Unlock()

	p.stopMoveTasks()

	if err != nil {
		p.Reset("", "")

		return err
	}

	// A panic while the state goes in leaves half of it there: empty it, and
	// close the queues that did not get in.
	defer closeOnPanic(func() {
		closeBuilt(built)
		p.Reset("", "")
	})

	p.install(built)

	return nil
}

// install puts each built State in place of what its State held, and empties
// every State the snapshot does not name.
func (p *Provider) install(built []builtState) {
	byPlace := make(map[[2]string]*builtState, len(built))

	for i := range built {
		one := &built[i]
		byPlace[[2]string{one.account, one.region}] = one

		// Range is sure to walk only the States that exist when it starts.
		p.states.Get(one.account, one.region)
	}

	p.states.Range(func(account, region string, state *State) bool {
		if one, ok := byPlace[[2]string{account, region}]; ok {
			state.replace(one)
		} else {
			state.reset()
		}

		return true
	})
}

// closeOnPanic is deferred to run fn when its caller panics. The panic goes on.
func closeOnPanic(fn func()) {
	//nolint:revive // The caller defers this function itself, so recover is called by the deferred call.
	if r := recover(); r != nil {
		fn()

		panic(r)
	}
}

// closeBuilt closes the queues of states that are not in the Provider. Closing
// a queue twice is harmless.
func closeBuilt(built []builtState) {
	for _, one := range built {
		for _, q := range one.queues {
			q.close()
		}
	}
}

// builtState is a State read from a snapshot and not yet in the Provider.
type builtState struct {
	account, region string
	next            uint64
	deleted         map[string]time.Time
	queues          []*Queue
	moveTasks       map[string]*moveTask
	moveSeq         uint64
}

// build reads a snapshot into queues with their contents restored. A queue
// that was built before an error or a panic is closed, so no timer outlives a
// failed restore, and a panic comes back as the error of the snapshot that
// caused it.
func (p *Provider) build(r io.Reader) (built []builtState, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			built, err = nil, fmt.Errorf("restore sqs snapshot: panic: %v", recovered)
		}
	}()

	var file snapshotFile

	dec := jsontext.NewDecoder(r, restoreOptions)

	err = json.UnmarshalDecode(dec, &file, restoreOptions)
	if err != nil {
		return nil, fmt.Errorf("decode sqs snapshot: %w", err)
	}

	_, err = dec.ReadToken()
	if !errors.Is(err, io.EOF) {
		return nil, errors.New("decode sqs snapshot: data after the snapshot")
	}

	if file.Version != snapshotVersion {
		return nil, fmt.Errorf("sqs snapshot is version %d, this build reads version %d", file.Version, snapshotVersion)
	}

	// Snapshot always writes the list, empty when there is no state.
	if file.Data.States == nil {
		return nil, errors.New("sqs snapshot has no list of states")
	}

	done := false

	defer func() {
		if !done {
			closeBuilt(built)

			built = nil
		}
	}()

	err = checkPlaces(file.Data.States)
	if err != nil {
		return nil, err
	}

	for i := range file.Data.States {
		var one builtState

		one, err = p.buildState(&file.Data.States[i])
		built = append(built, one)

		if err != nil {
			return built, err
		}
	}

	done = true

	return built, nil
}

// checkPlaces refuses a snapshot that names an account and region twice.
// install puts one built State in each State, so the queues of a second would
// be left running and out of reach.
func checkPlaces(states []stateData) error {
	places := make(map[[2]string]bool, len(states))

	for i := range states {
		place := [2]string{states[i].Account, states[i].Region}
		if places[place] {
			return fmt.Errorf("sqs snapshot has %s %s twice", place[0], place[1])
		}

		places[place] = true
	}

	return nil
}

// buildState makes the queues of one State. On error it returns the queues made
// so far, for the caller to close.
func (p *Provider) buildState(saved *stateData) (builtState, error) {
	one := builtState{
		account: saved.Account, region: saved.Region, next: saved.Next, deleted: saved.Deleted,
		queues:    make([]*Queue, 0, len(saved.Queues)),
		moveTasks: make(map[string]*moveTask, len(saved.MoveTasks)), moveSeq: saved.MoveSeq,
	}

	// A panic loses the queues made so far, which the caller would never see.
	defer closeOnPanic(func() { closeBuilt([]builtState{one}) })

	if saved.Account == "" || saved.Region == "" {
		return one, errors.New("sqs snapshot has a state without an account or region")
	}

	if one.deleted == nil {
		one.deleted = make(map[string]time.Time)
	}

	for name, at := range one.deleted {
		if at.IsZero() {
			return one, fmt.Errorf("sqs snapshot has no time for the deleted queue %s", name)
		}
	}

	for i := range saved.MoveTasks {
		d := &saved.MoveTasks[i]

		if d.ID == "" || d.Source == "" {
			return one, errors.New("sqs snapshot has a message move task without an id or a source")
		}

		one.moveTasks[d.ID] = d.task()
		one.moveSeq = max(one.moveSeq, d.Order)
	}

	seen := make(map[string]bool, len(saved.Queues))

	for i := range saved.Queues {
		d := &saved.Queues[i]

		if seen[d.Name] {
			return one, fmt.Errorf("sqs snapshot has queue %s twice in %s %s", d.Name, saved.Account, saved.Region)
		}

		seen[d.Name] = true

		q, err := p.buildQueue(saved, d)
		if err != nil {
			return one, err
		}

		one.queues = append(one.queues, q)
		one.next = max(one.next, d.Seq)
	}

	return one, nil
}

// buildQueue makes a queue from its saved form and restores its contents.
func (p *Provider) buildQueue(saved *stateData, d *queueData) (*Queue, error) {
	kind := kindStandard
	if d.FIFO {
		kind = kindFIFO
	}

	if validateQueueName(d.Name, kind) != nil {
		return nil, fmt.Errorf("sqs snapshot has the invalid queue name %q", d.Name)
	}

	q := &Queue{
		name: d.Name, account: saved.Account, region: saved.Region,
		arn:  queueARN(saved.Account, saved.Region, d.Name),
		kind: kind, seq: d.Seq, attrs: d.Attrs, tags: d.Tags, purgedAt: d.PurgedAt,
	}

	if q.attrs == nil {
		q.attrs = make(map[string]string)
	}

	q.contents = p.contents(q)

	contents, ok := q.contents.(persistentContents)
	if !ok {
		q.close()

		return nil, fmt.Errorf("queue %s (%s): its contents cannot be restored", q.name, q.arn)
	}

	// Snapshot always writes the list, empty when the queue holds nothing.
	if d.Contents.Entries == nil {
		q.close()

		return nil, fmt.Errorf("queue %s (%s): the snapshot has no list of messages", q.name, q.arn)
	}

	// It runs after the queue's lock is given up, which close takes.
	defer closeOnPanic(q.close)

	q.mu.Lock()
	defer q.mu.Unlock()

	err := contents.restore(&d.Contents)
	if err != nil {
		contents.Close()

		return nil, fmt.Errorf("queue %s (%s): %w", q.name, q.arn, err)
	}

	return q, nil
}
