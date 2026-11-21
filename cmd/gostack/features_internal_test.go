package main

import (
	"cmp"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// featuresFactsEnv names the file Test_featureFacts writes the registered
// services' facts to. `make features` sets it and parity/features.py reads the
// file, so docs/features.md states what the binary serves.
const featuresFactsEnv = "GOSTACK_FEATURES_FACTS"

// featureFact is what parity/features.py needs to know of one registered
// service.
type featureFact struct {
	Name string `json:"name"`
	// Operations is every operation of the service's API, sorted.
	Operations []string `json:"operations"`
	// Implemented is the operations the service answers itself, sorted, from the
	// same check GET /_gostack/services uses. It is nil when they cannot be told.
	Implemented []string `json:"implemented"`
	// Persist is whether the service has state that PERSIST_<NAME> saves.
	Persist bool `json:"persist"`
}

// featureFactOf is the fact of one registered service. A service that
// implements no operation has an empty, not a nil, Implemented: it encodes as
// [], and parity/features.py reads null as "unknown", which is not the same.
func featureFactOf(reg service.Registration, capability internalapi.ServiceCapability) featureFact {
	fact := featureFact{
		Name:       reg.Metadata.Name,
		Operations: slices.Sorted(slices.Values(reg.Metadata.Operations)),
		Persist:    reg.Persist != nil,
	}
	if !capability.Unknown {
		fact.Implemented = append([]string{}, slices.Sorted(slices.Values(capability.Implemented))...)
	}

	return fact
}

// Test_featureFactOf_implementedNothing holds the difference between a service
// that implements no operation and one whose operations are not known.
func Test_featureFactOf_implementedNothing(t *testing.T) {
	const name = "skeleton"

	reg := service.Registration{Metadata: service.Metadata{Name: name, Operations: []string{"B", "A"}}}

	none, err := json.Marshal(featureFactOf(reg, internalapi.ServiceCapability{Name: name, Total: 2}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"skeleton","operations":["A","B"],"implemented":[],"persist":false}`, string(none))

	capability := internalapi.ServiceCapability{Name: name, Unknown: true, Total: 2}
	unknown, err := json.Marshal(featureFactOf(reg, capability))
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"skeleton","operations":["A","B"],"implemented":null,"persist":false}`, string(unknown))
}

// Test_featureFacts builds the facts docs/features.md is generated from, and
// writes them to $GOSTACK_FEATURES_FACTS when that is set. Without it the test
// checks the facts are well formed, so the generator's input never rots.
func Test_featureFacts(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	regs := newProviders(cfg, func(string) {}).regs
	table := &serviceTable{regs: regs, eng: persist.New(persist.Config{})}
	capabilities := table.Capabilities()
	require.Len(t, capabilities, len(regs))

	facts := make([]featureFact, 0, len(regs))

	for i, capability := range capabilities {
		require.Equal(t, regs[i].Metadata.Name, capability.Name)

		fact := featureFactOf(regs[i], capability)
		assert.Subset(t, fact.Operations, fact.Implemented, "%s implements only operations of its API", fact.Name)
		assert.NotEmpty(t, fact.Operations, "%s lists its operations", fact.Name)
		facts = append(facts, fact)
	}

	slices.SortFunc(facts, func(a, b featureFact) int { return cmp.Compare(a.Name, b.Name) })

	path := os.Getenv(featuresFactsEnv)
	if path == "" {
		return
	}

	encoded, err := json.MarshalIndent(facts, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(encoded, '\n'), 0o600))
}
