package main

import (
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_providers_bind(t *testing.T) {
	var bound []http.Handler

	calling := registration("calling")
	calling.Bind = func(h http.Handler) { bound = append(bound, h) }

	gateway := http.NewServeMux()

	p := &providers{regs: []service.Registration{registration("alone"), calling}}
	p.bind(gateway)

	require.Len(t, bound, 1, "the service that calls others is bound once, the other is skipped")
	assert.Same(t, gateway, bound[0])
}

func Test_registrations_bindS3(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	for _, reg := range newProviders(cfg, func(string) {}).regs {
		if reg.Metadata.Name == s3Service {
			assert.NotNil(t, reg.Bind, "S3 delivers its notifications through the gateway")
		}
	}
}
