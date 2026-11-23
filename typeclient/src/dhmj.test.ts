import { create, fromBinary } from "@bufbuild/protobuf";
import assert from "node:assert/strict";
import { test } from "node:test";
import type { DemoClient } from "./client.js";
import { runDhmj } from "./dhmj.js";
import { DhmjScene, type DhmjEvent } from "./dhmj_state.js";
import {
  DhmjDealPushSchema, DhmjDiscardPushSchema, DhmjDiscardRequestSchema, DhmjDrawPushSchema, DhmjFlowerPushSchema,
  DhmjMeldPushSchema, DhmjMeldSchema, DhmjRoundEndPushSchema, DhmjSeatPushSchema, DhmjSettlementPushSchema, DhmjSnapshotSchema, DhmjTrusteePushSchema,
  DhmjTurnPushSchema, MsgId,
} from "./gen/demo/v1/game_pb.js";

function sceneSnapshot() {
  return create(DhmjSnapshotSchema, {
    tableName: "table", phase: "playing", seat: 0n, revision: 5n, hand: [1n, 2n],
    players: [{ userId: 7n, seat: 0n }], discards: [{}, {}, {}, {}], seatTiles: [{}, {}, {}, {}],
    handCounts: [2n, 13n, 13n, 13n],
  });
}

test("麻将等待区按序应用进桌、准备和离桌，发牌沿用同一序号", () => {
  const scene = new DhmjScene(() => {});
  const pushSeat = (revision: bigint, joined: boolean, ready: boolean) => scene.accept({
    kind: "seat", value: create(DhmjSeatPushSchema, { tableName: "table", revision, seat: 1n, userId: 8n, joined, ready }),
  });
  pushSeat(2n, true, false);
  scene.restore(create(DhmjSnapshotSchema, {
    tableName: "table", phase: "waiting", seat: 0n, revision: 1n, players: [{ userId: 7n, seat: 0n }],
  }));
  assert.deepEqual(scene.view?.players.map(p => p.userId), [7n, 8n]);
  pushSeat(3n, true, true);
  assert.equal(scene.view?.players[1]?.ready, true);
  pushSeat(4n, false, false);
  pushSeat(3n, true, true);
  assert.deepEqual(scene.view?.players.map(p => p.userId), [7n]);
  scene.accept({ kind: "deal", value: create(DhmjDealPushSchema, {
    tableName: "table", revision: 5n, seat: 0n, playerIds: [7n, 9n, 10n, 11n], hand: [1n, 2n],
  }) });
  assert.equal(scene.view?.phase, "playing");
  assert.equal(scene.view?.revision, 5n);
  assert.deepEqual(scene.view?.handCounts, [2n, 2n, 2n, 2n]);
  assert.deepEqual(scene.view?.seatTiles.map(seat => [seat.flowers, seat.melds]), Array.from({ length: 4 }, () => [[], []]));
  assert.equal(scene.needsSync, false);
});

test("麻将托管只更新变化的座位，本人开启时清除待响应选项", () => {
  const scene = new DhmjScene(() => {});
  scene.restore(create(DhmjSnapshotSchema, {
    ...sceneSnapshot(), operationPhase: "respond", offers: [6n], trustees: [false, false, false, false],
  }));
  const pushTrustee = (revision: bigint, seat: bigint, enabled: boolean) => scene.accept({
    kind: "trustee", value: create(DhmjTrusteePushSchema, { tableName: "table", revision, seat, enabled }),
  });
  pushTrustee(6n, 1n, true);
  assert.deepEqual(scene.view?.trustees, [false, true, false, false]);
  assert.deepEqual(scene.view?.offers, [6n]);
  pushTrustee(7n, 0n, true);
  pushTrustee(8n, 0n, false);
  assert.deepEqual(scene.view?.trustees, [false, true, false, false]);
  assert.deepEqual(scene.view?.offers, []);
  assert.equal(scene.view?.revision, 8n);
});

