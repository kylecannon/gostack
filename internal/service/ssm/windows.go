package ssm

import (
	"context"
	"crypto/rand"
	"slices"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	"github.com/kylecannon/gostack/internal/protocol"
)

// windowIDDigits is how many hex digits follow "mw-" in a window id.
const windowIDDigits = 17

// The maintenance windows are the records moto keeps for one, without targets,
// tasks or executions, and without moto's checks, which it does not make. They
// are here so GetMaintenanceWindow answers DoesNotExistException because no
// such window exists, as test_get_inexistent_maintenance_window expects.

// CreateMaintenanceWindow records a window and returns its id.
func (p *Provider) CreateMaintenanceWindow(
	ctx context.Context, in *ssmapi.CreateMaintenanceWindowInput,
) (*ssmapi.CreateMaintenanceWindowOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	w := &window{
		ID: newWindowID(), Name: in.Name, Description: in.Description, Duration: in.Duration, Cutoff: in.Cutoff,
		Schedule: in.Schedule, ScheduleTimezone: in.ScheduleTimezone, ScheduleOffset: in.ScheduleOffset,
		StartDate: in.StartDate, EndDate: in.EndDate, AllowUnassociatedTargets: in.AllowUnassociatedTargets,
		Created: p.now(),
	}
	if len(in.Tags) > 0 {
		w.Tags = mergeTags(nil, in.Tags)
	}

	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()

	s.windows[w.ID] = w
	s.windowOrder = append(s.windowOrder, w.ID)

	return &ssmapi.CreateMaintenanceWindowOutput{WindowId: new(w.ID)}, nil
}

// GetMaintenanceWindow returns a window.
func (p *Provider) GetMaintenanceWindow(
	ctx context.Context, in *ssmapi.GetMaintenanceWindowInput,
) (*ssmapi.GetMaintenanceWindowOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	w := s.windows[deref(in.WindowId)]
	if w == nil {
		return nil, windowNotFound(deref(in.WindowId))
	}

	return &ssmapi.GetMaintenanceWindowOutput{
		WindowId: new(w.ID), Name: w.Name, Description: w.Description, StartDate: w.StartDate, EndDate: w.EndDate,
		Schedule: w.Schedule, ScheduleTimezone: w.ScheduleTimezone, ScheduleOffset: w.ScheduleOffset,
		Duration: w.Duration, Cutoff: w.Cutoff, AllowUnassociatedTargets: w.AllowUnassociatedTargets,
		Enabled: new(true), CreatedDate: &protocol.Time{Time: w.Created}, ModifiedDate: &protocol.Time{Time: w.Created},
	}, nil
}

// DescribeMaintenanceWindows lists the windows in creation order. As in moto,
// only a Name filter applies and there is no paging.
func (p *Provider) DescribeMaintenanceWindows(
	ctx context.Context, in *ssmapi.DescribeMaintenanceWindowsInput,
) (*ssmapi.DescribeMaintenanceWindowsOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := &ssmapi.DescribeMaintenanceWindowsOutput{WindowIdentities: []ssmapi.MaintenanceWindowIdentity{}}

	for _, id := range s.windowOrder {
		w := s.windows[id]
		if !windowMatches(w, in.Filters) {
			continue
		}

		out.WindowIdentities = append(out.WindowIdentities, ssmapi.MaintenanceWindowIdentity{
			WindowId: new(w.ID), Name: w.Name, Description: w.Description, Enabled: new(true),
			Duration: w.Duration, Cutoff: w.Cutoff, Schedule: w.Schedule, ScheduleTimezone: w.ScheduleTimezone,
			ScheduleOffset: w.ScheduleOffset, EndDate: w.EndDate, StartDate: w.StartDate,
		})
	}

	return out, nil
}

// windowMatches applies moto's Name filters to a window.
func windowMatches(w *window, filters []ssmapi.MaintenanceWindowFilter) bool {
	for _, f := range filters {
		if deref(f.Key) == keyName && !slices.Contains(f.Values, deref(w.Name)) {
			return false
		}
	}

	return true
}

// DeleteMaintenanceWindow removes a window with its tags.
func (p *Provider) DeleteMaintenanceWindow(
	ctx context.Context, in *ssmapi.DeleteMaintenanceWindowInput,
) (*ssmapi.DeleteMaintenanceWindowOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	id := deref(in.WindowId)

	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.windows[id] == nil {
		return nil, windowNotFound(id)
	}

	delete(s.windows, id)
	s.windowOrder = slices.DeleteFunc(s.windowOrder, func(k string) bool { return k == id })

	return &ssmapi.DeleteMaintenanceWindowOutput{WindowId: new(id)}, nil
}

// windowNotFound is moto's DoesNotExistException for a window id.
func windowNotFound(id string) error {
	return ssmapi.NewDoesNotExistException("Maintenance window " + id + " does not exist")
}

// newWindowID is a window id as moto makes one: "mw-" and 17 hex digits.
func newWindowID() string {
	const hexDigits = "0123456789abcdef"

	var raw [windowIDDigits]byte

	_, _ = rand.Read(raw[:])

	id := make([]byte, 0, len("mw-")+windowIDDigits)
	id = append(id, "mw-"...)

	for _, b := range raw {
		id = append(id, hexDigits[b%16])
	}

	return string(id)
}
