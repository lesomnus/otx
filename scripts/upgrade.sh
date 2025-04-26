#!/usr/bin/env bash

set -o errexit
set -o pipefail
set -o nounset
# set -o xtrace

__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" # Directory where this script exists.
__root="$(cd "$(dirname "${__dir}")" && pwd)"         # Root directory of project.


VERSION="${1:-latest}"


cd "$__root"

ROOT_MODULE_PATH=$(grep "^module " go.mod | awk '{print $2}')
if [ -z "$ROOT_MODULE_PATH" ]; then
	echo "Error: Could not determine root module path from go.mod"
	exit 1
fi

for dir in exporters/*; do
	p="$__root/$dir"
	if [ -d "$p" ] && [ -f "$p/go.mod" ]; then
		echo "$p"
		cd "$p"
		go get "$ROOT_MODULE_PATH@$VERSION"
		go mod tidy
	fi
done
