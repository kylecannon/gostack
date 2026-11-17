#!/usr/bin/env bash
# Print the Go packages that hold the .go files named as arguments, as ./dir
# paths joined by spaces on one line. The pre-commit manifest passes the result
# to make as PKGS.
#
# A package with no buildable files for the current GOOS and GOARCH is dropped:
# a directory that only holds a `//go:build ignore` file, or only _linux.go
# files on macOS, cannot be linted or tested here. Prints nothing when no
# package is left.
#
# Known gap: the hook passes only added, copied, modified and renamed paths
# (ACMR), so a commit that only deletes files from a package does not check
# that package. CI runs the whole tree and catches it.
set -euo pipefail

dirs=()
for f in "$@"; do
	d=$(dirname "$f")
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
