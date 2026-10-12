#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOCACHE="${GOCACHE:-$project_root/.cache/go-build}"
export GOWORK=off
if [[ "$#" -eq 0 ]]; then
    "$project_root/scripts/build.sh" --standalone
    race=false
elif [[ "$#" -eq 1 && "$1" == "--race" ]]; then
    export GSR_NODE_BIN="$project_root/bin/race"
    mkdir -p "$GSR_NODE_BIN"
    for role in access login online mole settlement; do
        (cd "$project_root/nodes/$role" && go build -race -o "$GSR_NODE_BIN/$role" "./cmd/$role")
    done
    (cd "$project_root/nodes/login" && go build -race -o "$GSR_NODE_BIN/account" ./cmd/account)
    npm --prefix "$project_root/typeclient" run build
    race=true
else
    echo "用法：$0 [--race]" >&2
    exit 2
fi
cd "$project_root/tests/process"
GSR_PROCESS_TEST=1 go test -race="$race" -run '^Test(FiveNodeProbe|MultiplayerMole)$' -timeout=1m -v
