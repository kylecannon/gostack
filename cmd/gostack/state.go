package main

import (
	"context"
	"fmt"
	"slices"

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
}

// States returns every registered service as running, and says whether its state
// is saved to disk.
func (t *serviceTable) States() map[string]internalapi.ServiceState {
	states := make(map[string]internalapi.ServiceState, len(t.regs))

	for i := range t.regs {
		name := t.regs[i].Metadata.Name
		states[name] = internalapi.ServiceState{State: stateRunning, Persisted: t.eng.Enabled(name)}
	}

	return states
}

// Check does nothing: the states never change after startup.
func (*serviceTable) Check() {}

// Capabilities lists the registered services. A registration carries no list of
// the operations it implements yet, so only the names are reported.
func (t *serviceTable) Capabilities() []internalapi.ServiceCapability {
	capabilities := make([]internalapi.ServiceCapability, 0, len(t.regs))

	for i := range t.regs {
		capabilities = append(capabilities, internalapi.ServiceCapability{Name: t.regs[i].Metadata.Name})
	}

	return capabilities
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
// back; the engine ignores a service it does not save.
func (c *stateController) Reset(scope internalapi.StateScope) error {
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

		c.resetService(&c.regs[i], scope)

		return nil
	}

	for i := range c.regs {
		if c.regs[i].Reset != nil {
			c.resetService(&c.regs[i], scope)
		}
	}

	return nil
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

func (c *stateController) resetService(reg *service.Registration, scope internalapi.StateScope) {
	reg.Reset(scope.Account, scope.Region)
	c.eng.MarkDirty(reg.Metadata.Name)
}
