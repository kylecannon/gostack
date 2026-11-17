#!/usr/bin/env bash
# Print the unique ./dir package path for each .go file named as an argument.
# The pre-commit manifest uses it to scope lint and test to the staged files.
set -euo pipefail

for f in "$@"; do
	d=$(dirname "$f")
	if [[ $d == . ]]; then
		echo .
	else
		echo "./$d"
	fi
done | sort -u
