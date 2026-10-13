#!/usr/bin/env bash
# 用途：从 common/proto 生成 Go、协议 registry 和 TypeScript，再统一 TS 生成文件末尾空行。
# 用法：generate.sh，无参数；依赖 Go 和 typeclient/node_modules 中锁定的 Buf/TS 插件。
# 执行范围：只读 common/go.mod 选定的 GSR 模块，工具、临时目录及生成结果都写入 gsrdemo。
set -euo pipefail
if [[ "$#" -ne 0 ]]; then
    echo "用法：$0" >&2
    exit 2
fi
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export GOCACHE="${GOCACHE:-$project_root/.cache/go-build}"
export GOWORK=off
mkdir -p "$project_root/bin/tools" "$project_root/.cache"
(
    cd "$project_root/common"
    go build -mod=mod -o "$project_root/bin/tools/protoc-gen-go" google.golang.org/protobuf/cmd/protoc-gen-go
    go build -mod=mod -o "$project_root/bin/tools/protoc-gen-gsr-registry" github.com/cnooljaw/gsr/protocol/cmd/protoc-gen-gsr-registry
)
gsr_root="$(cd "$project_root/common" && go list -m -f '{{.Dir}}' github.com/cnooljaw/gsr)"
# schema 与 registry 工具使用同一 GSR 依赖；临时输入和生成输出都在 gsrdemo。
proto_input="$(mktemp -d "$project_root/.cache/proto-input.XXXXXX")"
# 模块缓存通常只读，清理前只给本次临时副本补上写权限。
trap 'chmod -R u+w "$proto_input"; rm -rf "$proto_input"' EXIT
cp -R "$gsr_root/protocol/proto/." "$proto_input/"
cp -R "$project_root/common/proto/." "$proto_input/"
export PATH="$project_root/bin/tools:$project_root/typeclient/node_modules/.bin:$PATH"
cd "$project_root"
buf generate "$proto_input" --template scripts/buf.gen.go.yaml --path "$proto_input/demo"
buf generate "$proto_input" --template scripts/buf.gen.ts.yaml
node --input-type=module <<'JS'
import { readdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";

// 只统一生成结果的末尾空行，不改写协议或手工修改类型。
async function normalize(directory) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      await normalize(path);
    } else if (entry.name.endsWith(".ts")) {
      const source = await readFile(path, "utf8");
      await writeFile(path, source.replace(/\n+$/, "\n"));
    }
  }
}
await normalize("typeclient/src/gen");
JS
