#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_root"
pids=()
cleanup() {
    trap - EXIT INT TERM
    [[ ${#pids[@]} -eq 0 ]] && return
    for pid in "${pids[@]}"; do kill -TERM "$pid" 2>/dev/null || true; done
    for ((attempt=0; attempt<100; attempt++)); do
        active=0
        for pid in "${pids[@]}"; do if kill -0 "$pid" 2>/dev/null; then active=1; fi; done
        [[ "$active" -eq 0 ]] && break
        sleep 0.05
    done
    for pid in "${pids[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid" 2>/dev/null || true; fi
        wait "$pid" 2>/dev/null || true
    done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
for role in login online mole settlement access; do
    [[ -x "bin/$role" ]] || { echo "请先执行 scripts/build.sh" >&2; exit 1; }
    mkdir -p "logs/$role-01"
    "bin/$role" -config "configs/$role-01.json" >"logs/$role-01/stdout.log" 2>"logs/$role-01/stderr.log" &
    pids+=("$!")
    ready=0
    for ((attempt=0; attempt<100; attempt++)); do
        if rg -q '"event":"ready"' "logs/$role-01/stdout.log"; then ready=1; break; fi
        kill -0 "${pids[${#pids[@]}-1]}" 2>/dev/null || break
        sleep 0.05
    done
    [[ "$ready" -eq 1 ]] || { cat "logs/$role-01/stderr.log" >&2; echo "$role 未就绪" >&2; exit 1; }
    echo "$role 就绪"
done
echo "五节点运行中；Ctrl-C 停止本次启动的全部进程。"
while sleep 1; do
    for pid in "${pids[@]}"; do
        kill -0 "$pid" 2>/dev/null || { echo "节点退出，停止本次进程组。" >&2; exit 1; }
    done
done
