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

function createSettlement() {
  return create(LandlordSettlementSchema, { status: "committed", operationId: "round" });
}
