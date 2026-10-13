#!/usr/bin/env bash
# 用途：统一执行构建、代码检查或真实多进程测试，逐个 Go module 操作。
# 用法：dev.sh build [--race] | check | test [--race]
# build 输出节点二进制和 TS；check 执行 Go test/vet 与 TS 类型检查；test 构建后启动临时测试进程。
# 执行范围：仅 gsrdemo；使用 go.mod 固定的 GSR 版本与本地 common，复用 .cache/go-build，不依赖 go.work。
# 依赖：Go、Node/npm，以及已安装的 typeclient/node_modules；test 需要本机端口。
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOCACHE="${GOCACHE:-$project_root/.cache/go-build}"
export GOWORK=off

usage() {
    echo "用法：$0 build [--race] | check | test [--race]"
}

action="${1:-}"
if [[ "$action" == "--help" || "$action" == "-h" ]]; then
    usage
    exit 0
fi
race=false
if [[ "$#" -eq 2 && "$2" == "--race" && ( "$action" == "build" || "$action" == "test" ) ]]; then
    race=true
elif [[ "$#" -ne 1 ]]; then
    usage >&2
    exit 2
fi
if [[ "$action" != "build" && "$action" != "check" && "$action" != "test" ]]; then
    usage >&2
    exit 2
fi
if [[ ! -d "$project_root/typeclient/node_modules" ]]; then
    echo "缺少客户端依赖，请执行 npm --prefix typeclient ci。" >&2
    exit 1
fi

build() {
    local output="$1"
    mkdir -p "$output"
    for module_directory in "$project_root/common" "$project_root"/nodes/* "$project_root"/tests/*; do
        [[ -f "$module_directory/go.mod" ]] || continue
        echo "构建 ${module_directory#"$project_root/"}"
        (
            cd "$module_directory"
            if [[ "$module_directory" == "$project_root"/nodes/* ]]; then
                go build -race="$race" -o "$output/$(basename "$module_directory")" .
            else
                go build -race="$race" ./...
            fi
        )
    done
    npm --prefix "$project_root/typeclient" run build
}

case "$action" in
    check)
        for module_directory in "$project_root/common" "$project_root"/nodes/* "$project_root"/tests/*; do
            [[ -f "$module_directory/go.mod" ]] || continue
            echo "检查 ${module_directory#"$project_root/"}"
            (cd "$module_directory" && GSR_PROCESS_TEST=0 go test ./... -timeout=5m && go vet ./...)
        done
        npm --prefix "$project_root/typeclient" run check
        ;;
    build|test)
        output="$project_root/bin"
        [[ "$race" == true ]] && output="$output/race"
        build "$output"
        if [[ "$action" == test ]]; then
            cd "$project_root/tests/process"
            GSR_NODE_BIN="$output" GSR_PROCESS_TEST=1 go test -race="$race" -run '^Test(FiveNodeProbe|MultiplayerMole)$' -timeout=1m -v
        fi
        ;;
esac
