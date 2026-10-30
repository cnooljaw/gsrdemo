#!/usr/bin/env bash
# 用途：构建、检查项目、前台启动节点或执行独立结算历史压测。
# 用法：dev.sh build|check [access|login|online|mole|settlement]；dev.sh run <节点>；dev.sh history settlement；dev.sh generate access。
# run 优先读取 configs/<节点>-01.local.yaml，未配置时读取公开模板。
# 不指定节点时，build/check 包含所有 Go module 和 TypeScript；指定时只处理 common 和该节点。
# 执行范围：仅 gsrdemo；GOWORK=off，复用 .cache/go-build，构建输出到 bin/ 和 typeclient/dist/。
# 依赖：Go；TypeScript 检查/构建和协议生成另需 Node/npm 与 typeclient/node_modules。
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOCACHE="${GOCACHE:-$project_root/.cache/go-build}"
export GOWORK=off

usage() {
    echo "用法：$0 build|check [access|login|online|mole|settlement] | run <节点> | history settlement | generate access"
}

action="${1:-}"
node="${2:-}"
if [[ "$action" == "--help" || "$action" == "-h" ]]; then
    usage
    exit 0
fi
if [[ "$#" -lt 1 || "$#" -gt 2 ]]; then
    usage >&2
    exit 2
fi
case "$action" in
    build|check|run|history|generate) ;;
    *) usage >&2; exit 2 ;;
esac
case "$node" in
    ""|access|login|online|mole|settlement) ;;
    *) usage >&2; exit 2 ;;
esac

if [[ "$action" == "generate" ]]; then
    [[ "$node" == "access" ]] || { usage >&2; exit 2; }
    # 只读发布模块的 schema；临时输入和全部生成产物均归本项目。
    mkdir -p "$project_root/.cache"
    schema_source="$(go -C "$project_root/nodes/access" list -m -f '{{.Dir}}' github.com/cnooljaw/gsr)"
    protocol_input="$(mktemp -d "$project_root/.cache/protocol-XXXXXX")"
    trap 'rm -rf "$protocol_input"' EXIT
    cp -R "$schema_source/protocol/proto/." "$protocol_input/"
    chmod -R u+w "$protocol_input"
    cp -R "$project_root/nodes/access/proto/." "$protocol_input/"
    mkdir -p "$project_root/.cache/tools/bin"
    go -C "$project_root/nodes/access" build -o "$project_root/.cache/tools/bin/protoc-gen-go" google.golang.org/protobuf/cmd/protoc-gen-go
    go -C "$project_root/nodes/access" build -o "$project_root/.cache/tools/bin/protoc-gen-gsr-registry" github.com/cnooljaw/gsr/protocol/cmd/protoc-gen-gsr-registry
    export PATH="$project_root/.cache/tools/bin:$project_root/typeclient/node_modules/.bin:$PATH"
    cd "$project_root"
    buf generate "$protocol_input" --template typeclient/buf.gen.yaml --path "$protocol_input/demo/v1/game.proto"
    buf generate "$protocol_input" --path "$protocol_input/gsr/common/v1/common.proto" \
        --template '{"version":"v2","plugins":[{"local":"protoc-gen-es","out":"typeclient/src/gen","include_imports":true,"opt":["target=ts","import_extension=js"]}]}'
    # 与框架示例一样，统一生成文件末尾换行，避免每次生成产生空白检查错误。
    node --input-type=module <<'JS'
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
for (const name of readdirSync("typeclient/src/gen", { recursive: true })) {
    if (!name.endsWith(".ts")) continue;
    const path = `typeclient/src/gen/${name}`;
    writeFileSync(path, readFileSync(path, "utf8").trimEnd() + "\n");
}
JS
    exit 0
fi
if [[ "$action" == "history" ]]; then
    [[ "$node" == "settlement" ]] || { usage >&2; exit 2; }
    mkdir -p "$project_root/bin"
    go -C "$project_root/nodes/settlement" build -o "$project_root/bin/settlement" .
    export GSRDEMO_SETTLEMENT_HISTORY=1
    exec go -C "$project_root/nodes/settlement" test -count=1 -run '^TestSettlementHistory$' -v -timeout=24h .
fi
if [[ "$action" == "run" ]]; then
    [[ -n "$node" ]] || { usage >&2; exit 2; }
    [[ -x "$project_root/bin/$node" ]] || { echo "请先执行 $0 build $node" >&2; exit 1; }
    node_config="$project_root/configs/$node-01.yaml"
    if [[ -f "$project_root/configs/$node-01.local.yaml" ]]; then
        node_config="$project_root/configs/$node-01.local.yaml"
    fi
    exec "$project_root/bin/$node" -config "$node_config"
fi
if [[ -z "$node" && ! -d "$project_root/typeclient/node_modules" ]]; then
    echo "缺少 TypeScript 依赖，请执行 npm --prefix typeclient ci。" >&2
    exit 1
fi

modules=("$project_root/common")
if [[ -n "$node" ]]; then
    modules+=("$project_root/nodes/$node")
else
    modules+=("$project_root"/nodes/*)
fi
for module_directory in "${modules[@]}"; do
    echo "$action ${module_directory#"$project_root/"}"
    (
        cd "$module_directory"
        if [[ "$action" == "check" ]]; then
            go test ./... -timeout=5m
            go vet ./...
        elif [[ "$module_directory" == "$project_root"/nodes/* ]]; then
            mkdir -p "$project_root/bin"
            go build -o "$project_root/bin/$(basename "$module_directory")" .
        else
            go build ./...
        fi
    )
done
if [[ -z "$node" ]]; then
    npm --prefix "$project_root/typeclient" run "$action"
fi
