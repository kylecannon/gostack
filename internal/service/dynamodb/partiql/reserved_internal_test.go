package partiql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test_reserved_noAllocation: the keyword check compares without a case-folded
// copy, so a keyword in lower case costs no more than one in upper case.
func Test_reserved_noAllocation(t *testing.T) {
	for _, w := range reservedWords {
		require.True(t, reserved(w), w)
		require.True(t, reserved(strings.ToLower(w)), w)
	}

	for _, w := range []string{"", "a", "selec", "selects", "totally", "ide", "ALLS", "x"} {
		require.False(t, reserved(w), w)
	}

	for _, w := range []string{"select", "where", "SELECT", "ident"} {
		require.Zero(t, testing.AllocsPerRun(100, func() { _ = reserved(w) }), w)
	}

	// The two differ in the case of their keywords alone.
	lower := testing.AllocsPerRun(1000, func() { _, _ = Parse("select a from t where id = 'a'") })
	upper := testing.AllocsPerRun(1000, func() { _, _ = Parse("SELECT a FROM t WHERE id = 'a'") })
	require.LessOrEqual(t, lower, upper, "a statement in lower case allocates no more than one in upper case")
}
