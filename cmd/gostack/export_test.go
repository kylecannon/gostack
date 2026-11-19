package main

import (
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/stretchr/testify/require"
)

// RegisteredServices maps every service registrations() lists, by name, to
// whether it has state that persistence saves. The tests that run the binary
// build the service lists they expect from it, so adding a service changes no
// assertion.
func RegisteredServices(t testing.TB) map[string]bool {
	t.Helper()

	cfg, err := config.Load()
	require.NoError(t, err)

	regs := newProviders(cfg, func(string) {}).regs
	services := make(map[string]bool, len(regs))

	for i := range regs {
		services[regs[i].Metadata.Name] = regs[i].Persist != nil
	}

	return services
}
