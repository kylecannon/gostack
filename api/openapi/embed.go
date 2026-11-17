package openapi

import _ "embed" // for the go:embed directive

// Spec is the canonical OpenAPI contract of the internal API, gostack.yaml.
//
//go:embed gostack.yaml
var Spec []byte
