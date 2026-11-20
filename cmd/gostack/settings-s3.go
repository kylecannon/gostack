package main

import (
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/service/s3"
)

// s3Settings maps the configuration onto the settings of the S3 provider.
func s3Settings(current *config.Settings) s3.Settings {
	settings := s3.Settings{
		LocalstackHost:     current.LocalstackHost.String(),
		ValidateSignatures: !current.S3SkipSignatureValidation,
		ValidateKMSKeys:    !current.S3SkipKMSKeyValidation,
	}
	if len(current.GatewayListen) > 0 {
		settings.GatewayPort = current.GatewayListen[0].Port
	}

	return settings
}
