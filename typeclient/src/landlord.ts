import { create, toBinary } from "@bufbuild/protobuf";
import { setTimeout as delay } from "node:timers/promises";
import { DemoClient } from "./client.js";
import {
  LandlordEnterRequestSchema, LandlordReadyRequestSchema, LandlordPlayRequestSchema,
  LandlordSnapshotRequestSchema, LandlordLeaveRequestSchema, LandlordEnterResponseSchema, LandlordReadyResponseSchema, LandlordPlayResponseSchema,
  LandlordSnapshotResponseSchema, LandlordLeaveResponseSchema, LandlordCallRequestSchema, LandlordCallResponseSchema, LandlordRobRequestSchema, LandlordRobResponseSchema,
  LandlordOperationRequestSchema, LandlordOperationResponseSchema, MsgId,
} from "./gen/demo/v1/game_pb.js";

type Printer = (event: string, value: unknown) => void;
function rank(card: bigint): number {
  const value = Number(card & 15n);
  return value >= 14 ? value + 2 : value < 3 ? value + 13 : value;
}

// 演示机器人只用单张和不出跑完一局；服务端独立校验全部牌型，不信任此选择。
export async function runLandlord(client: DemoClient, print: Printer, timeout: number): Promise<void> {
  const entry = await client.request(MsgId.C2S_LANDLORD_ENTER,
    toBinary(LandlordEnterRequestSchema, create(LandlordEnterRequestSchema)), MsgId.S2C_LANDLORD_ENTER, LandlordEnterResponseSchema);
  if (!entry.snapshot) throw new Error("missing landlord snapshot");
  client.landlord = entry.snapshot;
  print("landlord_enter", entry.snapshot);
  const ready = await client.request(MsgId.C2S_LANDLORD_READY,
    toBinary(LandlordReadyRequestSchema, create(LandlordReadyRequestSchema)), MsgId.S2C_LANDLORD_READY, LandlordReadyResponseSchema);
  print("landlord_ready", ready.snapshot);
  const deadline = Date.now() + Math.max(90_000, timeout);
  let settled = false;
  while (Date.now() < deadline) {
    const response = await client.request(MsgId.C2S_LANDLORD_SNAPSHOT,
      toBinary(LandlordSnapshotRequestSchema, create(LandlordSnapshotRequestSchema)), MsgId.S2C_LANDLORD_SNAPSHOT, LandlordSnapshotResponseSchema);
    const snapshot = response.snapshot;
    if (!snapshot) throw new Error("missing landlord snapshot");
    client.landlord = snapshot;
    if ((snapshot.phase === "calling" || snapshot.phase === "robbing") && snapshot.seat === snapshot.turnSeat) {
      print("landlord_bid", { phase: snapshot.phase, turn: snapshot.turn, yes: true });
      if (snapshot.phase === "calling") {
        await client.request(MsgId.C2S_LANDLORD_CALL,
          toBinary(LandlordCallRequestSchema, create(LandlordCallRequestSchema, { turn: snapshot.turn, yes: true })),
          MsgId.S2C_LANDLORD_CALL, LandlordCallResponseSchema);
      } else {
        await client.request(MsgId.C2S_LANDLORD_ROB,
          toBinary(LandlordRobRequestSchema, create(LandlordRobRequestSchema, { turn: snapshot.turn, yes: true })),
          MsgId.S2C_LANDLORD_ROB, LandlordRobResponseSchema);
      }
    } else if (snapshot.phase === "playing" && snapshot.seat === snapshot.turnSeat) {
      const selected = snapshot.lastCards.length === 0 ? snapshot.hand[0] :
        snapshot.lastCards.length === 1 ? snapshot.hand.find(card => rank(card) > rank(snapshot.lastCards[0])) : undefined;
      const cards = selected === undefined ? [] : [selected];
      print("landlord_action", { turn: snapshot.turn, cards });
      const played = await client.request(MsgId.C2S_LANDLORD_PLAY,
        toBinary(LandlordPlayRequestSchema, create(LandlordPlayRequestSchema, { turn: snapshot.turn, cards })),
        MsgId.S2C_LANDLORD_PLAY, LandlordPlayResponseSchema);
      print("landlord_play", played.snapshot);
    } else if (snapshot.phase === "finished" && snapshot.settlement?.status !== "pending") {
      const receipt = snapshot.settlement;
      if (!receipt || !receipt.operationId) throw new Error("missing round settlement");
      print("landlord_settlement", receipt);
      if (receipt.status !== "committed") throw new Error(`landlord settlement rejected: ${receipt.reason}`);
      const operation = await client.request(MsgId.C2S_LANDLORD_OPERATION,
        toBinary(LandlordOperationRequestSchema, create(LandlordOperationRequestSchema, { operationId: receipt.operationId })),
        MsgId.S2C_LANDLORD_OPERATION, LandlordOperationResponseSchema);
      if (!operation.found || operation.status !== "committed") throw new Error("round operation not confirmed");
      print("landlord_operation", operation);
      settled = true;
      break;
    }
    await delay(25);
  }
  if (!settled) throw new Error("landlord demo did not finish; keep original operation ID and query snapshot");
  await client.request(MsgId.C2S_LANDLORD_LEAVE,
    toBinary(LandlordLeaveRequestSchema, create(LandlordLeaveRequestSchema)), MsgId.S2C_LANDLORD_LEAVE, LandlordLeaveResponseSchema);
  client.landlord = undefined;
  print("landlord_leave", {});
}
