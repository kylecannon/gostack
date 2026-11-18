package main

import (
	"strings"
	"sync/atomic"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/gateway"
)

// settingsFunc maps the live configuration onto the gateway's own Settings. The
// gateway asks on every request, so the mapping is redone only when an update
// through /_localstack/config has replaced the snapshot.
//
// SHUTDOWN_TIMEOUT is not mapped. start reads it once and plans one budget from
// it, with four slots: the shutdown hooks, the drain, the save and the services'
// Close (lifecycle.PlanShutdown). The gateway keeps no timeout of its own, and
// SHUTDOWN_TIMEOUT=0 is no limit for all four.
func settingsFunc(cfg *config.Config) func() gateway.Settings {
	type mapped struct {
		from *config.Settings
		to   gateway.Settings
	}

	var last atomic.Pointer[mapped]

	return func() gateway.Settings {
		current := cfg.Settings()

		if m := last.Load(); m != nil && m.from == current {
			return m.to
		}

		m := &mapped{from: current, to: gatewaySettings(current)}
		last.Store(m)

		return m.to
	}
}

func gatewaySettings(s *config.Settings) gateway.Settings {
	listen := make([]string, 0, len(s.GatewayListen))
	for _, hostPort := range s.GatewayListen {
		listen = append(listen, hostPort.String())
	}

	return gateway.Settings{
		Listen:                     listen,
		CustomSSLCertPath:          s.CustomSSLCertPath,
		FailFast:                   s.FailFast,
		ResponseHeaderEnabled:      s.ResponseHeaderEnabled,
		ParityAWSAccessKeyID:       s.ParityAWSAccessKeyID,
		DisableCORSChecks:          s.DisableCORSChecks,
		DisableCORSHeaders:         s.DisableCORSHeaders,
		DisablePreflightProcessing: s.DisablePreflightProcessing,
		ExtraCORSAllowedOrigins:    strings.Join(s.ExtraCORSAllowedOrigins, ","),
		ExtraCORSAllowedHeaders:    strings.Join(s.ExtraCORSAllowedHeaders, ","),
		ExtraCORSExposeHeaders:     strings.Join(s.ExtraCORSExposeHeaders, ","),
		LocalstackHost:             s.LocalstackHost.Host,
	}
}
