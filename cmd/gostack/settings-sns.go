package main

import (
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/service/sns"
)

// snsSettings maps the configuration onto the settings of the SNS provider.
func snsSettings(current *config.Settings) sns.Settings {
	return sns.Settings{
		Host:                current.LocalstackHost.String(),
		CertURLHost:         current.SNSCertURLHost,
		SESSenderAddress:    current.SNSSESSenderAddress,
		LegacyGCMPublishing: current.LegacySNSGCMPublishing,
		DisableCompatPaths:  !current.CompatPaths,
	}
}
