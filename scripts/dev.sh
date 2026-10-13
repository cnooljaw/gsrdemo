#!/usr/bin/env bash
# 用途：构建、检查工程骨架，或前台启动一个指定节点。
# 用法：dev.sh build|check [access|login|online|mole|settlement]；dev.sh run <节点>。
# 不指定节点时，build/check 包含所有 Go module 和 TypeScript；指定时只处理 common 和该节点。
# 执行范围：仅 gsrdemo；GOWORK=off，复用 .cache/go-build，构建输出到 bin/ 和 typeclient/dist/。
# 依赖：Go；检查/构建 TypeScript 另需 Node/npm 和已安装的 typeclient/node_modules。
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOCACHE="${GOCACHE:-$project_root/.cache/go-build}"
export GOWORK=off

usage() {
    echo "用法：$0 build|check [access|login|online|mole|settlement] | run <节点>"
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
    build|check|run) ;;
    *) usage >&2; exit 2 ;;
esac
case "$node" in
    ""|access|login|online|mole|settlement) ;;
    *) usage >&2; exit 2 ;;
esac

if [[ "$action" == "run" ]]; then
    [[ -n "$node" ]] || { usage >&2; exit 2; }
    [[ -x "$project_root/bin/$node" ]] || { echo "请先执行 $0 build $node" >&2; exit 1; }
    exec "$project_root/bin/$node" -config "$project_root/configs/$node-01.yaml"
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
