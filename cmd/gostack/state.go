package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
)

// stateRunning is what the health endpoints report for a registered service.
// gostack has no lazy loading: a registered service is serving.
const stateRunning = "running"

// serviceTable reports the registered services to the health endpoints. It
// implements [internalapi.ServiceRegistry].
type serviceTable struct {
	regs []service.Registration
	eng  *persist.Engine

	// capabilities are worked out once, on the first request for them: they
	// never change after startup, and the startup does not pay for them.
	capabilities []internalapi.ServiceCapability
	once         sync.Once
}

// States returns every registered service as running, and says whether its state
// is saved to disk.
func (t *serviceTable) States() map[string]internalapi.ServiceState {
	states := make(map[string]internalapi.ServiceState, len(t.regs))

	for i := range t.regs {
		name := t.regs[i].Metadata.Name
		persisted := t.regs[i].Persist != nil && t.eng.Enabled(name)
		states[name] = internalapi.ServiceState{State: stateRunning, Persisted: persisted}
	}

	return states
}

// Check does nothing: the states never change after startup.
func (*serviceTable) Check() {}

// Capabilities lists, for every registered service, the operations of its API
// and the ones its Handler implements. A service without a Handler, or whose
// Handler the check cannot read, reports them as unknown.
func (t *serviceTable) Capabilities() []internalapi.ServiceCapability {
	t.once.Do(func() {
		t.capabilities = make([]internalapi.ServiceCapability, 0, len(t.regs))

		for i := range t.regs {
			metadata := &t.regs[i].Metadata
			implemented, known := service.Implemented(t.regs[i].Handler, metadata.Operations)
			t.capabilities = append(t.capabilities, internalapi.ServiceCapability{
				Name:        metadata.Name,
				Implemented: implemented,
				Unknown:     !known,
				Total:       len(metadata.Operations),
			})
		}
	})

	return t.capabilities
}

// stateController implements [internalapi.StateController]: reset goes to the
// services, save and load go through the persistence engine.
type stateController struct {
	regs []service.Registration
	eng  *persist.Engine
}

// Reset wipes the state of the service named by the scope, or of every service
// that keeps state when the scope names none, persisted or not. A wiped service
// is marked dirty, so the wipe reaches the disk and a restart does not bring it
// back; the engine ignores a service it does not save. A reset of every service
// tries each one, and returns the errors of the ones that failed joined, each
// naming its service.
func (c *stateController) Reset(ctx context.Context, scope internalapi.StateScope) error {
	if scope.Service != "" {
		i := slices.IndexFunc(c.regs, func(reg service.Registration) bool {
			return reg.Metadata.Name == scope.Service
		})
		if i < 0 {
			return fmt.Errorf("%w: %q", internalapi.ErrUnknownService, scope.Service)
		}

		if c.regs[i].Reset == nil {
			return fmt.Errorf("%w: %s keeps no state to reset", internalapi.ErrNotSupported, scope.Service)
		}

		return c.resetService(ctx, &c.regs[i], scope)
	}

	// A service that fails does not stop the ones after it: stopping would leave
	// the reset half done with no word of which services it reached.
	var errs []error

	for i := range c.regs {
		if c.regs[i].Reset == nil {
			continue
		}

		err := c.resetService(ctx, &c.regs[i], scope)
		if err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Save writes the state of every persisted service now, not only the services
// that changed since the last flush.
func (c *stateController) Save(ctx context.Context) error {
	if c.eng.BaseDir() == "" {
		return fmt.Errorf("%w: persistence is off", internalapi.ErrNotSupported)
	}

	for i := range c.regs {
		if name := c.regs[i].Metadata.Name; c.regs[i].Persist != nil {
			c.eng.MarkDirty(name) //nolint:contextcheck // MarkDirty arms a timer, whose callback has no caller context
		}
	}

	err := c.eng.Flush(ctx)
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	return nil
}

// Load replaces the state of every persisted service with what is on disk.
func (c *stateController) Load(ctx context.Context) error {
	if c.eng.BaseDir() == "" {
		return fmt.Errorf("%w: persistence is off", internalapi.ErrNotSupported)
	}

	err := c.eng.Load(ctx)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	return nil
}

// resetService wipes one service inside the scope. A reset of every account and
// region also removes every asset the service persisted, once the wipe is saved.
func (c *stateController) resetService(
	ctx context.Context, reg *service.Registration, scope internalapi.StateScope,
) error {
	name := reg.Metadata.Name
	wipe := func() { reg.Reset(scope.Account, scope.Region) }

	if scope.Account == "" && scope.Region == "" {
		err := c.eng.ResetAssets(ctx, name, wipe)
		if err != nil {
			return fmt.Errorf("reset state: %w", err) // names the service
		}
	} else {
		wipe()
	}

	c.eng.MarkDirty(name) //nolint:contextcheck // MarkDirty arms a timer, whose callback has no caller context

	return nil
}
