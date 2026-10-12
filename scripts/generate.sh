#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOCACHE="${GOCACHE:-$project_root/.cache/go-build}"
export GOWORK=off
mkdir -p "$project_root/bin/tools" "$project_root/.cache"
(
    cd "$project_root/common"
    go build -mod=mod -o "$project_root/bin/tools/protoc-gen-go" google.golang.org/protobuf/cmd/protoc-gen-go
    go build -mod=mod -o "$project_root/bin/tools/protoc-gen-gsr-registry" github.com/cnooljaw/gsr/protocol/cmd/protoc-gen-gsr-registry
)
# schema 依赖在临时目录组合，只读参考源，所有生成输出仍在 gsrdemo。
proto_input="$(mktemp -d "$project_root/.cache/proto-input.XXXXXX")"
trap 'rm -rf "$proto_input"' EXIT
cp -R "$project_root/../gsr/protocol/proto/." "$proto_input/"
cp -R "$project_root/common/proto/." "$proto_input/"
export PATH="$project_root/bin/tools:$project_root/typeclient/node_modules/.bin:$PATH"
cd "$project_root"
buf generate "$proto_input" --template scripts/buf.gen.go.yaml --path "$proto_input/demo/diagnostic/v1/diagnostic.proto"
buf generate "$proto_input" --template scripts/buf.gen.ts.yaml
node scripts/normalize-generated.mjs
