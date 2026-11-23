import { create, fromBinary } from "@bufbuild/protobuf";
import type { Envelope } from "./gen/gsr/common/v1/common_pb.js";
import {
  DhmjDealPushSchema, DhmjDiscardPushSchema, DhmjDiscardsSchema, DhmjDrawPushSchema, DhmjFlowerPushSchema,
  DhmjMeldPushSchema, DhmjPlayerSchema, DhmjRoundEndPushSchema, DhmjSeatPushSchema, DhmjSeatTilesSchema, DhmjSettlementPushSchema,
  DhmjTrusteePushSchema, DhmjTurnPushSchema, MsgId,
  type DhmjDealPush, type DhmjDiscardPush, type DhmjDrawPush, type DhmjFlowerPush, type DhmjMeldPush,
  type DhmjRoundEndPush, type DhmjSeatPush, type DhmjSettlementPush, type DhmjSnapshot, type DhmjTrusteePush, type DhmjTurnPush,
} from "./gen/demo/v1/game_pb.js";

type SequencedEvent =
  | { kind: "seat"; value: DhmjSeatPush }
  | { kind: "trustee"; value: DhmjTrusteePush }
  | { kind: "deal"; value: DhmjDealPush }
  | { kind: "discard"; value: DhmjDiscardPush }
  | { kind: "draw"; value: DhmjDrawPush }
  | { kind: "flower"; value: DhmjFlowerPush }
  | { kind: "meld"; value: DhmjMeldPush }
  | { kind: "turn"; value: DhmjTurnPush }
  | { kind: "round_end"; value: DhmjRoundEndPush };
export type DhmjEvent = SequencedEvent | { kind: "settlement"; value: DhmjSettlementPush };

export function decodeDhmjEvent(frame: Envelope): DhmjEvent | undefined {
  switch (frame.msgId) {
    case MsgId.S2C_DHMJ_SEAT_PUSH: return { kind: "seat", value: fromBinary(DhmjSeatPushSchema, frame.payload) };
    case MsgId.S2C_DHMJ_TRUSTEE_PUSH: return { kind: "trustee", value: fromBinary(DhmjTrusteePushSchema, frame.payload) };
    case MsgId.S2C_DHMJ_DEAL_PUSH: return { kind: "deal", value: fromBinary(DhmjDealPushSchema, frame.payload) };
    case MsgId.S2C_DHMJ_DISCARD_PUSH: return { kind: "discard", value: fromBinary(DhmjDiscardPushSchema, frame.payload) };
    case MsgId.S2C_DHMJ_DRAW_PUSH: return { kind: "draw", value: fromBinary(DhmjDrawPushSchema, frame.payload) };
    case MsgId.S2C_DHMJ_FLOWER_PUSH: return { kind: "flower", value: fromBinary(DhmjFlowerPushSchema, frame.payload) };
    case MsgId.S2C_DHMJ_MELD_PUSH: return { kind: "meld", value: fromBinary(DhmjMeldPushSchema, frame.payload) };
    case MsgId.S2C_DHMJ_TURN_PUSH: return { kind: "turn", value: fromBinary(DhmjTurnPushSchema, frame.payload) };
    case MsgId.S2C_DHMJ_ROUND_END_PUSH: return { kind: "round_end", value: fromBinary(DhmjRoundEndPushSchema, frame.payload) };
    case MsgId.S2C_DHMJ_SETTLEMENT_PUSH: return { kind: "settlement", value: fromBinary(DhmjSettlementPushSchema, frame.payload) };
  }
}

function removeTile(hand: bigint[], tile: bigint): void {
  const index = hand.indexOf(tile);
  if (index < 0) throw new Error("麻将手牌与事件不一致");
  hand.splice(index, 1);
}

function adjustHandCount(view: DhmjSnapshot, seat: bigint, delta: bigint): void {
  const index = Number(seat);
  const previous = view.handCounts[index];
  if (previous === undefined || previous + delta < 0n || previous + delta > 14n) {
    throw new Error("麻将手牌张数与事件不一致");
  }
  view.handCounts[index] = previous + delta;
}

function clearActionOptions(view: DhmjSnapshot): void {
  view.canDiscard = false;
  view.canSelfWin = false;
  view.selfAction = false;
  view.offers = [];
  view.concealedKongs = [];
  view.addedKongs = [];
}

