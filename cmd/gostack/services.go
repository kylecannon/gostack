package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/kylecannon/gostack/internal/service/sts"
)

// s3Service is S3's name in the catalog and in PERSIST_*.
const s3Service = "s3"

// providers are the registered services. They are built once, because the
// gateway serves them, persistence saves them and the state routes reset them,
// and all three have to reach the same instance.
type providers struct {
	regs []service.Registration

	// s3 delivers its notifications through the gateway, which is built from
	// regs and so does not exist yet: bind hands it over.
	s3 *s3.Provider
}

// newProviders builds the services. changed is how a service reports a change
// the gateway does not see, by its botocore name.
func newProviders(cfg *config.Config, changed func(name string)) *providers {
	p := &providers{s3: s3.New(s3Settings(cfg), blob.NewEphemeral(blob.DefaultSpillDir))}
	p.regs = registrations(cfg, changed, p.s3)

	return p
}

// bind gives the services that call other services in this process the
// handler to call them through, normally the gateway's.
func (p *providers) bind(h http.Handler) {
	p.s3.Bind(h)
}

// registrations lists the services the gateway serves. Adding a service is one
// line here; its Registration carries everything else. A service reads its
// settings through cfg on each call, so an update through /_localstack/config
// reaches it.
func registrations(cfg *config.Config, changed func(name string), s3Provider *s3.Provider) []service.Registration {
	return []service.Registration{
		sqs.New(sqsSettings(cfg), changed).Registration(),
		sts.Registration(func() bool { return cfg.Settings().ParityAWSAccessKeyID }),
		s3Provider.Registration(),
	}
}

// s3Settings maps the live configuration onto the settings of the S3 provider.
// S3 asks for them on every request, to claim its hosts, so the mapping is
// redone only when an update through /_localstack/config has replaced the
// snapshot.
func s3Settings(cfg *config.Config) func() s3.Settings {
	type mapped struct {
		from *config.Settings
		to   s3.Settings
	}

	var last atomic.Pointer[mapped]

	return func() s3.Settings {
		current := cfg.Settings()

		if m := last.Load(); m != nil && m.from == current {
			return m.to
		}

		m := &mapped{from: current, to: s3.Settings{
			LocalstackHost:     current.LocalstackHost.String(),
			ValidateSignatures: !current.S3SkipSignatureValidation,
		}}
		if len(current.GatewayListen) > 0 {
			m.to.GatewayPort = current.GatewayListen[0].Port
		}
		last.Store(m)

		return m.to
	}
}

// close stops what the services run on their own, such as SQS's message move
// tasks. It is called after the gateway has drained and before the engine's
// last flush, so that a task's final state is saved.
func (p *providers) close() {
	for i := range p.regs {
		if closeFn := p.regs[i].Close; closeFn != nil {
			closeFn()
		}
	}
}

// persist hands eng the services whose state is saved. The engine leaves out
// the ones PERSIST_* does not enable. A persisted S3 then keeps its bodies as
// the engine's assets, opened here: after S3 is registered, and before its
// state loads or is first saved, so no body is written anywhere else. With S3
// not persisted, or its assets directory unsafe or impossible to make, such as
// on a read-only base dir, the bodies stay in the ephemeral store: the engine
// has logged it, neither loads nor saves S3, and health reports S3 as not
// persisted. Only an S3 the engine cannot give assets to at all, a wiring
// error, stops the start.
func (p *providers) persist(eng *persist.Engine) error {
	for i := range p.regs {
		if snapshotter := p.regs[i].Persist; snapshotter != nil {
			eng.Register(p.regs[i].Metadata.Name, snapshotter)
		}
	}

	assets, err := eng.Assets(s3Service)

	switch {
	case errors.Is(err, persist.ErrUnsafe):
		slog.Error("s3 bodies are not persisted", "error", err)
	case err != nil:
		return fmt.Errorf("persist s3: %w", err)
	case assets != nil:
		p.s3.UseBlobs(persist.NewBlobStore(assets))
	default:
	}

	return nil
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
