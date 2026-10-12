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
