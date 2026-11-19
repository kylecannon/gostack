# Exclusions

Tests the parity runner does not run, with the reason for each. One file per
service, `<service>.yaml`, named as `TEST_DIRS` in `parity/ledger.py` names the
service. A pattern lives in the file of the service whose tests it matches;
`parity/ledger.py check` fails on a pattern in another service's file.

Format, one per line: `"nodeid pattern": reason`. The pattern is double quoted
and the reason is plain text; `ledger.py` refuses any other line shape.

A pattern is a pytest nodeid, optionally with `*` wildcards. Without a wildcard
it matches that nodeid and every parametrization of it (`...::test_x` covers
`...::test_x[a]` and `...::test_x[b]`). `parity/pytest_gostack.py` loads every
file here, skips the matching tests with the reason `excluded: <reason>` and
records them in the ledger as `excluded` for both columns, so they stay visible.
A pattern that matches no collected test of a file the run collected prints
`pytest_gostack: exclusion matches nothing: <pattern>`.

Reasons in use:

| reason | meaning |
| --- | --- |
| `requires_in_process` | Marked so in LocalStack; it patches state inside the server process, which no external container can reach. A test carrying the marker is excluded unless `parity/bridgeable.yaml` lists it: either all it patches is `localstack.config`, which the bridge forwards, or it is marked only because it starts the runner's `HTTPServer`, which the target reaches through the shared network namespace. |
| `hard-skipped` | LocalStack's own source skips the test unconditionally. |
| `depends-on:<service>` | Needs a service outside the current scope. |
| `engine-deferred` | Needs a real engine (OpenSearch, Transcribe) that gostack defers. |
| `runner-docker` | Needs Docker from inside the runner. |

Adding an entry for a test that passes is a deferral, not a fix. parity-check
fails when a test that passes in the gostack column turns into an exclusion,
unless `parity/demotions.yaml` lists it.

A test whose result changes from run to run is excluded here with a reason
and demoted in `parity/demotions.yaml`, in the same change. Its row is never
left to flap: `parity.yml` fails a pull request whose committed gostack column
differs from what its run observed, so a flapping row fails unrelated pull
requests.
