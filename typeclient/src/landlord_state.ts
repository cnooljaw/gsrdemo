import { create } from "@bufbuild/protobuf";
import {
  LandlordPlayerSchema, type LandlordSnapshot, type LandlordSeatPush, type LandlordReadyPush,
  type LandlordDealPush, type LandlordBidPush, type LandlordPlayPush, type LandlordSettlementPush, type LandlordTrusteePush, type LandlordTurnPush,
} from "./gen/demo/v1/game_pb.js";

export type LandlordEvent =
  | { kind: "seat"; value: LandlordSeatPush }
  | { kind: "ready"; value: LandlordReadyPush }
  | { kind: "deal"; value: LandlordDealPush }
  | { kind: "bid"; value: LandlordBidPush }
  | { kind: "play"; value: LandlordPlayPush }
  | { kind: "settlement"; value: LandlordSettlementPush }
  | { kind: "trustee"; value: LandlordTrusteePush }
  | { kind: "turn"; value: LandlordTurnPush };

// 只应用已按牌桌和序号排序的事件；手牌只存在于本人场景中。
export function applyLandlordEvent(view: LandlordSnapshot, event: LandlordEvent): void {
  const value = event.value;
  switch (event.kind) {
    case "seat": {
      const old = view.players.find(player => player.seat === event.value.seat);
      if (!event.value.joined) {
        view.players = view.players.filter(player => player.seat !== event.value.seat);
      } else if (old && old.userId === event.value.userId) {
        old.ready = event.value.ready;
      } else {
        view.players = view.players.filter(player => player.seat !== event.value.seat);
        view.players.push(create(LandlordPlayerSchema, { userId: event.value.userId, seat: event.value.seat, ready: event.value.ready }));
      }
      break;
    }
    case "ready": {
      const player = view.players.find(player => player.userId === event.value.userId);
      if (player) player.ready = event.value.ready;
      break;
    }
    case "deal":
      view.hand = [...event.value.hand];
      view.bottom = [];
      view.lastCards = [];
      view.lastSeat = -1n;
      view.landlordSeat = -1n;
      view.candidateSeat = -1n;
      view.winnerSeat = -1n;
      view.multiplier = 1n;
      view.settlement = undefined;
      view.redeals = event.value.redeals;
      view.phase = "calling";
      view.turn = event.value.turn;
      view.turnSeat = event.value.turnSeat;
      view.deadlineMs = event.value.deadlineMs;
      view.serverMs = event.value.serverMs;
      view.startMs = event.value.startMs;
      view.canAct = false;
      for (const player of view.players) {
        player.remaining = 17n;
        player.callChoice = 0n;
        player.robChoice = 0n;
        player.trustee = false;
      }
      break;
    case "bid": {
      const player = view.players.find(player => player.userId === event.value.userId);
      if (player) {
        if (event.value.rob) player.robChoice = event.value.yes ? 1n : -1n;
        else player.callChoice = event.value.yes ? 1n : -1n;
      }
      if (view.landlordSeat < 0n && event.value.landlordSeat >= 0n && event.value.landlordSeat === view.seat) {
        view.hand.push(...event.value.bottom);
      }
      if (event.value.landlordSeat >= 0n && view.landlordSeat < 0n) {
        const landlord = view.players.find(item => item.seat === event.value.landlordSeat);
        if (landlord) landlord.remaining += BigInt(event.value.bottom.length);
      }
      view.phase = event.value.redealt ? "dealing" : event.value.phase;
      view.turn = event.value.turn;
      view.turnSeat = event.value.turnSeat;
      view.deadlineMs = event.value.deadlineMs;
      view.serverMs = event.value.serverMs;
      view.startMs = event.value.startMs;
      view.canAct = event.value.canAct;
      view.candidateSeat = event.value.candidateSeat;
      view.landlordSeat = event.value.landlordSeat;
      view.multiplier = event.value.multiplier;
      view.bottom = [...event.value.bottom];
      if (view.phase === "waiting") {
        view.hand = [];
        view.redeals = 0n;
        for (const item of view.players) { item.ready = false; item.remaining = 0n; }
      }
      break;
    }
    case "play": {
      const player = view.players.find(item => item.userId === event.value.userId);
      if (player) {
        player.remaining = event.value.remaining;
        if (player.seat === view.seat) {
          for (const card of event.value.cards) {
            const index = view.hand.indexOf(card);
            if (index < 0) throw new Error("landlord event hand mismatch");
            view.hand.splice(index, 1);
          }
        }
      }
      view.phase = event.value.phase;
      view.turn = event.value.turn;
      view.turnSeat = event.value.turnSeat;
      view.deadlineMs = event.value.deadlineMs;
      view.serverMs = event.value.serverMs;
      view.startMs = event.value.startMs;
      view.canAct = event.value.canAct;
      view.lastCards = [...event.value.lastCards];
      view.lastSeat = event.value.lastSeat;
      view.winnerSeat = event.value.winnerSeat;
      view.multiplier = event.value.multiplier;
      break;
    }
    case "settlement":
      view.settlement = event.value.settlement;
      break;
    case "trustee": {
      const player = view.players.find(item => item.userId === event.value.userId);
      if (player) player.trustee = event.value.trustee;
      view.deadlineMs = event.value.deadlineMs;
      view.serverMs = event.value.serverMs;
      view.startMs = event.value.startMs;
      break;
    }
    case "turn": {
      view.phase = event.value.phase;
      view.turn = event.value.turn;
      view.turnSeat = event.value.turnSeat;
      view.deadlineMs = event.value.deadlineMs;
      view.serverMs = event.value.serverMs;
      view.startMs = 0n;
      view.canAct = true;
      break;
    }
  }
  view.revision = value.revision;
}