// 演示客户端按序更新等待区座位、暗手、公开牌面和操作权。
function applyDhmjEvent(view: DhmjSnapshot, event: SequencedEvent): void {
  if (event.kind === "seat") {
    const { revision, seat, userId, joined, ready } = event.value;
    if (view.phase !== "waiting") throw new Error("麻将座位事件与当前阶段不一致");
    const players = view.players.filter(player => player.seat !== seat);
    if (joined) players.push(create(DhmjPlayerSchema, { seat, userId, ready }));
    view.players = players.sort((a, b) => Number(a.seat - b.seat));
    view.revision = revision;
    return;
  }
  if (event.kind === "trustee") {
    const { revision, seat, enabled } = event.value;
    view.trustees[Number(seat)] = enabled;
    if (enabled && seat === view.seat) clearActionOptions(view);
    view.revision = revision;
    return;
  }
  const ownHand = [...view.hand];
  if (event.kind !== "turn") clearActionOptions(view);
  switch (event.kind) {
    case "deal":
      if (event.value.seat !== view.seat) throw new Error("起手牌座位不匹配");
      view.hand = [...event.value.hand];
      view.players = event.value.playerIds.map((userId, seat) => create(DhmjPlayerSchema, { userId, seat: BigInt(seat), ready: true }));
      view.discards = view.players.map(() => create(DhmjDiscardsSchema));
      view.handCounts = view.players.map(() => BigInt(event.value.hand.length));
      view.seatTiles = view.players.map(() => create(DhmjSeatTilesSchema));
      view.trustees = view.players.map(() => false);
      view.dealerSeat = event.value.dealerSeat;
      view.remaining = event.value.remaining;
      view.phase = "playing";
      break;
    case "discard":
      if (event.value.seat === view.seat) removeTile(ownHand, event.value.tile);
      adjustHandCount(view, event.value.seat, -1n);
      view.discards[Number(event.value.seat)]!.tiles.push(event.value.tile);
      view.hand = ownHand;
      break;
    case "draw":
      if (event.value.seat === view.seat) {
        if (event.value.tile === undefined) throw new Error("缺少本人摸牌");
        ownHand.push(event.value.tile);
      }
      adjustHandCount(view, event.value.seat, 1n);
      view.hand = ownHand;
      view.remaining = event.value.remaining;
      break;
    case "flower": {
      const { seat, flowers, replacements, fromDraw, remaining, handCount } = event.value;
      const seatTiles = view.seatTiles[Number(seat)];
      if (!seatTiles) throw new Error("补花座位与场景不一致");
      if (seat === view.seat && !fromDraw) {
        if (flowers.length !== replacements.length) throw new Error("补花牌张不完整");
        for (const [index, flower] of flowers.entries()) {
          removeTile(ownHand, flower);
          if (replacements[index] !== 0n) ownHand.push(replacements[index]!);
        }
      }
      if (seat === view.seat && handCount !== BigInt(ownHand.length)) throw new Error("补花后的手牌张数不一致");
      view.handCounts[Number(seat)] = handCount;
      view.hand = ownHand;
      seatTiles.flowers.push(...flowers);
      view.remaining = remaining;
      break;
    }
    case "meld": {
      const { seat, meld, meldIndex } = event.value;
      const seatTiles = view.seatTiles[Number(seat)];
      if (!meld || !seatTiles) throw new Error("副露座位与场景不一致");
      const index = Number(meldIndex);
      if (meld.kind === "added_kong") {
        const previous = seatTiles.melds[index];
        if (previous?.kind !== "pong" || previous.sourceSeat !== meld.sourceSeat || previous.tiles[0] !== meld.tiles[0]) {
          throw new Error("补杠找不到对应的原碰");
        }
        if (seat === view.seat) removeTile(ownHand, meld.tiles[3]!);
        adjustHandCount(view, seat, -1n);
      } else {
        if (meldIndex !== BigInt(seatTiles.melds.length)) throw new Error("副露位置与场景不一致");
        if (meld.kind !== "concealed_kong") {
          const discard = view.discards[Number(meld.sourceSeat)]!.tiles;
          const taken = discard.at(-1);
          if (taken === undefined) throw new Error("找不到被取走的弃牌");
          const tiles = [...meld.tiles];
          removeTile(tiles, taken);
          if (seat === view.seat) for (const tile of tiles) removeTile(ownHand, tile);
          adjustHandCount(view, seat, -BigInt(tiles.length));
          discard.pop();
        } else {
          if (seat === view.seat) for (const tile of meld.tiles) removeTile(ownHand, tile);
          adjustHandCount(view, seat, -BigInt(meld.tiles.length));
        }
      }
      seatTiles.melds[index] = meld;
      view.hand = ownHand;
      break;
    }
    case "turn":
      if (event.value.seat !== view.seat) throw new Error("操作选项座位不匹配");
      view.operationPhase = event.value.phase;
      view.phase = event.value.phase === "finished" ? "finished" : "playing";
      view.activeSeat = event.value.activeSeat;
      view.turn = event.value.turn;
      view.windowId = event.value.windowId;
      view.windowTile = event.value.windowTile;
      view.robKong = event.value.robKong;
      view.serverMs = event.value.serverMs;
      view.deadlineMs = event.value.deadlineMs;
      view.offers = event.value.offers;
      view.canDiscard = event.value.canDiscard;
      view.canSelfWin = event.value.canSelfWin;
      view.concealedKongs = event.value.concealedKongs;
      view.addedKongs = event.value.addedKongs;
      view.selfAction = view.operationPhase === "self" && (view.canSelfWin ||
        view.concealedKongs.length > 0 || view.addedKongs.length > 0);
      break;
    case "round_end": {
      const end = event.value.roundEnd;
      if (end?.robKong && end.sourceSeat !== undefined && end.tile !== undefined) {
        if (end.sourceSeat === view.seat) removeTile(ownHand, end.tile);
        adjustHandCount(view, end.sourceSeat, -1n);
        view.hand = ownHand;
      }
      view.roundEnd = end;
      view.phase = "finished";
      view.operationPhase = "finished";
      view.windowId = 0n;
      view.windowTile = 0n;
      view.robKong = false;
      view.deadlineMs = 0n;
      break;
    }
  }
  view.revision = event.value.revision;
}

