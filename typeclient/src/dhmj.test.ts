import { create, fromBinary } from "@bufbuild/protobuf";
import assert from "node:assert/strict";
import { test } from "node:test";
import type { DemoClient } from "./client.js";
import { runDhmj } from "./dhmj.js";
import { DhmjScene, type DhmjEvent } from "./dhmj_state.js";
import {
  DhmjDiscardPushSchema, DhmjDiscardRequestSchema, DhmjDrawPushSchema, DhmjFlowerPushSchema,
  DhmjRoundEndPushSchema, DhmjSettlementPushSchema, DhmjSnapshotSchema, DhmjTurnPushSchema, MsgId,
} from "./gen/demo/v1/game_pb.js";

function sceneSnapshot() {
  return create(DhmjSnapshotSchema, {
    tableName: "table", phase: "playing", seat: 0n, revision: 5n, hand: [1n, 2n],
    players: [{ userId: 7n, seat: 0n }], discards: [{}, {}, {}, {}],
  });
}

test("麻将缺号时恢复快照并接续较新的摸牌和轮次", () => {
  const scene = new DhmjScene(() => {});
  scene.restore(sceneSnapshot());
  scene.accept({ kind: "draw", value: create(DhmjDrawPushSchema, { tableName: "table", revision: 7n, seat: 0n, tile: 3n }) });
  scene.accept({ kind: "turn", value: create(DhmjTurnPushSchema, { tableName: "table", revision: 8n, seat: 0n, phase: "discard", canDiscard: true, turn: 4n }) });
  assert.equal(scene.needsSync, true);
  assert.deepEqual(scene.view?.hand, [1n, 2n]);
  scene.restore(create(DhmjSnapshotSchema, { ...sceneSnapshot(), revision: 6n, hand: [2n] }));
  assert.equal(scene.needsSync, false);
  assert.equal(scene.view?.revision, 8n);
  assert.equal(scene.view?.canDiscard, true);
  assert.deepEqual(scene.view?.hand, [2n, 3n]);
});

test("麻将补花和出牌事件只更新本人的暗手", () => {
  const scene = new DhmjScene(() => {});
  scene.restore(create(DhmjSnapshotSchema, { ...sceneSnapshot(), hand: [1n, 0x41n] }));
  scene.accept({ kind: "flower", value: create(DhmjFlowerPushSchema, {
    tableName: "table", revision: 6n, seat: 0n, flowers: [0x41n, 0x42n], replacements: [0x42n, 5n],
  }) });
  scene.accept({ kind: "discard", value: create(DhmjDiscardPushSchema, { tableName: "table", revision: 7n, seat: 1n, tile: 1n }) });
  assert.deepEqual(scene.view?.hand, [1n, 5n]);
  assert.deepEqual(scene.view?.discards[1]?.tiles, [1n]);
});

test("麻将出牌结果未知时恢复同一轮次也只提交一次", async () => {
  const sent: MsgId[] = [];
  const snapshot = create(DhmjSnapshotSchema, { ...sceneSnapshot(), operationPhase: "discard", turn: 3n, canDiscard: true });
  let push: ((event: DhmjEvent) => void) | undefined;
  const client = {
    closed: new AbortController().signal,
    set onDhmjPush(handler: typeof push) { push = handler; },
    async request(id: MsgId, payload: Uint8Array) {
      sent.push(id);
      if (id === MsgId.C2S_DHMJ_ENTER) return { snapshot };
      if (id === MsgId.C2S_DHMJ_DISCARD) {
        const discard = fromBinary(DhmjDiscardRequestSchema, payload);
        assert.equal(discard.turn, 3n);
        assert.equal(discard.tile, 1n);
        throw new Error("timeout");
      }
      if (id === MsgId.C2S_DHMJ_SNAPSHOT) {
        setImmediate(() => {
          push?.({ kind: "settlement", value: create(DhmjSettlementPushSchema, {
            tableName: "table", settlement: { userId: 7n, status: "committed", balance: 50n },
          }) });
          push?.({ kind: "round_end", value: create(DhmjRoundEndPushSchema, { tableName: "table", revision: 6n, roundEnd: { kind: "draw" } }) });
        });
        return { snapshot };
      }
      return {};
    },
  };
  const printed: string[] = [];
  await runDhmj(client as unknown as DemoClient, event => printed.push(event));
  assert.deepEqual(sent, [MsgId.C2S_DHMJ_ENTER, MsgId.C2S_DHMJ_DISCARD, MsgId.C2S_DHMJ_SNAPSHOT, MsgId.C2S_DHMJ_LEAVE]);
  assert.deepEqual(printed.slice(-2), ["dhmj_settlement", "dhmj_leave"]);
});
