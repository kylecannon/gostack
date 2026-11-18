package route

import "github.com/kylecannon/gostack/internal/service"

// KnownServices is the table of services gostack does not serve, for the test
// that keeps it in step with the vendored specs.
func KnownServices() []service.Metadata { return knownServices }
