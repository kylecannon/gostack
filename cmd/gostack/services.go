package main

import (
	"sync/atomic"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/kylecannon/gostack/internal/service/sts"
)

// serviceSQS is the botocore name SQS registers and persists under.
const serviceSQS = "sqs"

// providers are the services that keep state. They are built once, because the
// gateway serves them through registrations and persistence saves them through
// snapshotters, and both have to reach the same instance.
type providers struct {
	sqs *sqs.Provider
}

func newProviders(cfg *config.Config) *providers {
	return &providers{sqs: sqs.New(sqsSettings(cfg))}
}

// registrations lists the services the gateway serves. Each service ticket
// adds one line here. A service reads its settings through cfg on each call,
// so an update through /_localstack/config reaches it.
func registrations(cfg *config.Config, p *providers) []service.Registration {
	return []service.Registration{
		p.sqs.Registration(),
		sts.Registration(func() bool { return cfg.Settings().ParityAWSAccessKeyID }),
	}
}

// close stops what the services run on their own: SQS's message move tasks.
// It is called after the gateway has drained and before the engine's last flush,
// so that a task's final state is saved.
func (p *providers) close() {
	p.sqs.Close()
}

// markDirty makes the calls that persistence cannot see tell eng that a service
// changed. The gateway reports every call it serves itself, but a call made to
// an SQS queue URL goes through the routes SQS mounts, so SQS reports those. SQS
// also reports a ReceiveMessage that changed state and the work of a move task.
func (p *providers) markDirty(eng *persist.Engine) {
	p.sqs.OnChange(func() { eng.MarkDirty(serviceSQS) })
}

// sqsSettings maps the live configuration onto the settings of the SQS
// provider. The provider asks on every call, so the mapping is redone only when
// an update through /_localstack/config has replaced the snapshot.
func sqsSettings(cfg *config.Config) func() sqs.Settings {
	type mapped struct {
		from *config.Settings
		to   sqs.Settings
	}

	var last atomic.Pointer[mapped]

	return func() sqs.Settings {
		current := cfg.Settings()

		if m := last.Load(); m != nil && m.from == current {
			return m.to
		}

		m := &mapped{from: current, to: sqs.Settings{
			EndpointStrategy:     current.SQSEndpointStrategy,
			DelayRecentlyDeleted: current.SQSDelayRecentlyDeleted,
			DelayPurgeRetry:      current.SQSDelayPurgeRetry,
			Host:                 current.LocalstackHost.String(),
			ParityAWSAccessKeyID: current.ParityAWSAccessKeyID,
			DisableCompatPaths:   !current.CompatPaths,

			EnableMessageRetentionPeriod:   current.SQSEnableMessageRetentionPeriod,
			DisableMaxNumberOfMessageLimit: current.SQSDisableMaxNumberOfMessageLimit,
		}}
		last.Store(m)

		return m.to
	}
}

// snapshotters lists the persistence hook of each service that keeps state, by
// botocore service name. A service ticket whose state persists adds one entry.
// A Snapshotter that also has Reset(account, region string) takes part in
// /_gostack/state/reset.
func snapshotters(p *providers) map[string]persist.Snapshotter {
	return map[string]persist.Snapshotter{
		serviceSQS: p.sqs,
	}
}
