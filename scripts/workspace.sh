#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -e "$project_root/go.work" ]]; then
    echo "已有 go.work，保留现有配置。"
    exit 0
fi
cp "$project_root/go.work.example" "$project_root/go.work"
echo "已创建本地 go.work；只组装本仓库 module，GSR 由各节点 go.mod 引用本地只读基线。"
