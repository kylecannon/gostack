package main

import (
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sts"
)

// registrations lists the services the gateway serves. Each service ticket
// adds one line here. A service reads its settings through cfg on each call,
// so an update through /_localstack/config reaches it.
func registrations(cfg *config.Config) []service.Registration {
	return []service.Registration{
		sts.Registration(func() bool { return cfg.Settings().ParityAWSAccessKeyID }),
	}
}

// snapshotters lists the persistence hook of each service that keeps state, by
// botocore service name. A service ticket whose state persists adds one entry.
// A Snapshotter that also has Reset(account, region string) takes part in
// /_gostack/state/reset.
func snapshotters() map[string]persist.Snapshotter {
	return nil
}
