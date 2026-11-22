//go:build !race

package persist

// raceBuild is true when the test binary is built with the race detector (see
// race_internal_test.go).
const raceBuild = false
