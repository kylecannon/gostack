package main

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/persist"
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
		metadata := regs[i].Metadata
		require.Equal(t, metadata.Name, capability.Name)

		fact := featureFact{
			Name:       metadata.Name,
			Operations: slices.Sorted(slices.Values(metadata.Operations)),
			Persist:    regs[i].Persist != nil,
		}
		if !capability.Unknown {
			fact.Implemented = slices.Sorted(slices.Values(capability.Implemented))
			assert.Subset(t, fact.Operations, fact.Implemented, "%s implements only operations of its API", fact.Name)
		}

		assert.NotEmpty(t, fact.Operations, "%s lists its operations", fact.Name)
		facts = append(facts, fact)
	}

	slices.SortFunc(facts, func(a, b featureFact) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})

	path := os.Getenv(featuresFactsEnv)
	if path == "" {
		return
	}

	encoded, err := json.MarshalIndent(facts, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(encoded, '\n'), 0o600))
}
