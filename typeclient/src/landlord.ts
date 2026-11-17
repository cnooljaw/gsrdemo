import { create, toBinary } from "@bufbuild/protobuf";
import { DemoClient } from "./client.js";
import {
  LandlordEnterRequestSchema, LandlordReadyRequestSchema, LandlordPlayRequestSchema,
  LandlordSnapshotRequestSchema, LandlordLeaveRequestSchema, LandlordEnterResponseSchema, LandlordReadyResponseSchema, LandlordPlayResponseSchema,
  LandlordSnapshotResponseSchema, LandlordLeaveResponseSchema, LandlordCallRequestSchema, LandlordCallResponseSchema, LandlordRobRequestSchema, LandlordRobResponseSchema,
  MsgId,
} from "./gen/demo/v1/game_pb.js";

type Printer = (event: string, value: unknown) => void;
function rank(card: bigint): number {
  const value = Number(card & 15n);
  return value >= 14 ? value + 2 : value < 3 ? value + 13 : value;
}

// 演示机器人由牌桌事件驱动；只在事件缺口或等待超时时恢复场景。
export async function runLandlord(client: DemoClient, print: Printer, timeout: number): Promise<void> {
  const entry = await client.request(MsgId.C2S_LANDLORD_ENTER,
    toBinary(LandlordEnterRequestSchema, create(LandlordEnterRequestSchema)), MsgId.S2C_LANDLORD_ENTER, LandlordEnterResponseSchema);
  if (!entry.snapshot) throw new Error("missing landlord snapshot");
  client.setLandlord(entry.snapshot);
  print("landlord_enter", entry.snapshot);
  await client.request(MsgId.C2S_LANDLORD_READY,
    toBinary(LandlordReadyRequestSchema, create(LandlordReadyRequestSchema)), MsgId.S2C_LANDLORD_READY, LandlordReadyResponseSchema);
  const deadline = Date.now() + Math.max(90_000, timeout);
  let lastAction = "";
  let settled = false;
  while (Date.now() < deadline) {
    if (client.landlordNeedsSync) {
      const response = await client.request(MsgId.C2S_LANDLORD_SNAPSHOT,
        toBinary(LandlordSnapshotRequestSchema, create(LandlordSnapshotRequestSchema)), MsgId.S2C_LANDLORD_SNAPSHOT, LandlordSnapshotResponseSchema);
      if (!response.snapshot) throw new Error("missing landlord snapshot");
      client.setLandlord(response.snapshot);
      continue;
    }
    const snapshot = client.landlord;
    if (!snapshot) throw new Error("missing landlord scene");
    if (snapshot.phase === "finished" && snapshot.settlement?.status === "committed") {
      print("landlord_settlement", snapshot.settlement);
      settled = true;
      break;
    }
    const action = `${snapshot.phase}:${snapshot.turn}`;
    if ((snapshot.phase === "calling" || snapshot.phase === "robbing") && snapshot.seat === snapshot.turnSeat && lastAction !== action) {
      lastAction = action;
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
      continue;
    }
    if (snapshot.phase === "playing" && snapshot.seat === snapshot.turnSeat && lastAction !== action) {
      lastAction = action;
      const selected = snapshot.lastCards.length === 0 ? snapshot.hand[0] :
        snapshot.lastCards.length === 1 ? snapshot.hand.find(card => rank(card) > rank(snapshot.lastCards[0]!)) : undefined;
      const cards = selected === undefined ? [] : [selected];
      print("landlord_action", { turn: snapshot.turn, cards });
      await client.request(MsgId.C2S_LANDLORD_PLAY,
        toBinary(LandlordPlayRequestSchema, create(LandlordPlayRequestSchema, { turn: snapshot.turn, cards })),
        MsgId.S2C_LANDLORD_PLAY, LandlordPlayResponseSchema);
      continue;
    }
    // 定时器只在当前阶段预计应有下一次变化后查询，不持续轮询牌桌。
    const wait = snapshot.phase !== "finished" && snapshot.deadlineMs > 0n ? Math.max(1000, Number(snapshot.deadlineMs) - Date.now() + 1000) : 15_000;
    if (!await client.waitLandlord(snapshot.revision, Math.min(wait, 15_000))) {
      const response = await client.request(MsgId.C2S_LANDLORD_SNAPSHOT,
        toBinary(LandlordSnapshotRequestSchema, create(LandlordSnapshotRequestSchema)), MsgId.S2C_LANDLORD_SNAPSHOT, LandlordSnapshotResponseSchema);
      if (!response.snapshot) throw new Error("missing landlord snapshot");
      client.setLandlord(response.snapshot);
    }
  }
  if (!settled) throw new Error("landlord demo did not finish; keep original operation ID and query snapshot");
  await client.request(MsgId.C2S_LANDLORD_LEAVE,
    toBinary(LandlordLeaveRequestSchema, create(LandlordLeaveRequestSchema)), MsgId.S2C_LANDLORD_LEAVE, LandlordLeaveResponseSchema);
  client.leaveLandlord();
  print("landlord_leave", {});
}
