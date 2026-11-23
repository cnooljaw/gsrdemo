import { create, toBinary } from "@bufbuild/protobuf";
import type { DemoClient } from "./client.js";
import { DhmjScene } from "./dhmj_state.js";
import {
  ActionAckSchema, DhmjDiscardRequestSchema, DhmjEnterRequestSchema, DhmjEnterResponseSchema,
  DhmjLeaveRequestSchema, DhmjPassSelfRequestSchema, DhmjReadyRequestSchema, DhmjRespondRequestSchema,
  DhmjSelfWinRequestSchema, DhmjSnapshotRequestSchema, DhmjSnapshotResponseSchema, MsgId,
  type DhmjSnapshot,
} from "./gen/demo/v1/game_pb.js";

const claimHu = 6n;
const pass = 7n;

function chooseAction(view: DhmjSnapshot): { key: string; id: MsgId; payload: Uint8Array } | undefined {
  if (view.trustees[Number(view.seat)]) return;
  if (view.operationPhase === "respond" && view.offers.length !== 0) {
    const action = view.offers.includes(claimHu) ? claimHu : pass;
    return { key: `window:${view.windowId}`, id: MsgId.C2S_DHMJ_RESPOND,
      payload: toBinary(DhmjRespondRequestSchema, create(DhmjRespondRequestSchema, { windowId: view.windowId, action })) };
  }
  if (view.operationPhase === "self" && view.canSelfWin) {
    return { key: `turn:${view.turn}`, id: MsgId.C2S_DHMJ_SELF_WIN,
      payload: toBinary(DhmjSelfWinRequestSchema, create(DhmjSelfWinRequestSchema, { turn: view.turn })) };
  }
  if (view.operationPhase === "self" && view.selfAction) {
    return { key: `turn:${view.turn}`, id: MsgId.C2S_DHMJ_PASS_SELF,
      payload: toBinary(DhmjPassSelfRequestSchema, create(DhmjPassSelfRequestSchema, { turn: view.turn })) };
  }
  const tile = view.canDiscard ? view.hand.find(tile => tile > 0n && tile < 0x41n) : undefined;
  if (tile !== undefined) {
    return { key: `turn:${view.turn}`, id: MsgId.C2S_DHMJ_DISCARD,
      payload: toBinary(DhmjDiscardRequestSchema, create(DhmjDiscardRequestSchema, { turn: view.turn, tile })) };
  }
}

export async function runDhmj(client: DemoClient, print: (event: string, value: unknown) => void): Promise<void> {
  const scene = new DhmjScene(print);
  client.onDhmjPush = event => scene.accept(event);
  try {
    const entry = await client.request(MsgId.C2S_DHMJ_ENTER,
      toBinary(DhmjEnterRequestSchema, create(DhmjEnterRequestSchema)), MsgId.S2C_DHMJ_ENTER, DhmjEnterResponseSchema);
    if (!entry.snapshot) throw new Error("缺少麻将进桌快照");
    print("dhmj_enter", entry.snapshot);
    scene.restore(entry.snapshot);

    const recover = async () => {
      const reply = await client.request(MsgId.C2S_DHMJ_SNAPSHOT,
        toBinary(DhmjSnapshotRequestSchema, create(DhmjSnapshotRequestSchema)), MsgId.S2C_DHMJ_SNAPSHOT, DhmjSnapshotResponseSchema);
      if (!reply.snapshot) throw new Error("缺少麻将恢复快照");
      print("dhmj_recover", { revision: reply.snapshot.revision, phase: reply.snapshot.phase });
      scene.restore(reply.snapshot);
    };
    const sendAction = (id: MsgId, payload: Uint8Array) => client.request(id, payload, MsgId.S2C_ACTION_ACK, ActionAckSchema);
    let readySent = false;
    let lastAction = "";
    for (;;) {
      client.closed.throwIfAborted();
      if (scene.needsSync) {
        await recover();
        continue;
      }
      const view = scene.view!;
      if (view.phase === "finished" && view.settlement?.status === "committed") {
        print("dhmj_settlement", view.settlement);
        break;
      }
      if (view.phase === "waiting" && !readySent && !view.players.find(p => p.seat === view.seat)?.ready) {
        readySent = true;
        try {
          await sendAction(MsgId.C2S_DHMJ_READY, toBinary(DhmjReadyRequestSchema, create(DhmjReadyRequestSchema)));
        } catch (error) {
          await recover();
          const current = scene.view!;
          if (current.phase === "waiting" && !current.players.find(p => p.seat === current.seat)?.ready) throw error;
        }
        continue;
      }
      const action = chooseAction(view);
      if (action && action.key !== lastAction) {
        lastAction = action.key;
        const windowId = view.windowId;
        print("dhmj_action", { id: action.id, turn: view.turn, windowId });
        try {
          await sendAction(action.id, action.payload);
          const current = scene.view;
          if (action.id === MsgId.C2S_DHMJ_RESPOND &&
              current?.operationPhase === "respond" && current.windowId === windowId) {
            current.offers = [];
          }
        } catch {
          await recover();
        }
        continue;
      }
      if (!await scene.wait(client.closed)) await recover();
    }
    await sendAction(MsgId.C2S_DHMJ_LEAVE, toBinary(DhmjLeaveRequestSchema, create(DhmjLeaveRequestSchema)));
    print("dhmj_leave", {});
  } finally {
    client.onDhmjPush = undefined;
  }
}