test("麻将缺号时恢复快照并接续较新的摸牌和轮次", () => {
  const scene = new DhmjScene(() => {});
  scene.restore(sceneSnapshot());
  scene.accept({ kind: "draw", value: create(DhmjDrawPushSchema, { tableName: "table", revision: 7n, seat: 0n, tile: 3n, remaining: 40n }) });
  scene.accept({ kind: "turn", value: create(DhmjTurnPushSchema, { tableName: "table", revision: 8n, seat: 0n, phase: "discard", canDiscard: true, turn: 4n }) });
  assert.equal(scene.needsSync, true);
  assert.deepEqual(scene.view?.hand, [1n, 2n]);
  scene.restore(create(DhmjSnapshotSchema, { ...sceneSnapshot(), revision: 6n, hand: [2n], handCounts: [1n, 13n, 13n, 13n], dealerSeat: 0n, remaining: 41n }));
  assert.equal(scene.needsSync, false);
  assert.equal(scene.view?.revision, 8n);
  assert.equal(scene.view?.canDiscard, true);
  assert.equal(scene.view?.dealerSeat, 0n);
  assert.equal(scene.view?.remaining, 40n);
  assert.deepEqual(scene.view?.hand, [2n, 3n]);
  assert.deepEqual(scene.view?.handCounts, [2n, 13n, 13n, 13n]);
});

test("麻将轮次事件更新抢杠窗口，并替换重连快照中的自选候选", () => {
  const scene = new DhmjScene(() => {});
  scene.restore(create(DhmjSnapshotSchema, {
    ...sceneSnapshot(), operationPhase: "self", selfAction: true, concealedKongs: [3n], addedKongs: [5n],
  }));
  scene.accept({ kind: "turn", value: create(DhmjTurnPushSchema, {
    tableName: "table", revision: 6n, seat: 0n, phase: "respond", windowId: 4n, windowTile: 5n, robKong: true, offers: [6n],
  }) });
  assert.equal(scene.view?.windowId, 4n);
  assert.equal(scene.view?.windowTile, 5n);
  assert.equal(scene.view?.robKong, true);
  assert.deepEqual(scene.view?.offers, [6n]);
  assert.deepEqual(scene.view?.concealedKongs, []);
  assert.deepEqual(scene.view?.addedKongs, []);
  scene.accept({ kind: "turn", value: create(DhmjTurnPushSchema, {
    tableName: "table", revision: 7n, seat: 0n, phase: "self", concealedKongs: [8n], addedKongs: [9n],
  }) });
  assert.equal(scene.view?.windowId, 0n);
  assert.equal(scene.view?.windowTile, 0n);
  assert.equal(scene.view?.robKong, false);
  assert.deepEqual(scene.view?.offers, []);
  assert.deepEqual(scene.view?.concealedKongs, [8n]);
  assert.deepEqual(scene.view?.addedKongs, [9n]);
  assert.equal(scene.view?.selfAction, true);
  scene.accept({ kind: "trustee", value: create(DhmjTrusteePushSchema, {
    tableName: "table", revision: 8n, seat: 0n, enabled: true,
  }) });
  assert.equal(scene.view?.selfAction, false);
  assert.deepEqual(scene.view?.concealedKongs, []);
  assert.deepEqual(scene.view?.addedKongs, []);
});

test("麻将补花更新各座位的公开花牌，替换牌只更新本人暗手", () => {
  const scene = new DhmjScene(() => {});
  scene.restore(create(DhmjSnapshotSchema, { ...sceneSnapshot(), hand: [1n, 0x41n] }));
  scene.accept({ kind: "flower", value: create(DhmjFlowerPushSchema, {
    tableName: "table", revision: 6n, seat: 0n, flowers: [0x41n, 0x42n], replacements: [0x42n, 5n], remaining: 26n, handCount: 2n,
  }) });
  scene.accept({ kind: "discard", value: create(DhmjDiscardPushSchema, { tableName: "table", revision: 7n, seat: 1n, tile: 1n }) });
  scene.accept({ kind: "flower", value: create(DhmjFlowerPushSchema, {
    tableName: "table", revision: 8n, seat: 2n, flowers: [0x43n], fromDraw: true, remaining: 24n, handCount: 13n,
  }) });
  scene.accept({ kind: "draw", value: create(DhmjDrawPushSchema, {
    tableName: "table", revision: 9n, seat: 2n, remaining: 24n,
  }) });
  assert.deepEqual(scene.view?.hand, [1n, 5n]);
  assert.deepEqual(scene.view?.seatTiles.map(seat => seat.flowers), [[0x41n, 0x42n], [], [0x43n], []]);
  assert.deepEqual(scene.view?.discards[1]?.tiles, [1n]);
  assert.equal(scene.view?.remaining, 24n);
  assert.deepEqual(scene.view?.handCounts, [2n, 12n, 14n, 13n]);
});

