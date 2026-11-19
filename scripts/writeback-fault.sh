#!/usr/bin/env bash
# Runs the internal/persist tests that need a filesystem whose data writes fail
# (assets-writeback_linux_internal_test.go), in a privileged Linux container
# under the pinned toolchain. Each test gets a fresh filesystem: ext4 on a loop
# device over a 12 MiB tmpfs, mounted at <base>/svc/assets, so an asset larger
# than the free space fails its writeback while the state file beside it, on the
# container's own filesystem, stays healthy. The tests read the base dir from
# GOSTACK_WRITEBACK_FAULT_BASE and skip without it.
#
# Usage: scripts/writeback-fault.sh [TEST...]
# With no TEST, it runs every test in that file. WRITEBACK_FAULT_NAME names the
# container (default gostack-writeback-fault-<pid>). Exits non-zero when a test
# fails or the filesystem cannot be made.
set -euo pipefail

tests=("$@")
if ((${#tests[@]} == 0)); then
	tests=(Test_Assets_SyncfsRetryAfterAFailedSave Test_Assets_SyncfsAbandonedThenNextSave)
fi

if [[ ${WRITEBACK_FAULT_INSIDE:-} != 1 ]]; then
	repo=$(cd "$(dirname "$0")/.." && pwd)
	toolchain=$(make -s -C "$repo" toolchain)
	exec docker run --rm --privileged \
		--name "${WRITEBACK_FAULT_NAME:-gostack-writeback-fault-$$}" \
		-e WRITEBACK_FAULT_INSIDE=1 -e GOTOOLCHAIN=local -e GOPROXY=off -e GOFLAGS=-mod=mod \
		-v "$repo":/src:ro -v "$(go env GOMODCACHE)":/go/pkg/mod:ro -w /src \
		"golang:${toolchain#go}" /src/scripts/writeback-fault.sh "${tests[@]}"
fi

if ! command -v mkfs.ext4 >/dev/null; then
	export DEBIAN_FRONTEND=noninteractive
	apt-get -qq update >/dev/null && apt-get -qq install -y e2fsprogs >/dev/null 2>&1
fi

back=/work/back
base=/work/base
loop=

teardown() {
	umount "$base/svc/assets" 2>/dev/null || true
	[[ -n $loop ]] && losetup -d "$loop" 2>/dev/null || true
	loop=
	umount "$back" 2>/dev/null || true
	rm -rf /work
}
trap teardown EXIT

status=0
for t in "${tests[@]}"; do
	mkdir -p "$back" "$base/svc/assets"
	mount -t tmpfs -o size=12m tmpfs "$back"
	truncate -s 128M "$back/img"
	loop=$(losetup -f --show "$back/img")
	mkfs.ext4 -q -F -N 2048 -J size=4 -E lazy_itable_init=0,lazy_journal_init=0,nodiscard "$loop"
	mount "$loop" "$base/svc/assets"
	echo "== $t: kernel $(uname -r), $(du -k "$back/img" | cut -f1) KiB of 12288 used after mkfs"

	GOSTACK_WRITEBACK_FAULT_BASE=$base go test -race -count=1 -v -run "^$t\$" ./internal/persist/ || status=1

	teardown
done

exit $status
