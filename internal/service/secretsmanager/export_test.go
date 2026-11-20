package secretsmanager

import "context"

// SetFunctionLookup replaces the lookup RotateSecret asks whether a rotation
// function exists, so a test can get past it before Lambda is served.
func SetFunctionLookup(p *Provider, exists func(ctx context.Context, account, region, functionARN string) bool) {
	p.functionExists = exists
}
