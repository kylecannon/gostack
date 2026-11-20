package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodbstreams"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/kylecannon/gostack/internal/service/sts"
)

// s3Service is S3's name in the catalog and in PERSIST_*.
const s3Service = "s3"

// providers are the registered services. They are built once, because the
// gateway serves them, persistence saves them and the state routes reset them,
// and all three have to reach the same instance.
type providers struct {
	regs []service.Registration

	// s3 takes the persistence engine's assets as its body store once its state
	// has loaded (useAssets).
	s3 *s3.Provider
}

// newProviders builds the services. changed is how a service reports a change
// the gateway does not see, by its botocore name.
func newProviders(cfg *config.Config, changed func(name string)) *providers {
	p := &providers{s3: s3.New(liveSettings(cfg, s3Settings), blob.NewEphemeral(blob.DefaultSpillDir))}
	p.regs = registrations(cfg, changed, p.s3)

	return p
}

// registrations lists the services the gateway serves. Adding a service is one
// import and one line here; its Registration carries everything else. A service
// reads its settings through cfg on each call, so an update through
// /_localstack/config reaches it. Its settings mapping is a pure
// func(*config.Settings) T in its own file, settings-<svc>.go, which liveSettings
// wraps on its line here; this file holds no settings code, and parity selection
// reads a change to settings-<svc>.go as a change to that service alone. Keep one
// service to a line: parity selection reads a changed line of this file as a
// change to the services it names.
func registrations(cfg *config.Config, changed func(name string), s3Provider *s3.Provider) []service.Registration {
	// DynamoDB Streams reads the streams of this DynamoDB provider.
	ddb := dynamodb.New(liveSettings(cfg, dynamodbSettings))

	return []service.Registration{
		sqs.New(liveSettings(cfg, sqsSettings), changed).Registration(),
		sts.Registration(liveSettings(cfg, stsSettings)),
		s3Provider.Registration(),
		ddb.Registration(),
		dynamodbstreams.New(ddb).Registration(),
		secretsmanager.New(changed).Registration(),
		ssm.New().Registration(),
	}
}

// bind gives the services that call other services in this process the
// handler to call them through, normally the gateway's.
func (p *providers) bind(h http.Handler) {
	for i := range p.regs {
		if bindFn := p.regs[i].Bind; bindFn != nil {
			bindFn(h)
		}
	}
}

// closeGrace is how long close waits, once ctx has ended, for a Close that
// honours ctx to notice and return. A Close that stopped at the deadline returns
// within it; one that ignores ctx does not, and is the one left behind.
const closeGrace = 20 * time.Millisecond

// close stops what the services run on their own, such as SQS's message move
// tasks. Every Close runs at once, given ctx, so a slow one takes no time from
// the others. It waits until each has returned or ctx has ended, and for
// closeGrace more once it has. It returns the names of the services whose Close
// had not returned by then. Those cannot be stopped and are left behind, warned
// of by the caller: the process exits next. A Close that returned, even because
// ctx ended, is not one of them.
func (p *providers) close(ctx context.Context) []string {
	var (
		closing  sync.WaitGroup
		returned = make([]atomic.Bool, len(p.regs))
	)

	for i := range p.regs {
		if closeFn := p.regs[i].Close; closeFn != nil {
			closing.Go(func() {
				closeFn(ctx)
				returned[i].Store(true)
			})
		}
	}

	closed := make(chan struct{})

	go func() {
		closing.Wait()
		close(closed)
	}()

	select {
	case <-closed:
		return nil
	case <-ctx.Done():
	}

	grace := time.NewTimer(closeGrace)
	defer grace.Stop()

	select {
	case <-closed:
		return nil
	case <-grace.C:
	}

	var running []string

	for i := range p.regs {
		if p.regs[i].Close != nil && !returned[i].Load() {
			running = append(running, p.regs[i].Metadata.Name)
		}
	}

	return running
}

// liveSettings maps the live configuration onto the settings of a component
// with mapTo. The component asks on every call, so the mapping is redone only
// when an update through /_localstack/config has replaced the snapshot.
func liveSettings[T any](cfg *config.Config, mapTo func(*config.Settings) T) func() T {
	type mapped struct {
		from *config.Settings
		to   T
	}

	var last atomic.Pointer[mapped]

	return func() T {
		current := cfg.Settings()

		if m := last.Load(); m != nil && m.from == current {
			return m.to
		}

		m := &mapped{from: current, to: mapTo(current)}
		last.Store(m)

		return m.to
	}
}

// persist hands eng the services whose state is saved. The engine leaves out
// the ones PERSIST_* does not enable. A persisted S3's assets are opened here:
// after S3 is registered and before its state loads, so the load can sweep
// them. It returns them, nil when S3 is not persisted or its assets directory
// is unsafe or impossible to make, such as on a read-only base dir: the engine
// has logged it and neither loads nor saves S3. Only an S3 the engine cannot
// give assets to at all, a wiring error, stops the start.
func (p *providers) persist(eng *persist.Engine) (*persist.Assets, error) {
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
		return nil, fmt.Errorf("persist s3: %w", err)
	default:
	}

	return assets, nil
}

// useAssets makes S3 keep its bodies as assets once the load has run, and
// only when the engine still persists S3: one whose assets could not be opened,
// or whose state file the load refused, keeps its bodies in the ephemeral
// store, since nothing would save or sweep them, and health reports S3 as not
// persisted. It runs before S3 serves.
func (p *providers) useAssets(eng *persist.Engine, assets *persist.Assets) {
	if assets == nil {
		return
	}

	if !eng.Enabled(s3Service) {
		slog.Error("s3 bodies are not persisted: its state file was refused")

		return
	}

	p.s3.UseBlobs(persist.NewBlobStore(assets))
}
