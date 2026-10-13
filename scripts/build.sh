#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# 复用本项目缓存；允许调用者显式指定已有 GOCACHE。
export GOCACHE="${GOCACHE:-$project_root/.cache/go-build}"
if [[ "${1:-}" == "--standalone" && "$#" -eq 1 ]]; then
    export GOWORK=off
elif [[ "$#" -eq 0 ]]; then
    if [[ ! -f "$project_root/go.work" ]]; then
        echo "请先执行 scripts/workspace.sh，或使用 --standalone 验证固定依赖。" >&2
        exit 1
    fi
    export GOWORK="$project_root/go.work"
else
    echo "用法：$0 [--standalone]" >&2
    exit 2
fi
# 仅枚举本项目 module；后续 landlord 或 tests module 无需修改枚举逻辑。
for module_directory in "$project_root/common" "$project_root"/nodes/* "$project_root"/tests/*; do
    [[ -f "$module_directory/go.mod" ]] || continue
    echo "检查 ${module_directory#"$project_root/"}"
    (
        cd "$module_directory"
        if [[ "$module_directory" == "$project_root"/nodes/* ]]; then
            mkdir -p "$project_root/bin"
            go build -o "$project_root/bin/$(basename "$module_directory")" .
        else
            go build ./...
        fi
    )
done
if [[ ! -d "$project_root/typeclient/node_modules" ]]; then
    echo "缺少客户端依赖，请在 typeclient 执行 npm ci 后重试。" >&2
    exit 1
fi
npm --prefix "$project_root/typeclient" run build
