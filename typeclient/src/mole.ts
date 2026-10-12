import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import { setTimeout as delay } from "node:timers/promises";
import { create } from "@bufbuild/protobuf";
import * as pb from "./gen/demo/business/v1/demo_pb.js";
import { DemoClient } from "./client.js";

function check(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

function options() {
  let url = "ws://127.0.0.1:7888/ws";
  let accounts = "configs/clients.local.json";
  for (let index = 2; index < process.argv.length; index += 2) {
    const value = process.argv[index + 1];
    if (value === undefined) throw new Error("usage: mole [--url URL] [--accounts FILE]");
    if (process.argv[index] === "--url") url = value;
    else if (process.argv[index] === "--accounts") accounts = value;
    else throw new Error("unknown option");
  }
  return { url, accounts };
}

async function snapshot(client: DemoClient) {
  const reply = await client.request(pb.MsgId.C2S_MOLE_SNAPSHOT, pb.MsgId.S2C_MOLE_SNAPSHOT, pb.MoleSnapshotRequestSchema, pb.MoleSnapshotResponseSchema, create(pb.MoleSnapshotRequestSchema));
  check(reply.code === "ok" && reply.snapshot, `snapshot: ${reply.code}`);
  client.install(reply.snapshot);
  return client.snapshot!;
}

async function login(client: DemoClient, account: { username: string; password: string }) {
  const reply = await client.request(pb.MsgId.C2S_PASSWORD_LOGIN, pb.MsgId.S2C_PASSWORD_LOGIN, pb.PasswordLoginRequestSchema, pb.PasswordLoginResponseSchema, create(pb.PasswordLoginRequestSchema, { operationId: randomUUID(), ...account }));
  check(reply.result?.code === "ok" && reply.result.session, `login: ${reply.result?.code}`);
  client.install(reply.result.snapshot);
  return reply.result;
}

async function enter(client: DemoClient) {
  const reply = await client.request(pb.MsgId.C2S_ENTER_MOLE, pb.MsgId.S2C_ENTER_MOLE, pb.EnterMoleRequestSchema, pb.EnterMoleResponseSchema, create(pb.EnterMoleRequestSchema, { operationId: randomUUID() }));
  check(reply.result?.code === "ok" && reply.result.snapshot, `enter: ${reply.result?.code}`);
  client.install(reply.result.snapshot);
  return reply.result;
}

async function run() {
  const { url, accounts } = options();
  const credentials: { username: string; password: string }[] = JSON.parse(await readFile(accounts, "utf8"));
  check(Array.isArray(credentials) && credentials.length === 4 && credentials.every((account) => typeof account.username === "string" && typeof account.password === "string"), "provide four test accounts");
  const clients: DemoClient[] = [];
  try {
    for (let index = 0; index < 4; index++) clients.push(await DemoClient.connect(url));
    const denied = await clients[0].request(pb.MsgId.C2S_MOLE_HIT, pb.MsgId.S2C_MOLE_HIT, pb.MoleHitRequestSchema, pb.MoleHitResponseSchema, create(pb.MoleHitRequestSchema, { operationId: randomUUID() }));
    check(denied.code === "not_authenticated", "guest hit admitted");
    const wrong = await clients[0].request(pb.MsgId.C2S_PASSWORD_LOGIN, pb.MsgId.S2C_PASSWORD_LOGIN, pb.PasswordLoginRequestSchema, pb.PasswordLoginResponseSchema, create(pb.PasswordLoginRequestSchema, { operationId: randomUUID(), username: credentials[0].username, password: "wrong-password" }));
    check(wrong.result?.code === "invalid_credentials" && wrong.result.session?.location === "guest", "wrong password logged in");
    await delay(300);
    const sessions = await Promise.all(clients.map((client, index) => login(client, credentials[index])));
    check(sessions.every((result) => result.session?.location === "lobby"), "login did not confirm lobby");
    const entries: pb.PositionResult[] = [];
    for (const client of clients) entries.push(await enter(client));
    const roomId = entries[0].session!.roomId;
    check(entries.slice(0, 3).every((entry) => entry.session?.roomId === roomId) && entries[3].session?.roomId !== roomId && entries[3].snapshot?.phase === "filling", "group allocation or isolation");
    const snapshots = await Promise.all(clients.slice(0, 3).map(snapshot));
    check(snapshots.every((state) => state.phase === "running" && state.members.length === 3 && state.target?.instanceId === snapshots[0].target?.instanceId), "shared target mismatch");
    check(snapshots[0].target, "missing target");
    const target = snapshots[0].target.instanceId;
    const requests = clients.slice(0, 3).map(() => create(pb.MoleHitRequestSchema, { operationId: randomUUID(), roomId, roundId: snapshots[0].roundId, instanceId: target }));
    const hits = await Promise.all(clients.slice(0, 3).map((client, index) => client.request(pb.MsgId.C2S_MOLE_HIT, pb.MsgId.S2C_MOLE_HIT, pb.MoleHitRequestSchema, pb.MoleHitResponseSchema, requests[index])));
    check(hits.filter((hit) => hit.code === "ok").length === 1, `competition: ${hits.map((hit) => hit.code)}`);
    const winner = hits.findIndex((hit) => hit.code === "ok");
    const repeated = await clients[winner].request(pb.MsgId.C2S_MOLE_HIT, pb.MsgId.S2C_MOLE_HIT, pb.MoleHitRequestSchema, pb.MoleHitResponseSchema, requests[winner]);
    check(repeated.code === "ok" && repeated.snapshot?.members.reduce((sum, member) => sum + Number(member.score), 0) === 10, "repeated hit rewarded again");
    const isolated = await snapshot(clients[3]);
    check(isolated.target === undefined && isolated.members.length === 1 && isolated.members[0].score === 0n, "cross-group state leaked");
    const spoofed = await clients[0].request(pb.MsgId.C2S_MOLE_HIT, pb.MsgId.S2C_MOLE_HIT, pb.MoleHitRequestSchema, pb.MoleHitResponseSchema, create(pb.MoleHitRequestSchema, { ...requests[0], operationId: randomUUID(), roomId: isolated.roomId }));
    check(spoofed.code === "state_mismatch", "client selected another room route");
    const leave = await clients[3].request(pb.MsgId.C2S_LEAVE_ROOM, pb.MsgId.S2C_LEAVE_ROOM, pb.LeaveRoomRequestSchema, pb.LeaveRoomResponseSchema, create(pb.LeaveRoomRequestSchema, { operationId: randomUUID(), roomId: isolated.roomId }));
    check(leave.result?.code === "ok" && leave.result.session?.location === "lobby", "filling leave failed");
    // 新连接只换绑定，恢复同一局、分数和期限；旧连接随后关闭。
    const replacement = await DemoClient.connect(url);
    clients.push(replacement);
    const reconnected = await login(replacement, credentials[0]);
    check(reconnected.session?.roomId === roomId && reconnected.snapshot?.endsAtMs === snapshots[0].endsAtMs, "reconnect reset round");
    const disconnected = winner === 1 ? 2 : 1;
    await clients[disconnected].close();
    const active = [replacement, ...clients.slice(1, 3).filter((_, index) => index + 1 !== disconnected)];
    console.log(JSON.stringify({ event: "running", room_id: roomId, winner: sessions[winner].session!.userId, score: 10 }));
    const waitUntil = Date.now() + 65000;
    let ended = await snapshot(replacement);
    while (ended.phase !== "ended" && Date.now() < waitUntil) { await delay(100); ended = await snapshot(replacement); }
    check(ended.phase === "ended" && ended.result?.progress === "unpersisted" && ended.result.deltas.length === 3, "missing frozen full-group result");
    check(ended.members.some((member) => member.userId === sessions[disconnected].session!.userId && !member.connected && member.score === 0n), "disconnected zero-score participant lost");
    check(ended.result.deltas.reduce((sum, member) => sum + Number(member.delta), 0) === 10, "incorrect group reward");
    const same = await Promise.all(active.map(snapshot));
    check(same.every((state) => state.result?.settlementId === ended.result?.settlementId), "different group results");
    const refused = await replacement.request(pb.MsgId.C2S_LEAVE_ROOM, pb.MsgId.S2C_LEAVE_ROOM, pb.LeaveRoomRequestSchema, pb.LeaveRoomResponseSchema, create(pb.LeaveRoomRequestSchema, { operationId: randomUUID(), roomId }));
    check(refused.result?.code === "settlement_pending" && refused.result.session?.location === "room", "uncredited member removed");
    const query = await replacement.request(pb.MsgId.C2S_QUERY_SESSION, pb.MsgId.S2C_QUERY_SESSION, pb.QuerySessionRequestSchema, pb.QuerySessionResponseSchema, create(pb.QuerySessionRequestSchema));
    check(query.result?.refreshed && query.result.session?.roomPhase === "ended" && !query.result.session.allowedMsgIds.includes(pb.MsgId.C2S_MOLE_HIT), "online location or admission not refreshed");
    console.log(JSON.stringify({ event: "complete", room_id: roomId, participants: ended.result.deltas.length, progress: ended.result.progress, score: 10 }));
  } finally {
    await Promise.all(clients.map((client) => client.close()));
  }
}

run().catch((error) => { console.error(error instanceof Error ? error.message : String(error)); process.exitCode = 1; });
