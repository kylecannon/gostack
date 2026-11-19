#!/usr/bin/env bash
# Runs the internal/persist tests that need a filesystem whose data writes fail
# (assets-writeback_linux_internal_test.go and the fault-filesystem check in
# assets-sync_linux_internal_test.go), in a privileged Linux container under the
# pinned toolchain. Each test gets a fresh filesystem on a loop device over a
# tmpfs left with 6 MiB free after mkfs, mounted at <base>/svc/assets, so an
# asset larger than that fails its writeback while the state file beside it, on
# the container's own filesystem, stays healthy. The tests read the base dir
# from GOSTACK_WRITEBACK_FAULT_BASE and the filesystem's name from
# GOSTACK_WRITEBACK_FAULT_FS, and skip without them.
#
# Usage: scripts/writeback-fault.sh [TEST...]
# With no TEST, it runs every test of the two files.
#
# WRITEBACK_FAULT_FS lists the filesystems to run each test on, default every
# one: ext4, overlay (an overlayfs whose upper dir is on the faulty
# ext4, as a container's own layer is), xfs and btrfs. GOSTACK_WRITEBACK_FAULT_SYNCFS=1
# makes the tests sync by syncfs whatever the filesystem, to qualify one for
# the syncfs allowlist (syncfsProven).
# WRITEBACK_FAULT_SIDECAR=1 runs another container beside the tests that loops
# sync(2) the whole time, as a process outside gostack's container would.
# WRITEBACK_FAULT_NAME names the container (default
# gostack-writeback-fault-<pid>); the sidecar is <name>-sidecar.
# Exits non-zero when a test fails or a filesystem cannot be made.
set -euo pipefail

tests=("$@")
if ((${#tests[@]} == 0)); then
	tests=(
		Test_Assets_SyncPathOfTheFaultFilesystem
		Test_Assets_SyncfsRetryAfterAFailedSave
		Test_Assets_SyncfsAbandonedThenNextSave
		Test_Assets_ForeignSyncfsFirst
		Test_Assets_ForeignGlobalSyncFirst
		Test_Assets_SaveAfterAFailedWriteback
	)
fi

if [[ ${WRITEBACK_FAULT_INSIDE:-} != 1 ]]; then
	repo=$(cd "$(dirname "$0")/.." && pwd)
	toolchain=$(make -s -C "$repo" toolchain)
	image="golang:${toolchain#go}"
	name=${WRITEBACK_FAULT_NAME:-gostack-writeback-fault-$$}

	if [[ ${WRITEBACK_FAULT_SIDECAR:-} == 1 ]]; then
		trap 'docker rm -f "$name-sidecar" >/dev/null 2>&1 || true' EXIT
		docker run -d --rm --name "$name-sidecar" "$image" \
			sh -c 'while :; do sync; sleep 0.05; done' >/dev/null
		echo "== sidecar $name-sidecar loops sync(2)"
	fi

	docker run --rm --privileged --name "$name" \
		-e WRITEBACK_FAULT_INSIDE=1 -e WRITEBACK_FAULT_FS="${WRITEBACK_FAULT_FS:-ext4 overlay xfs btrfs}" \
		-e GOSTACK_WRITEBACK_FAULT_SYNCFS="${GOSTACK_WRITEBACK_FAULT_SYNCFS:-}" \
		-e GOTOOLCHAIN=local -e GOPROXY=off -e GOFLAGS=-mod=mod \
		-v "$repo":/src:ro -v "$(go env GOMODCACHE)":/go/pkg/mod:ro -w /src \
		"$image" /src/scripts/writeback-fault.sh "${tests[@]}"
	exit
fi

read -r -a filesystems <<<"$WRITEBACK_FAULT_FS"

need=()
for fs in "${filesystems[@]}"; do
	case $fs in
	ext4 | overlay) command -v mkfs.ext4 >/dev/null || need+=(e2fsprogs) ;;
	xfs) command -v mkfs.xfs >/dev/null || need+=(xfsprogs) ;;
	btrfs) command -v mkfs.btrfs >/dev/null || need+=(btrfs-progs) ;;
	*)
		echo "unknown filesystem $fs" >&2
		exit 2
		;;
	esac
done

if ((${#need[@]} > 0)); then
	export DEBIAN_FRONTEND=noninteractive
	apt-get -qq update >/dev/null && apt-get -qq install -y "${need[@]}" >/dev/null 2>&1
fi

(cd /src && go test -c -race -o /tmp/persist.test ./internal/persist/)

back=/work/back
base=/work/base
lower=/work/lower
outer=/work/outer
loop=

teardown() {
	umount "$base/svc/assets" 2>/dev/null || true
	umount "$outer" 2>/dev/null || true
	[[ -n $loop ]] && losetup -d "$loop" 2>/dev/null || true
	loop=
	umount "$back" 2>/dev/null || true
	rm -rf /work
}
trap teardown EXIT

# mkfault makes filesystem $1 at $2 on a loop device over a tmpfs, and leaves
# the tmpfs 6 MiB free.
mkfault() {
	local fs=$1 at=$2 size=128M
	case $fs in
	xfs) size=512M ;; # mkfs.xfs refuses a filesystem under 300 MB
	btrfs) size=256M ;;
	esac

	mkdir -p "$back" "$at"
	mount -t tmpfs -o size=256m tmpfs "$back"
	truncate -s "$size" "$back/img"
	loop=$(losetup -f --show "$back/img")

	case $fs in
	ext4) mkfs.ext4 -q -F -N 2048 -J size=4 -E lazy_itable_init=0,lazy_journal_init=0,nodiscard "$loop" ;;
	xfs) mkfs.xfs -q -f -K "$loop" ;;
	btrfs) mkfs.btrfs -q -f -K "$loop" ;;
	esac

	local used
	used=$(du -k "$back/img" | cut -f1)
	mount -o remount,size=$((used + 6144))k "$back"
	mount "$loop" "$at"
	echo "   $fs: $used KiB used after mkfs, 6144 KiB free"
}

status=0
for fs in "${filesystems[@]}"; do
	for t in "${tests[@]}"; do
		mkdir -p "$base/svc/assets"
		if [[ $fs == overlay ]]; then
			mkfault ext4 "$outer"
			mkdir -p "$lower" "$outer/upper" "$outer/work"
			mount -t overlay overlay -o "lowerdir=$lower,upperdir=$outer/upper,workdir=$outer/work" "$base/svc/assets"
		else
			mkfault "$fs" "$base/svc/assets"
		fi

		echo "== [$fs] $t: kernel $(uname -r), mounted $(stat -f -c %T "$base/svc/assets")"
		(cd /src/internal/persist &&
			GOSTACK_WRITEBACK_FAULT_BASE=$base GOSTACK_WRITEBACK_FAULT_FS=$fs \
				/tmp/persist.test -test.count=1 -test.v -test.run "^$t\$") || status=1

		teardown
	done
done

exit $status
