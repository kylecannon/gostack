package main

import (
	"strings"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/gateway"
)

// settingsFunc maps the live configuration onto the gateway's own Settings. The
// gateway asks on every request, so the mapping is redone only when an update
// through /_localstack/config has replaced the snapshot.
//
// SHUTDOWN_TIMEOUT is not mapped. start reads it once and plans one budget from
// it (lifecycle.PlanShutdown): the shutdown hooks and the drain of requests,
// then the save's reserve, which holds the first save, the drain of the
// in-process calls and the second save, then the services' Close. The gateway
// keeps no timeout of its own. SHUTDOWN_TIMEOUT=0 is no limit for all of them,
// except that the in-process calls still queued are cut at once.
func settingsFunc(cfg *config.Config) func() gateway.Settings {
	return liveSettings(cfg, gatewaySettings)
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
		DisableCustomCORS:          s.CustomCORSDisabled(),
	}
}
