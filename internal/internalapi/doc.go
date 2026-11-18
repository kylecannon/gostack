// Package internalapi serves gostack's own HTTP surface: the canonical
// /_gostack/* endpoints, and the /_localstack/* aliases that keep LocalStack
// tooling working.
//
// [Mount] adds the routes to a chi router. Everything the handlers need from the
// rest of the process comes through [Deps], so the package depends on no service.
//
// The canonical contract is api/openapi/gostack.yaml. A compat alias answers
// with LocalStack's exact response shape, as specs/localstack/openapi.yaml
// describes it, and has one handler behind it with the canonical route's logic
// where the two do the same thing.
package internalapi
