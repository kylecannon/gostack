package dynamodb

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// Time to live is stored as LocalStack stores it, and expired items are
// removed only two ways, as in LocalStack: DELETE on the developer endpoint,
// and an hourly sweep when DYNAMODB_REMOVE_EXPIRED_ITEMS is set. The sweep is
// one timer, armed when a table turns time to live on while the setting is
// set, and gone again once no table has it on or the setting is off.

const (
	// expiredCanonicalPath and expiredCompatPath are the developer endpoint:
	// gostack's own name, and LocalStack's, mounted unless compat paths are off.
	expiredCanonicalPath = "/_gostack/services/dynamodb/expired"
	expiredCompatPath    = "/_aws/dynamodb/expired"

	// sweepPeriod is how often the worker sweeps, LocalStack's hour.
	sweepPeriod = time.Hour
)

// expiry is the sweep timer. timer is nil while none is armed.
type expiry struct {
	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

// DescribeTimeToLive answers the table's time to live setting.
func (p *Provider) DescribeTimeToLive(
	ctx context.Context, in *api.DescribeTimeToLiveInput,
) (*api.DescribeTimeToLiveOutput, error) {
	e, err := p.lookupNamed(p.caller(ctx), in.TableName)
	if err != nil {
		return nil, err
	}

	e.mu.RLock()
	spec := e.meta.TTL
	e.mu.RUnlock()

	d := &api.TimeToLiveDescription{TimeToLiveStatus: new(statusDisabled)}
	if spec != nil {
		d.AttributeName = spec.AttributeName
		if spec.Enabled != nil && *spec.Enabled {
			d.TimeToLiveStatus = new(statusEnabled)
		}
	}

	return &api.DescribeTimeToLiveOutput{TimeToLiveDescription: d}, nil
}

// UpdateTimeToLive stores the table's time to live setting as given, and arms
// the sweep when it turns time to live on.
func (p *Provider) UpdateTimeToLive(
	ctx context.Context, in *api.UpdateTimeToLiveInput,
) (*api.UpdateTimeToLiveOutput, error) {
	e, err := p.lookupNamed(p.caller(ctx), in.TableName)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	e.meta.TTL = in.TimeToLiveSpecification
	e.mu.Unlock()

	if attribute, ok := ttlAttribute(in.TimeToLiveSpecification); ok && attribute != "" {
		p.armExpiry()
	} else {
		p.settleExpiry()
	}

	return &api.UpdateTimeToLiveOutput{TimeToLiveSpecification: in.TimeToLiveSpecification}, nil
}

// ttlAttribute is the attribute a time to live setting expires items by, and
// whether it is on.
func ttlAttribute(spec *api.TimeToLiveSpecification) (string, bool) {
	if spec == nil || spec.Enabled == nil || !*spec.Enabled {
		return "", false
	}

	return deref(spec.AttributeName), true
}

// mountExpired adds the developer endpoint that removes expired items.
func (p *Provider) mountExpired(r chi.Router) {
	r.Delete(expiredCanonicalPath, p.serveExpired)
	if !p.settings().DisableCompatPaths {
		r.Delete(expiredCompatPath, p.serveExpired)
	}
}

func (p *Provider) serveExpired(w http.ResponseWriter, _ *http.Request) {
	n := p.removeExpired()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ExpiredItems": ` + strconv.Itoa(n) + `}`))
}

// removeExpired removes the expired items of every table with time to live on,
// in every account and region, and returns how many it removed. A removal
// marks the service changed, since neither the sweep nor the developer
// endpoint is a mutating call the gateway sees.
func (p *Provider) removeExpired() int {
	removed := p.removeExpiredItems()
	if removed > 0 {
		p.notifyChanged()
	}

	return removed
}

func (p *Provider) removeExpiredItems() int {
	threshold := api.AttributeValue{N: new(strconv.FormatInt(p.now().Unix(), 10))}
	removed := 0

	p.states.Range(func(_, _ string, s *State) bool {
		for _, e := range s.entries() {
			removed += e.removeExpired(&threshold)
		}

		return true
	})

	return removed
}

// entries returns the state's tables.
func (s *State) entries() []*tableEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*tableEntry, 0, len(s.tables))
	for _, e := range s.tables {
		out = append(out, e)
	}

	return out
}

