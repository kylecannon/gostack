#!/usr/bin/env bash
# Runner entrypoint: add the *.localhost.localstack.cloud entries to /etc/hosts
# (Docker rewrites that file per container, so the image cannot carry them),
# optionally wait for the target's health endpoint, then run the command.
#
#   GOSTACK_WAIT_HEALTH   URL to poll until it answers 200 (unset: no wait)
#   GOSTACK_WAIT_SECONDS  how long to poll (default 180)
#
# Exits 97 when the health endpoint never answers.
set -euo pipefail

cat /etc/gostack-hosts >>/etc/hosts

if [ -n "${GOSTACK_WAIT_HEALTH:-}" ]; then
	python - "$GOSTACK_WAIT_HEALTH" "${GOSTACK_WAIT_SECONDS:-180}" <<'EOF' || exit 97
import sys, time, urllib.request

url, budget = sys.argv[1], float(sys.argv[2])
deadline = time.monotonic() + budget
while True:
    try:
        with urllib.request.urlopen(url, timeout=3) as resp:
            if resp.status == 200:
                sys.exit(0)
    except OSError:
        pass
    if time.monotonic() > deadline:
        print(f"{url} did not answer 200 within {budget:.0f}s", file=sys.stderr)
        sys.exit(1)
    time.sleep(0.5)
EOF
fi

exec "$@"
