import assert from "node:assert/strict";
import { test } from "node:test";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { BalanceSchema, HitResponseSchema, LoginResponseSchema } from "./gen/demo/v1/game_pb.js";

// 保护客户端在银子和 UserID 超过 Number 安全范围时仍使用准确的 bigint。
test("银子和身份保持 64 位精度", () => {
  const amount = 9_223_372_036_854_775_807n;
  const reply = create(HitResponseSchema, { silver: create(BalanceSchema, { balance: amount, revision: 18_446_744_073_709_551_615n }) });
  const decoded = fromBinary(HitResponseSchema, toBinary(HitResponseSchema, reply));
  assert.equal(decoded.silver?.balance, amount);
  assert.equal(decoded.silver?.revision, 18_446_744_073_709_551_615n);
  const login = create(LoginResponseSchema, { userId: 18_446_744_073_709_551_615n });
  assert.equal(fromBinary(LoginResponseSchema, toBinary(LoginResponseSchema, login)).userId, login.userId);
});
