import { create, toBinary } from "@bufbuild/protobuf";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { setTimeout as delay } from "node:timers/promises";
import { DemoClient } from "./client.js";
import {
  EnterRequestSchema, EnterResponseSchema, HitRequestSchema, HitResponseSchema,
  LeaveRequestSchema, LeaveResponseSchema, LoginRequestSchema, LoginResponseSchema,
  MsgId, OperationRequestSchema, OperationResponseSchema, SnapshotRequestSchema, SnapshotResponseSchema,
} from "./gen/demo/v1/game_pb.js";

function print(event: string, value: unknown): void {
  console.log(JSON.stringify({ event, value }, (_, item) => typeof item === "bigint" ? item.toString() : item));
}

function options(): { url: string; username: string; hits: number; timeout: number } {
  const parsed = { url: "ws://127.0.0.1:7888/ws", username: "demo", hits: 1, timeout: 5_000 };
  const args = process.argv.slice(2);
  for (let index = 0; index < args.length; index += 2) {
    const value = args[index + 1];
    if (!value) throw new Error(`missing value for ${args[index]}`);
    switch (args[index]) {
      case "--url": parsed.url = value; break;
      case "--username": parsed.username = value; break;
      case "--hits": parsed.hits = Number(value); break;
      case "--timeout-ms": parsed.timeout = Number(value); break;
      default: throw new Error(`unknown argument ${args[index]}`);
    }
  }
  if (!Number.isSafeInteger(parsed.hits) || parsed.hits < 1 || parsed.hits > 100) throw new Error("--hits must be 1..100");
  if (!Number.isSafeInteger(parsed.timeout) || parsed.timeout < 1) throw new Error("--timeout-ms must be positive");
  return parsed;
}

async function run(): Promise<void> {
  const config = options();
  // 管理入口和客户端均从 stdin 读取密码；不把密码放命令行或日志。
  const password = readFileSync(0, "utf8").replace(/\r?\n$/, "");
  if (Buffer.byteLength(password) < 1 || Buffer.byteLength(password) > 128) throw new Error("stdin password must contain 1..128 bytes");
  const client = await DemoClient.connect(config.url, config.timeout);
  client.onPush = print;
  try {
    const login = await client.request(MsgId.C2S_LOGIN,
      toBinary(LoginRequestSchema, create(LoginRequestSchema, { username: config.username, password })),
      MsgId.S2C_LOGIN, LoginResponseSchema);
    print("login", login);
    const entry = await client.request(MsgId.C2S_ENTER, toBinary(EnterRequestSchema, create(EnterRequestSchema)), MsgId.S2C_ENTER, EnterResponseSchema);
    if (!entry.snapshot || !entry.silver) throw new Error("missing entry snapshot/balance");
    client.enter(entry.snapshot, entry.silver);
    print("enter", entry);
    for (let hitIndex = 0; hitIndex < config.hits; hitIndex++) {
      const targetDeadline = Date.now() + config.timeout;
      let target = client.snapshot?.targets.find((item) => !item.claimed && item.deadlineMs > BigInt(Date.now() + 50));
      while (!target && Date.now() < targetDeadline) {
        await delay(100);
        const view = await client.request(MsgId.C2S_SNAPSHOT, toBinary(SnapshotRequestSchema, create(SnapshotRequestSchema)), MsgId.S2C_SNAPSHOT, SnapshotResponseSchema);
        client.snapshot = view.snapshot;
        target = view.snapshot?.targets.find((item) => !item.claimed && item.deadlineMs > BigInt(Date.now() + 50));
      }
      if (!target) throw new Error("no target available");
      const actionID = randomUUID();
      print("action", { actionId: actionID, spawnSeq: target.spawnSeq, hole: target.hole });
      const hit = await client.request(MsgId.C2S_HIT,
        toBinary(HitRequestSchema, create(HitRequestSchema, { actionId: actionID, spawnSeq: target.spawnSeq, hole: target.hole })),
        MsgId.S2C_HIT, HitResponseSchema);
      if (!hit.hit) throw new Error("missing hit");
      if (hit.hit.status !== "accepted") {
        print("hit", hit);
        throw new Error("memory hit rejected; balance unchanged");
      }
      if (!hit.silver) throw new Error("missing hit balance");
      client.applyBalance(hit.silver);
      print("hit", hit); // 此处立即展示；查询入账不是奖励反馈的前置步骤。
      const receiptDeadline = Date.now() + Math.max(10_000, config.timeout);
      let found = false;
      while (Date.now() < receiptDeadline) {
        const receipt = await client.request(MsgId.C2S_OPERATION,
          toBinary(OperationRequestSchema, create(OperationRequestSchema, { hitId: hit.hit.hitId })),
          MsgId.S2C_OPERATION, OperationResponseSchema);
        if (receipt.found) {
          print("settlement", receipt);
          if (receipt.status !== "committed") throw new Error(`settlement rejected: ${receipt.reason}`);
          found = true;
          break;
        }
        await delay(100);
      }
      if (!found) throw new Error(`settlement not confirmed; query original HitID ${hit.hit.hitId}`);
      const view = await client.request(MsgId.C2S_SNAPSHOT, toBinary(SnapshotRequestSchema, create(SnapshotRequestSchema)), MsgId.S2C_SNAPSHOT, SnapshotResponseSchema);
      client.snapshot = view.snapshot;
    }
    const leave = await client.request(MsgId.C2S_LEAVE, toBinary(LeaveRequestSchema, create(LeaveRequestSchema)), MsgId.S2C_LEAVE, LeaveResponseSchema);
    print("leave", leave);
  } finally {
    client.close();
  }
}

try {
  await run();
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
}
