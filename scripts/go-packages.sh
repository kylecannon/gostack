#!/usr/bin/env bash
# Print the Go packages that hold the .go files named as arguments, as ./dir
# paths joined by spaces on one line. The pre-commit manifest reaches it through
# `make -s packages FILES=...`, so it runs under the pinned toolchain, and
# passes the result to make as PKGS. Exits non-zero when go list fails.
#
# A directory with a path element named testdata or starting with _ or . is
# dropped, matching what ./... skips. A package with no buildable files for the current GOOS and GOARCH is dropped:
# a directory that only holds a `//go:build ignore` file, or only _linux.go
# files on macOS, cannot be linted or tested here. Prints nothing when no
# package is left.
#
# Known gap: the hook passes only added, copied, modified and renamed paths
# (ACMR), and a rename is reported under its new path only. A commit that
# deletes files from a package, or moves them out with `git mv`, does not check
# the package they left. CI runs the whole tree and catches it.
set -euo pipefail

dirs=()
for f in "$@"; do
	d=$(dirname "$f")
	# ./... skips a directory with a path element named testdata or starting
	# with _ or ., so those files are not part of any package we lint or test.
	skip=0
	IFS=/ read -ra elems <<<"$d"
	for e in "${elems[@]}"; do
		case $e in
		. | '') ;;
		testdata | _* | .*) skip=1 ;;
		esac
	done
	((skip)) && continue
	[[ $d == . ]] || d=./$d
	dirs+=("$d")
done
((${#dirs[@]})) || exit 0

module=$(go list -m)

# go list -e keeps the packages it cannot build and reports them with empty
# file lists, which the template filters out. Strip the module path to get the
# ./dir form back.
printf '%s\n' "${dirs[@]}" | sort -u |
	xargs go list -e -f '{{if or .GoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' |
	sed -e '/^$/d' -e "s|^$module\$|.|" -e "s|^$module/|./|" |
	sort -u | paste -sd' ' -
