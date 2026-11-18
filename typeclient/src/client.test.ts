import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { DemoClient } from "./client.js";
import { EnvelopeSchema } from "./gen/gsr/common/v1/common_pb.js";
import { LandlordSeatPushSchema, LandlordPlayPushSchema, LandlordTurnPushSchema, LandlordSnapshotSchema, BalanceSchema, BalancePushSchema, LoginResponseSchema, MsgId, SnapshotSchema } from "./gen/demo/v1/game_pb.js";

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

// 保护交错消息按 request_id 匹配，余额按修订收敛，身份、金额和版本保留 64 位精度。
test("交错响应与余额推送保持修订顺序及 64 位精度", async () => {
  const amount = 9_223_372_036_854_775_807n;
  const maxUint64 = 18_446_744_073_709_551_615n;
  const client = await DemoClient.connect("ws://fixture", 1000);
  try {
    client.enter(create(SnapshotSchema, { attackName: "attack" }), create(BalanceSchema, { balance: amount, confirmed: { amount, version: maxUint64 }, revision: maxUint64 - 2n }));
    const pending = client.request(MsgId.C2S_LOGIN, new Uint8Array(), MsgId.S2C_LOGIN, LoginResponseSchema);
    const wire = FakeSocket.last;
    wire.envelope(MsgId.S2C_BALANCE_PUSH, 0, toBinary(BalancePushSchema, create(BalancePushSchema, { silver: { balance: amount - 100n, revision: maxUint64 } })));
    wire.envelope(MsgId.S2C_BALANCE_PUSH, 0, toBinary(BalancePushSchema, create(BalancePushSchema, { silver: { balance: 1n, revision: maxUint64 - 1n } })));
    wire.envelope(MsgId.S2C_LOGIN, wire.requests[0]!, toBinary(LoginResponseSchema, create(LoginResponseSchema, { userId: maxUint64 })));
    assert.equal((await pending).userId, maxUint64);
    assert.equal(client.silver?.balance, amount - 100n);
    assert.equal(client.silver?.confirmed?.version, maxUint64);
    assert.equal(client.silver?.confirmed?.amount, amount);
    assert.equal(client.applyBalance(create(BalanceSchema, { balance: 5n, revision: maxUint64 })), false);
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


// 保护同桌事件按序应用，旧事件和其他桌事件不覆盖当前场景。
test("斗地主事件按牌桌序号应用并恢复缺口", async () => {
  const client = await DemoClient.connect("ws://fixture", 1000);
  try {
    client.setLandlord(create(LandlordSnapshotSchema, { roomName: "table-a", seat: 0n, phase: "playing", turn: 1n, revision: 3n, hand: [3n, 4n], players: [{ userId: 1n, seat: 0n, remaining: 2n }] }));
    const play = (roomName: string, revision: bigint, card: bigint) => FakeSocket.last.envelope(MsgId.S2C_LANDLORD_PLAY_PUSH, 0,
      toBinary(LandlordPlayPushSchema, create(LandlordPlayPushSchema, { roomName, revision, userId: 1n, cards: [card], remaining: 1n, phase: "playing", turn: 2n, turnSeat: 1n })));
    const seat = (roomName: string, revision: bigint) => FakeSocket.last.envelope(MsgId.S2C_LANDLORD_SEAT_PUSH, 0,
      toBinary(LandlordSeatPushSchema, create(LandlordSeatPushSchema, { roomName, revision, userId: 2n, seat: 1n, joined: true })));
    seat("other", 4n);
    play("table-a", 5n, 3n);
    assert.equal(client.landlordNeedsSync, true);
    assert.deepEqual(client.landlord?.hand, [3n, 4n]);
    seat("table-a", 4n);
    assert.equal(client.landlordNeedsSync, false);
    assert.equal(client.landlord?.revision, 5n);
    assert.deepEqual(client.landlord?.hand, [4n]);
    play("table-a", 5n, 3n);
    assert.deepEqual(client.landlord?.hand, [4n]);
    client.leaveLandlord();
  } finally { client.close(); }
});

// 保护恢复场景不会丢掉请求期间已收到的更新，也不会应用场景已经包含的旧事件。
test("斗地主恢复场景后接续较新事件", async () => {
  const client = await DemoClient.connect("ws://fixture", 1000);
  try {
    client.setLandlord(create(LandlordSnapshotSchema, { roomName: "table", revision: 4n }));
    const send = (revision: bigint, userId: bigint) => FakeSocket.last.envelope(MsgId.S2C_LANDLORD_SEAT_PUSH, 0,
      toBinary(LandlordSeatPushSchema, create(LandlordSeatPushSchema, { roomName: "table", revision, userId, seat: BigInt(userId - 1n), joined: true })));
    send(7n, 3n);
    assert.equal(client.landlordNeedsSync, true);
    client.setLandlord(create(LandlordSnapshotSchema, { roomName: "table", revision: 6n, players: [{ userId: 1n, seat: 0n }, { userId: 2n, seat: 1n }] }));
    assert.equal(client.landlordNeedsSync, false);
    assert.equal(client.landlord?.revision, 7n);
    assert.deepEqual(client.landlord?.players.map(player => player.userId), [1n, 2n, 3n]);
    send(6n, 9n);
    assert.deepEqual(client.landlord?.players.map(player => player.userId), [1n, 2n, 3n]);
  } finally { client.close(); }
});

// 保护等待恢复的预算由服务端时间差计算，不受本机日历时钟偏差影响。
test("斗地主恢复等待使用服务端时间", async () => {
  const client = await DemoClient.connect("ws://fixture", 1000);
  try {
    client.setLandlord(create(LandlordSnapshotSchema, { roomName: "table", phase: "playing", revision: 1n, canAct: true, serverMs: 1_000_000n, deadlineMs: 1_020_000n }));
    const original = Date.now;
    let first: number, second: number;
    try {
      Date.now = () => 1;
      first = client.landlordRecoveryWait();
      Date.now = () => 9_000_000_000;
      second = client.landlordRecoveryWait();
    } finally { Date.now = original; }
    assert.ok(first > 20_000 && first <= 21_000);
    assert.ok(Math.abs(first - second) < 100);
  } finally { client.close(); }
});

// 保护动画期间的场景保持不可操作，只由服务器轮次事件开放。
test("斗地主轮次由服务器事件开放", async () => {
  const client = await DemoClient.connect("ws://fixture", 1000);
  try {
    client.setLandlord(create(LandlordSnapshotSchema, { roomName: "table", phase: "calling", revision: 1n, canAct: false, startMs: 4_000n, serverMs: 1_000n }));
    assert.ok(client.landlordRecoveryWait() > 3_500);
    FakeSocket.last.envelope(MsgId.S2C_LANDLORD_TURN_PUSH, 0, toBinary(LandlordTurnPushSchema,
      create(LandlordTurnPushSchema, { roomName: "table", revision: 2n, phase: "calling", turn: 1n, turnSeat: 0n, serverMs: 4_000n, deadlineMs: 14_000n })));
    assert.equal(client.landlord?.canAct, true);
    assert.equal(client.landlord?.startMs, 0n);
    assert.ok(client.landlordRecoveryWait() > 10_500);
  } finally { client.close(); }
});
