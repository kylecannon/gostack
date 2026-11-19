# Tech landscape

*Last Updated: 2026-11-18*

- Language: Go (`go.mod`: go 1.27.0, toolchain go1.27.4; `make -s toolchain` prints it). Python only in `parity/` (pytest plugin, ledger tooling) and `internal/protocol/query/testdata/werkzeug-form.py`.
- Build and gate: `Makefile`; lint is golangci-lint v2 strict mode (`.golangci.yml`, run from `tools/go.mod`). Standards: `docs/standards.md`.
- Dependency table and rationale: `docs/architecture.md` "Dependencies". In `go.mod`:

| Group | Modules |
| --- | --- |
| HTTP | go-chi/chi v5, go-chi/cors |
| Config | spf13/viper |
| JSON | stdlib encoding/json/v2, used by `internal/protocol/awsjson` |
| Test only | testify, aws-sdk-go-v2 (+ credentials, service/sqs), santhosh-tekuri/jsonschema/v6, go.yaml.in/yaml/v3 |

- Containers: `docker/Dockerfile` (full), `docker/Dockerfile.slim`; `make image`, `make image-slim`. Image name `ghcr.io/kylecannon/gostack`. Parity runner image: `parity/runner/Dockerfile`.
- Stdlib `time/tzdata` is embedded in the binary (S3 Object Lock error text names a time zone).
- Specs: `specs/botocore` (vendored per service), `specs/localstack` (patches and sqs-query), pinned in `specs/SOURCES.lock`, refreshed by `scripts/vendor-specs.sh`.
- Generated code: `internal/api/` from `make gen`; `make gen-check` fails on drift. botocore rest-xml and rest-json protocol fixtures (`internal/protocol/testdata/botocore`) also generate packages under `internal/protocol/{restxml,restjson}/fixturegen_test`.
- Parity: `docs/parity.md` (generated), `parity/exclusions.yaml`, `parity/ledger.json`.
- Fuzz: `make fuzz`; seeds in `testdata/fuzz` (awsjson, query, sqs restore).
