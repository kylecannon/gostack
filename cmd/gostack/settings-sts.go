package main

import "github.com/kylecannon/gostack/internal/config"

// stsSettings maps the configuration onto the one setting of the STS
// registration: whether it issues the access key id the parity run asks for.
func stsSettings(current *config.Settings) bool {
	return current.ParityAWSAccessKeyID
}
