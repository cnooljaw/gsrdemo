import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { DemoClient } from "./client.js";
import { EnvelopeSchema } from "./gen/gsr/common/v1/common_pb.js";
import { BalanceSchema, BalancePushSchema, LoginResponseSchema, MsgId, SnapshotSchema } from "./gen/demo/v1/game_pb.js";

// 仅替代传输，业务请求/推送仍经过实际 Envelope 编解码和 DemoClient 分发。
class FakeSocket extends EventTarget {
  static readonly OPEN = 1;
  static readonly CONNECTING = 0;
  static last: FakeSocket;
  readyState = 0;
  binaryType = "";
  readonly requests: number[] = [];
  private first = true;
  constructor(_url: string) {
    super();
    FakeSocket.last = this;
    queueMicrotask(() => { this.readyState = 1; this.dispatchEvent(new Event("open")); });
  }
  send(bytes: Uint8Array): void {
    if (this.first) {
      this.first = false;
      const handshake = new Uint8Array(33);
      handshake[0] = 1;
      handshake[1] = 9; // X25519 的公开基点，仅用于协议测试。
      queueMicrotask(() => this.dispatchEvent(new MessageEvent("message", { data: handshake.buffer })));
    } else {
      this.requests.push(fromBinary(EnvelopeSchema, bytes.subarray(8, -16)).requestId);
    }
  }
  envelope(msgId: number, requestId: number, payload: Uint8Array): void {
    const bytes = toBinary(EnvelopeSchema, create(EnvelopeSchema, { msgId, requestId, payload }));
    this.dispatchEvent(new MessageEvent("message", { data: Uint8Array.from(bytes).buffer }));
  }
  close(): void {
    this.readyState = 3;
    this.dispatchEvent(new Event("close"));
  }
}

const originalSocket = globalThis.WebSocket;
beforeEach(() => { globalThis.WebSocket = FakeSocket as unknown as typeof WebSocket; });
afterEach(() => { globalThis.WebSocket = originalSocket; });

// 保护推送与响应交错时仍按 request_id 匹配，旧余额和重复修订不能重复修改显示。
test("推送与响应交错，余额按修订收敛并保留确认版本", async () => {
  const client = await DemoClient.connect("ws://fixture", 1000);
  try {
    client.enter(create(SnapshotSchema, { attackName: "attack" }), create(BalanceSchema, { balance: 1000n, confirmed: { amount: 1000n, version: 8n }, revision: 1n }));
    const pending = client.request(MsgId.C2S_LOGIN, new Uint8Array(), MsgId.S2C_LOGIN, LoginResponseSchema);
    const wire = FakeSocket.last;
    wire.envelope(MsgId.S2C_BALANCE_PUSH, 0, toBinary(BalancePushSchema, create(BalancePushSchema, { silver: { balance: 900n, revision: 3n } })));
    wire.envelope(MsgId.S2C_BALANCE_PUSH, 0, toBinary(BalancePushSchema, create(BalancePushSchema, { silver: { balance: 1n, revision: 2n } })));
    wire.envelope(MsgId.S2C_LOGIN, wire.requests[0]!, toBinary(LoginResponseSchema, create(LoginResponseSchema, { userId: 7n })));
    assert.equal((await pending).userId, 7n);
    assert.equal(client.silver?.balance, 900n);
    assert.equal(client.silver?.confirmed?.version, 8n);
    assert.equal(client.silver?.confirmed?.amount, 1000n);
    assert.equal(client.applyBalance(create(BalanceSchema, { balance: 5n, revision: 3n })), false);
  } finally {
    client.close();
  }
});

// 保护超时后的迟到响应不能完成新请求，且客户端不自动重发未知操作。
test("迟到响应不串入新请求，超时不重发", async () => {
  const client = await DemoClient.connect("ws://fixture", 20);
  try {
    await assert.rejects(client.request(MsgId.C2S_LOGIN, new Uint8Array(), MsgId.S2C_LOGIN, LoginResponseSchema), /outcome may be unknown/);
    const wire = FakeSocket.last;
    assert.equal(wire.requests.length, 1);
    const pending = client.request(MsgId.C2S_LOGIN, new Uint8Array(), MsgId.S2C_LOGIN, LoginResponseSchema);
    wire.envelope(MsgId.S2C_LOGIN, wire.requests[0]!, toBinary(LoginResponseSchema, create(LoginResponseSchema, { userId: 1n })));
    wire.envelope(MsgId.S2C_LOGIN, wire.requests[1]!, toBinary(LoginResponseSchema, create(LoginResponseSchema, { userId: 2n })));
    assert.equal((await pending).userId, 2n);
    assert.equal(wire.requests.length, 2);
  } finally {
    client.close();
  }
});

// 保护关闭连接时结束所有等待，而不是一直保留到每个请求超时。
test("连接关闭结束在途等待", async () => {
  const client = await DemoClient.connect("ws://fixture", 1000);
  const pending = client.request(MsgId.C2S_LOGIN, new Uint8Array(), MsgId.S2C_LOGIN, LoginResponseSchema);
  client.close();
  await assert.rejects(pending, /client closed/);
});