export class DhmjScene {
  view?: DhmjSnapshot;
  private readonly buffer = new Map<bigint, SequencedEvent>();
  private discardedThrough = 0n;
  private settlement?: DhmjSettlementPush;
  private wake?: () => void;
  private sampledAt = 0;

  constructor(private readonly print: (event: string, value: unknown) => void) {}

  get needsSync(): boolean {
    return !!this.view && (this.view.revision < this.discardedThrough || this.buffer.size > 0);
  }

  restore(view: DhmjSnapshot): void {
    if (this.view && this.view.tableName !== view.tableName) throw new Error("恢复到了另一张麻将桌");
    if (this.view && view.revision < this.view.revision) return;
    this.view = view;
    this.sampledAt = performance.now();
    for (const [revision, event] of this.buffer) {
      if (revision <= view.revision || event.value.tableName !== view.tableName) this.buffer.delete(revision);
    }
    this.drain();
  }

  accept(event: DhmjEvent): void {
    const view = this.view;
    if (view && event.value.tableName !== view.tableName) return;
    if (event.kind === "settlement") {
      this.settlement = event.value;
    } else if (event.value.revision > (view?.revision ?? 0n)) {
      this.buffer.set(event.value.revision, event);
      if (this.buffer.size > 128) {
        for (const revision of this.buffer.keys()) {
          if (revision > this.discardedThrough) this.discardedThrough = revision;
        }
        this.buffer.clear();
      }
    }
    this.drain();
    this.wake?.();
  }

  private drain(): void {
    const view = this.view;
    if (!view) return;
    while (view.revision >= this.discardedThrough) {
      const next = view.revision + 1n;
      const event = this.buffer.get(next);
      if (!event) break;
      this.buffer.delete(next);
      try {
        applyDhmjEvent(view, event);
      } catch {
        this.discardedThrough = next;
        break;
      }
      if (event.kind === "turn") this.sampledAt = performance.now();
      this.print(`dhmj_event_${event.kind}`, event.value);
    }
    const result = this.settlement?.settlement;
    if (view.phase === "finished" && this.settlement?.tableName === view.tableName &&
        result?.status === "committed" && result.userId === view.players.find(p => p.seat === view.seat)?.userId) {
      view.settlement = result;
    }
  }

  async wait(closed: AbortSignal): Promise<boolean> {
    closed.throwIfAborted();
    const view = this.view!;
    const remaining = Number(view.deadlineMs - view.serverMs) - (performance.now() - this.sampledAt);
    const timeout = view.phase === "playing" && view.deadlineMs > 0n
      ? Math.max(1_000, Math.min(25_000, remaining + 1_000)) : 25_000;
    return await new Promise<boolean>((resolve, reject) => {
      const finish = () => {
        this.wake = undefined;
        clearTimeout(timer);
        closed.removeEventListener("abort", onClose);
      };
      const onClose = () => { finish(); reject(closed.reason); };
      const timer = setTimeout(() => { finish(); resolve(false); }, timeout);
      this.wake = () => { finish(); resolve(true); };
      closed.addEventListener("abort", onClose, { once: true });
    });
  }
}
