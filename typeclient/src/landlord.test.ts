import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { DemoClient } from "./client.js";
import { runLandlord } from "./landlord.js";
import { LandlordSnapshotSchema, LandlordSettlementSchema, MsgId } from "./gen/demo/v1/game_pb.js";

// 保护重新进入正在进行的牌局时不发送 Ready，也没有 90 秒整局限制。
test("斗地主重入继续当前阶段", async () => {
  const calls: number[] = [];
  const snapshot = create(LandlordSnapshotSchema, { roomName: "table", phase: "playing", seat: 1n, turnSeat: 0n, turn: 7n, players: [{ userId: 1n, seat: 1n }] });
  const client = {
    landlord: snapshot,
    landlordNeedsSync: false,
    setLandlord(view: typeof snapshot) { this.landlord = view; },
    leaveLandlord() {},
    landlordRecoveryWait() { return 1; },
    async waitLandlord() {
      this.landlord.phase = "finished";
      this.landlord.settlement = createSettlement();
      return true;
    },
    async request(id: number) {
      calls.push(id);
      if (id === MsgId.C2S_LANDLORD_ENTER) return { snapshot };
      if (id === MsgId.C2S_LANDLORD_LEAVE) return {};
      throw new Error(`unexpected request ${id}`);
    },
  };
  const oldNow = Date.now;
  let now = 0;
  Date.now = () => { now += 100_000; return now; };
  try {
    await runLandlord(client as unknown as DemoClient, () => {});
  } finally {
    Date.now = oldNow;
  }
  assert.equal(calls.includes(MsgId.C2S_LANDLORD_READY), false);
  assert.ok(calls.includes(MsgId.C2S_LANDLORD_LEAVE));
});

// 保护动作结果未知时只恢复场景，不重发相同 Turn 的出牌。
test("斗地主动作失败后恢复而不重复提交", async () => {
  const calls: number[] = [];
  const snapshot = create(LandlordSnapshotSchema, { roomName: "table", phase: "playing", seat: 0n, turnSeat: 0n, turn: 3n, canAct: true, hand: [3n], players: [{ userId: 1n, seat: 0n }] });
  const client = {
    landlord: snapshot,
    landlordNeedsSync: false,
    setLandlord(view: typeof snapshot) { this.landlord = view; },
    leaveLandlord() {},
    landlordRecoveryWait() { return 1; },
    async waitLandlord() {
      this.landlord.phase = "finished";
      this.landlord.settlement = createSettlement();
      return true;
    },
    async request(id: number) {
      calls.push(id);
      if (id === MsgId.C2S_LANDLORD_ENTER || id === MsgId.C2S_LANDLORD_SNAPSHOT) return { snapshot };
      if (id === MsgId.C2S_LANDLORD_PLAY) throw new Error("unknown outcome");
      if (id === MsgId.C2S_LANDLORD_LEAVE) return {};
      throw new Error(`unexpected request ${id}`);
    },
  };
  await runLandlord(client as unknown as DemoClient, () => {});
  assert.equal(calls.filter(id => id === MsgId.C2S_LANDLORD_PLAY).length, 1);
  assert.ok(calls.includes(MsgId.C2S_LANDLORD_SNAPSHOT));
});

// 保护三次无人叫地主后机器人重新准备，并且在事件尚未到达时不重复准备。
test("斗地主回到等待阶段后重新准备一次", async () => {
  const calls: number[] = [];
  const snapshot = create(LandlordSnapshotSchema, { roomName: "table", phase: "calling", seat: 0n, turn: 3n, players: [{ userId: 1n, seat: 0n, ready: true }] });
  let waits = 0;
  const client = {
    landlord: snapshot,
    landlordNeedsSync: false,
    setLandlord(view: typeof snapshot) { this.landlord = view; },
    leaveLandlord() {},
    landlordRecoveryWait() { return 1; },
    async waitLandlord() {
      waits++;
      if (waits === 1) {
        this.landlord.phase = "waiting";
        this.landlord.turn = 12n;
        this.landlord.players[0]!.ready = false;
      } else if (waits === 3) {
        this.landlord.phase = "finished";
        this.landlord.settlement = createSettlement();
      }
      return true;
    },
    async request(id: number) {
      calls.push(id);
      if (id === MsgId.C2S_LANDLORD_ENTER) return { snapshot };
      if (id === MsgId.C2S_LANDLORD_READY || id === MsgId.C2S_LANDLORD_LEAVE) return {};
      throw new Error(`unexpected request ${id}`);
    },
  };
  await runLandlord(client as unknown as DemoClient, () => {});
  assert.equal(calls.filter(id => id === MsgId.C2S_LANDLORD_READY).length, 1);
});

// 保护准备回包丢失时先恢复场景：已准备则继续，未准备则报告错误而非反复提交。
test("斗地主准备结果未知时通过场景确认", async () => {
  for (const accepted of [true, false]) {
    const calls: number[] = [];
    const snapshot = create(LandlordSnapshotSchema, { roomName: "table", phase: "waiting", seat: 0n, turn: 3n, players: [{ userId: 1n, seat: 0n }] });
    const recovered = create(LandlordSnapshotSchema, { roomName: "table", phase: "waiting", seat: 0n, turn: 3n, players: [{ userId: 1n, seat: 0n, ready: accepted }] });
    const client = {
      landlord: snapshot,
      landlordNeedsSync: false,
      setLandlord(view: typeof snapshot) { this.landlord = view; },
      leaveLandlord() {},
      landlordRecoveryWait() { return 1; },
      async waitLandlord() {
        this.landlord.phase = "finished";
        this.landlord.settlement = createSettlement();
        return true;
      },
      async request(id: number) {
        calls.push(id);
        if (id === MsgId.C2S_LANDLORD_ENTER) return { snapshot };
        if (id === MsgId.C2S_LANDLORD_READY) throw new Error("ready outcome unknown");
        if (id === MsgId.C2S_LANDLORD_SNAPSHOT) return { snapshot: recovered };
        if (id === MsgId.C2S_LANDLORD_LEAVE) return {};
        throw new Error(`unexpected request ${id}`);
      },
    };
    if (accepted) await runLandlord(client as unknown as DemoClient, () => {});
    else await assert.rejects(runLandlord(client as unknown as DemoClient, () => {}), /ready outcome unknown/);
    assert.equal(calls.filter(id => id === MsgId.C2S_LANDLORD_READY).length, 1);
    assert.equal(calls.filter(id => id === MsgId.C2S_LANDLORD_SNAPSHOT).length, 1);
  }
});

function createSettlement() {
  return create(LandlordSettlementSchema, { status: "committed", operationId: "round" });
}
