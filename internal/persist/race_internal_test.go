//go:build race

package persist

// raceBuild is true when the test binary is built with the race detector, which
// allocates in places the plain build does not: the figures of an allocation test
// differ by it.
const raceBuild = true
