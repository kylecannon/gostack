package sqs

import (
	"cmp"
	"encoding/json"
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

// persistentContents is a Contents that can be saved and restored. Every
// Contents the Provider makes has to be one, because a queue whose messages
// cannot be saved would come back empty: Snapshot fails for it instead.
//
// The queue calls both methods with its own lock held.
type persistentContents interface {
	Contents
	// snapshot returns the messages and everything that orders or times them as
	// json, deadlines as absolute times.
	snapshot() (json.RawMessage, error)
	// restore fills a Contents that was just made from what snapshot returned,
	// and arms its timers.
	restore(raw json.RawMessage) error
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
// lately and the next creation sequence number.
type stateData struct {
	Account string               `json:"account"`
	Region  string               `json:"region"`
	Next    uint64               `json:"next"`
	Deleted map[string]time.Time `json:"deleted,omitempty"`
	Queues  []queueData          `json:"queues"`
}

// queueData is a Queue: what makes it up besides its name and place, and the
// contents' own data.
type queueData struct {
	Name     string            `json:"name"`
	FIFO     bool              `json:"fifo,omitempty"`
	Seq      uint64            `json:"seq"`
	Attrs    map[string]string `json:"attributes"`
	Tags     map[string]string `json:"tags,omitempty"`
	PurgedAt time.Time         `json:"purgedAt,omitzero"`
	Contents json.RawMessage   `json:"contents"`
}

// Snapshot writes the state of every account and region to w as json. It
// implements persist.Snapshotter. Each queue is read under its own lock, so a
// call that runs meanwhile is in the snapshot or not, whole.
func (p *Provider) Snapshot(w io.Writer) error {
	file := snapshotFile{Version: snapshotVersion, Data: snapshotData{States: []stateData{}}}

	var failure error

	p.states.Range(func(account, region string, state *State) bool {
		saved, err := state.save(account, region)
		if err != nil {
			failure = err

			return false
		}

		if saved.Next > 0 || len(saved.Deleted) > 0 || len(saved.Queues) > 0 {
			file.Data.States = append(file.Data.States, saved)
		}

		return true
	})

	if failure != nil {
		return failure
	}

	slices.SortFunc(file.Data.States, func(a, b stateData) int {
		return cmp.Or(strings.Compare(a.Account, b.Account), strings.Compare(a.Region, b.Region))
	})

	raw, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("encode sqs snapshot: %w", err)
	}

	_, err = w.Write(raw)
	if err != nil {
		return fmt.Errorf("write sqs snapshot: %w", err)
	}

	return nil
}

// save reads one State.
func (s *State) save(account, region string) (stateData, error) {
	s.mu.RLock()
	saved := stateData{Account: account, Region: region, Next: s.next, Deleted: maps.Clone(s.deleted)}
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

// save reads one queue under its lock.
func (q *Queue) save() (queueData, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	contents, ok := q.contents.(persistentContents)
	if !ok {
		return queueData{}, fmt.Errorf("queue %s (%s): its contents cannot be saved", q.name, q.arn)
	}

	raw, err := contents.snapshot()
	if err != nil {
		return queueData{}, fmt.Errorf("queue %s (%s): %w", q.name, q.arn, err)
	}

	return queueData{
		Name: q.name, FIFO: q.kind == kindFIFO, Seq: q.seq,
		Attrs: maps.Clone(q.attrs), Tags: maps.Clone(q.tags), PurgedAt: q.purgedAt, Contents: raw,
	}, nil
}

// Restore replaces the state of every account and region with what r holds. It
// implements persist.Snapshotter. The snapshot is read and checked whole before
// anything is replaced, and a snapshot that is not valid leaves SQS empty, as
// the Snapshotter contract says.
func (p *Provider) Restore(r io.Reader) error {
	built, err := p.build(r)

	p.Reset("", "")

	if err != nil {
		return err
	}

	for _, one := range built {
		state := p.states.Get(one.account, one.region)

		state.mu.Lock()
		state.next = one.next
		state.deleted = one.deleted

		for _, q := range one.queues {
			state.add(q)
		}

		state.mu.Unlock()
	}

	return nil
}

// builtState is a State read from a snapshot and not yet in the Provider.
type builtState struct {
	account, region string
	next            uint64
	deleted         map[string]time.Time
	queues          []*Queue
}

// build reads a snapshot into queues with their contents restored. A queue
// that was built before an error is closed, so no timer outlives a failed
// restore.
func (p *Provider) build(r io.Reader) (built []builtState, err error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read sqs snapshot: %w", err)
	}

	var file snapshotFile

	err = json.Unmarshal(raw, &file)
	if err != nil {
		return nil, fmt.Errorf("decode sqs snapshot: %w", err)
	}

	if file.Version != snapshotVersion {
		return nil, fmt.Errorf("sqs snapshot is version %d, this build reads version %d", file.Version, snapshotVersion)
	}

	defer func() {
		if err != nil {
			for _, one := range built {
				for _, q := range one.queues {
					q.close()
				}
			}

			built = nil
		}
	}()

	for i := range file.Data.States {
		var one builtState

		one, err = p.buildState(&file.Data.States[i])
		built = append(built, one)

		if err != nil {
			return built, err
		}
	}

	return built, nil
}

// buildState makes the queues of one State. On error it returns the queues made
// so far, for the caller to close.
func (p *Provider) buildState(saved *stateData) (builtState, error) {
	one := builtState{
		account: saved.Account, region: saved.Region, next: saved.Next, deleted: saved.Deleted,
		queues: make([]*Queue, 0, len(saved.Queues)),
	}

	if saved.Account == "" || saved.Region == "" {
		return one, errors.New("sqs snapshot has a state without an account or region")
	}

	if one.deleted == nil {
		one.deleted = make(map[string]time.Time)
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

	q.mu.Lock()
	defer q.mu.Unlock()

	err := contents.restore(d.Contents)
	if err != nil {
		contents.Close()

		return nil, fmt.Errorf("queue %s (%s): %w", q.name, q.arn, err)
	}

	return q, nil
}
