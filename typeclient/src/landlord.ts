import { create, toBinary } from "@bufbuild/protobuf";
import { DemoClient } from "./client.js";
import {
  LandlordEnterRequestSchema, LandlordReadyRequestSchema, LandlordPlayRequestSchema,
  LandlordSnapshotRequestSchema, LandlordLeaveRequestSchema, LandlordEnterResponseSchema,
  LandlordSnapshotResponseSchema, LandlordCallRequestSchema, LandlordRobRequestSchema,
  ActionAckSchema, MsgId,
} from "./gen/demo/v1/game_pb.js";

type Printer = (event: string, value: unknown) => void;
function rank(card: bigint): number {
  const value = Number(card & 15n);
  return value >= 14 ? value + 2 : value < 3 ? value + 13 : value;
}

// 演示机器人由牌桌事件驱动；只在事件缺口或等待超时时恢复场景。
export async function runLandlord(client: DemoClient, print: Printer): Promise<void> {
  const entry = await client.request(MsgId.C2S_LANDLORD_ENTER,
    toBinary(LandlordEnterRequestSchema, create(LandlordEnterRequestSchema)), MsgId.S2C_LANDLORD_ENTER, LandlordEnterResponseSchema);
  if (!entry.snapshot) throw new Error("missing landlord snapshot");
  client.setLandlord(entry.snapshot);
  print("landlord_enter", entry.snapshot);
  let lastAction = "";
  let lastReadyTurn: bigint | undefined;
  const recover = async () => {
    const response = await client.request(MsgId.C2S_LANDLORD_SNAPSHOT,
      toBinary(LandlordSnapshotRequestSchema, create(LandlordSnapshotRequestSchema)), MsgId.S2C_LANDLORD_SNAPSHOT, LandlordSnapshotResponseSchema);
    if (!response.snapshot) throw new Error("missing landlord snapshot");
    client.setLandlord(response.snapshot);
  };
  for (;;) {
    if (client.landlordNeedsSync) {
      await recover();
      continue;
    }
    const snapshot = client.landlord;
    if (!snapshot) throw new Error("missing landlord scene");
    if (snapshot.phase === "finished" && snapshot.settlement?.status === "committed") {
      print("landlord_settlement", snapshot.settlement);
      break;
    }
    const player = snapshot.players.find(item => item.seat === snapshot.seat);
    if (snapshot.phase === "waiting" && !player?.ready && lastReadyTurn !== snapshot.turn) {
      const turn = snapshot.turn;
      lastReadyTurn = turn;
      try {
        await client.request(MsgId.C2S_LANDLORD_READY,
          toBinary(LandlordReadyRequestSchema, create(LandlordReadyRequestSchema)), MsgId.S2C_ACTION_ACK, ActionAckSchema);
      } catch (error) {
        await recover();
        const current = client.landlord;
        if (current?.phase === "waiting" && current.turn === turn && !current.players.find(item => item.seat === current.seat)?.ready) throw error;
      }
      continue;
    }
    const action = `${snapshot.phase}:${snapshot.turn}`;
    const trustee = player?.trustee ?? false;
    if (snapshot.canAct && !trustee && (snapshot.phase === "calling" || snapshot.phase === "robbing") && snapshot.seat === snapshot.turnSeat && lastAction !== action) {
      lastAction = action;
      print("landlord_bid", { phase: snapshot.phase, turn: snapshot.turn, yes: true });
      try {
        if (snapshot.phase === "calling") {
          await client.request(MsgId.C2S_LANDLORD_CALL,
            toBinary(LandlordCallRequestSchema, create(LandlordCallRequestSchema, { turn: snapshot.turn, yes: true })),
            MsgId.S2C_ACTION_ACK, ActionAckSchema);
        } else {
          await client.request(MsgId.C2S_LANDLORD_ROB,
            toBinary(LandlordRobRequestSchema, create(LandlordRobRequestSchema, { turn: snapshot.turn, yes: true })),
            MsgId.S2C_ACTION_ACK, ActionAckSchema);
        }
      } catch {
        await recover(); // 结果未知时只恢复场景，不重发原动作。
      }
      continue;
    }
    if (snapshot.canAct && !trustee && snapshot.phase === "playing" && snapshot.seat === snapshot.turnSeat && lastAction !== action) {
      lastAction = action;
      const selected = snapshot.lastCards.length === 0 ? snapshot.hand[0] :
        snapshot.lastCards.length === 1 ? snapshot.hand.find(card => rank(card) > rank(snapshot.lastCards[0]!)) : undefined;
      const cards = selected === undefined ? [] : [selected];
      print("landlord_action", { turn: snapshot.turn, cards });
      try {
        await client.request(MsgId.C2S_LANDLORD_PLAY,
          toBinary(LandlordPlayRequestSchema, create(LandlordPlayRequestSchema, { turn: snapshot.turn, cards })),
          MsgId.S2C_ACTION_ACK, ActionAckSchema);
      } catch {
        await recover();
      }
      continue;
    }
    // 定时器只在当前阶段预计应有下一次变化后查询，不持续轮询牌桌。
    if (!await client.waitLandlord(snapshot.revision, client.landlordRecoveryWait())) {
      await recover();
    }
  }
  await client.request(MsgId.C2S_LANDLORD_LEAVE,
    toBinary(LandlordLeaveRequestSchema, create(LandlordLeaveRequestSchema)), MsgId.S2C_ACTION_ACK, ActionAckSchema);
  client.leaveLandlord();
  print("landlord_leave", {});
}
