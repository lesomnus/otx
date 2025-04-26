#!/usr/bin/env bash

set -o errexit
set -o pipefail
set -o nounset
# set -o xtrace

__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" # Directory where this script exists.
__root="$(cd "$(dirname "${__dir}")" && pwd)"         # Root directory of project.



cd "$__root"
echo "[INFO] Generating go.work in $__root"

if [ -f "$__root/go.work" ]; then
  echo "[INFO] Removing existing go.work"
  rm "$__root/go.work"
fi

go work init
go work use .

find . -type f -name "go.mod" | while read -r modfile; do
	module_dir="$(dirname "$modfile")"
	echo "[INFO] Adding module: $module_dir"
	go work use "$module_dir"
done

echo "[INFO] go.work file has been generated"