test("麻将起手花未补回时，其他座位也能更新公开张数", () => {
  const scene = new DhmjScene(() => {});
  scene.restore(sceneSnapshot());
  scene.accept({ kind: "flower", value: create(DhmjFlowerPushSchema, {
    tableName: "table", revision: 6n, seat: 2n, flowers: [0x41n], handCount: 12n,
  }) });
  assert.deepEqual(scene.view?.handCounts, [2n, 13n, 12n, 13n]);
  assert.deepEqual(scene.view?.hand, [1n, 2n]);
});

test("麻将补杠替换原碰位置，非本人副露保留本人的暗手", () => {
  const scene = new DhmjScene(() => {});
  const pong = create(DhmjMeldSchema, { kind: "pong", sourceSeat: 2n, tiles: [5n, 5n, 5n] });
  const eat = create(DhmjMeldSchema, { kind: "eat", sourceSeat: 0n, tiles: [1n, 2n, 3n] });
  const kong = create(DhmjMeldSchema, { ...pong, kind: "added_kong", tiles: [5n, 5n, 5n, 5n] });
  const snapshot = sceneSnapshot();
  snapshot.discards[0]!.tiles = [3n];
  snapshot.discards[2]!.tiles = [5n];
  scene.restore(snapshot);
  for (const [revision, meldIndex, meld] of [[6n, 0n, pong], [7n, 1n, eat], [8n, 0n, kong]] as const) {
    scene.accept({ kind: "meld", value: create(DhmjMeldPushSchema, {
      tableName: "table", revision, seat: 1n, meldIndex, meld,
    }) });
  }
  assert.deepEqual(scene.view?.seatTiles[1]?.melds, [kong, eat]);
  assert.deepEqual(scene.view?.discards.map(seat => seat.tiles), [[], [], [], []]);
  assert.deepEqual(scene.view?.hand, [1n, 2n]);
  assert.deepEqual(scene.view?.handCounts, [2n, 8n, 13n, 13n]);
  assert.equal(scene.needsSync, false);
});

test("麻将抢杠胡保留原碰并移除本人用于补杠的牌", () => {
  const scene = new DhmjScene(() => {});
  const pong = create(DhmjMeldSchema, { kind: "pong", sourceSeat: 2n, tiles: [5n, 5n, 5n] });
  const snapshot = sceneSnapshot();
  snapshot.hand = [1n, 5n];
  snapshot.seatTiles[0]!.melds = [pong];
  snapshot.operationPhase = "respond";
  snapshot.windowId = 4n;
  snapshot.windowTile = 5n;
  snapshot.robKong = true;
  scene.restore(snapshot);
  scene.accept({ kind: "round_end", value: create(DhmjRoundEndPushSchema, {
    tableName: "table", revision: 6n, roundEnd: {
      kind: "discard_hu", sourceSeat: 0n, tile: 5n, robKong: true, winners: [{ seat: 1n, point: 1n }],
    },
  }) });
  assert.deepEqual(scene.view?.seatTiles[0]?.melds, [pong]);
  assert.deepEqual(scene.view?.hand, [1n]);
  assert.deepEqual(scene.view?.handCounts, [1n, 13n, 13n, 13n]);
  assert.equal(scene.view?.phase, "finished");
  assert.equal(scene.view?.operationPhase, "finished");
  assert.equal(scene.view?.windowId, 0n);
  assert.equal(scene.view?.windowTile, 0n);
  assert.equal(scene.view?.robKong, false);
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