// removeExpired removes the table's items whose time to live attribute is a
// number at or before threshold, and returns how many it removed. It collects
// the expired items first and removes each through the engine's write path,
// checking again under the write lock, so an item written since is kept.
func (e *tableEntry) removeExpired(threshold *api.AttributeValue) int {
	e.mu.RLock()
	attribute, on := ttlAttribute(e.meta.TTL)
	e.mu.RUnlock()

	if !on || attribute == "" {
		return 0
	}

	// The time to live attribute is read in place, and an expired item is
	// kept as the table stores it, which names its key: nothing is decoded.
	var found []value.Packed

	_ = e.data.Scan("", nil, func(_ string, packed value.Packed) bool {
		if expired(packed, attribute, threshold) {
			found = append(found, packed)
		}

		return true
	})

	removed := 0

	for _, item := range found {
		if e.expire(item, attribute, threshold) {
			removed++
		}
	}

	return removed
}

// expire removes the item at the key of item when the item stored there now
// is still expired, and reports whether it did. It is the one place a time to
// live removal reaches the engine's write path, so the stream record of a
// removal hangs off it.
func (e *tableEntry) expire(item value.Packed, attribute string, threshold *api.AttributeValue) bool {
	return e.data.Expire(item, func(stored value.Packed) bool {
		return expired(stored, attribute, threshold)
	})
}

// expired reports whether the item's attribute is a number at or before
// threshold, the filter LocalStack's sweep scans with. It reads the number
// in place.
func expired(item value.Packed, attribute string, threshold *api.AttributeValue) bool {
	n, ok := item.Number(attribute)

	return ok && expiredNumber(n, threshold)
}

// expiredNumber reports whether the number n is at or before threshold.
func expiredNumber(n string, threshold *api.AttributeValue) bool {
	v := api.AttributeValue{N: &n}

	return value.Compare(&v, threshold) <= 0
}

// keyNames is the names of the key attributes of schema.
func keyNames(schema table.KeySchema) []string {
	if schema.HasRange() {
		return []string{schema.Hash.Name, schema.Range.Name}
	}

	return []string{schema.Hash.Name}
}

// armExpiry arms the sweep timer when DYNAMODB_REMOVE_EXPIRED_ITEMS is set and
// none is armed.
func (p *Provider) armExpiry() {
	if !p.settings().RemoveExpiredItems {
		return
	}

	p.expiry.mu.Lock()
	defer p.expiry.mu.Unlock()

	if p.expiry.timer == nil && !p.expiry.closed {
		p.expiry.timer = time.AfterFunc(sweepPeriod, p.sweep)
	}
}

// sweep is the timer's run: it removes the expired items, then arms the next
// run while the setting is set and some table has time to live on.
func (p *Provider) sweep() {
	on := p.settings().RemoveExpiredItems
	if on {
		p.removeExpired()
	}

	p.expiry.mu.Lock()
	defer p.expiry.mu.Unlock()

	p.expiry.timer = nil
	if on && !p.expiry.closed && p.anyTTL() {
		p.expiry.timer = time.AfterFunc(sweepPeriod, p.sweep)
	}
}

// settleExpiry stops the sweep timer when no table has time to live on, after
// a call that may have turned the last one off.
func (p *Provider) settleExpiry() {
	p.expiry.mu.Lock()
	defer p.expiry.mu.Unlock()

	if p.expiry.timer != nil && !p.anyTTL() {
		p.expiry.timer.Stop()
		p.expiry.timer = nil
	}
}

// anyTTL reports whether some table has time to live on.
func (p *Provider) anyTTL() bool {
	found := false

	p.states.Range(func(_, _ string, s *State) bool {
		for _, e := range s.entries() {
			e.mu.RLock()
			attribute, on := ttlAttribute(e.meta.TTL)
			e.mu.RUnlock()

			if on && attribute != "" {
				found = true

				return false
			}
		}

		return true
	})

	return found
}

// close stops the sweep timer and every stream shard's retention timer for
// good. The shards keep their records, so a save after close still holds
// them.
func (p *Provider) close(context.Context) {
	p.client.Close()

	p.expiry.mu.Lock()
	p.expiry.closed = true

	if p.expiry.timer != nil {
		p.expiry.timer.Stop()
		p.expiry.timer = nil
	}
	p.expiry.mu.Unlock()

	p.states.Range(func(_, _ string, s *State) bool {
		s.mu.RLock()
		entries := slices.Collect(maps.Values(s.tables))
		s.mu.RUnlock()

		for _, e := range entries {
			e.mu.RLock()
			if e.shard != nil {
				e.shard.Halt()
			}
			e.mu.RUnlock()
		}

		return true
	})
}
