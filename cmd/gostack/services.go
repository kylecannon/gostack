package main

import (
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
)

// registrations lists the services the gateway serves. Each service ticket
// adds one line here, for example `sts.Registration(...)`.
func registrations() []service.Registration {
	return nil
}

// snapshotters lists the persistence hook of each service that keeps state, by
// botocore service name. A service ticket whose state persists adds one entry.
// A Snapshotter that also has Reset(account, region string) takes part in
// /_gostack/state/reset.
func snapshotters() map[string]persist.Snapshotter {
	return nil
}
