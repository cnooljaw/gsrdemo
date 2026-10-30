// GSR v1.0.2 独立跨语言测试向量，保护本项目客户端的 MAC 兼容性。
import assert from "node:assert/strict";
import { test } from "node:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createPrivateKey } from "node:crypto";

import {
  authenticatedFrame,
  completeClientHandshake,
  handshakeVersion,
} from "./secure_auth.js";

interface ClientAuthVector {
  client_private_key: string;
  client_public_key: string;
  server_public_key: string;
  seq: string;
  seq_bytes: string;
  envelope_bytes: string;
  mac: string;
}

const vectorPath = fileURLToPath(new URL("../testdata/client-auth-vector.json", import.meta.url));
const privateKeyPrefix = Buffer.from("302e020100300506032b656e04220420", "hex");

function readVector(): ClientAuthVector {
  return JSON.parse(readFileSync(vectorPath, "utf8")) as ClientAuthVector;
}

function privateKeyFromRaw(encoded: string) {
  return createPrivateKey({
    key: Buffer.concat([privateKeyPrefix, Buffer.from(encoded, "hex")]),
    format: "der",
    type: "pkcs8",
  });
}

// TestProductionHandshakeAndFrameMatchVector 验证固定 33 字节握手和生产上行帧与跨语言向量一致。
test("固定握手和生产上行帧匹配跨语言向量", () => {
  const vector = readVector();
  const clientPublicKey = Buffer.from(vector.client_public_key, "hex");
  const handshake = {
    privateKey: privateKeyFromRaw(vector.client_private_key),
    publicKey: clientPublicKey,
    frame: Buffer.concat([Buffer.from([handshakeVersion]), clientPublicKey]),
  };
  assert.equal(handshake.frame.length, 33);
  const response = Buffer.concat([
    Buffer.from([handshakeVersion]),
    Buffer.from(vector.server_public_key, "hex"),
  ]);
  const authenticator = completeClientHandshake(handshake, response);
  authenticator.nextSequence = BigInt(`0x${vector.seq}`) - 1n;

  const frame = authenticatedFrame(authenticator, Buffer.from(vector.envelope_bytes, "hex"));
  assert.deepEqual(frame, Buffer.concat([
    Buffer.from(vector.seq_bytes, "hex"),
    Buffer.from(vector.envelope_bytes, "hex"),
    Buffer.from(vector.mac, "hex"),
  ]));
});

// TestProductionHandshakeRejectsWrongFormat 验证服务端握手响应的长度和版本不符合约定时立即失败。
test("生产握手拒绝错误长度和版本", () => {
  const vector = readVector();
  const clientPublicKey = Buffer.from(vector.client_public_key, "hex");
  const handshake = {
    privateKey: privateKeyFromRaw(vector.client_private_key),
    publicKey: clientPublicKey,
    frame: Buffer.concat([Buffer.from([handshakeVersion]), clientPublicKey]),
  };
  assert.throws(() => completeClientHandshake(handshake, Buffer.alloc(32)), /invalid server handshake/);
  const wrongVersion = Buffer.concat([
    Buffer.from([handshakeVersion + 1]),
    Buffer.from(vector.server_public_key, "hex"),
  ]);
  assert.throws(() => completeClientHandshake(handshake, wrongVersion), /invalid server handshake/);
  const invalidPublicKey = Buffer.concat([Buffer.from([handshakeVersion]), Buffer.alloc(32)]);
  assert.throws(() => completeClientHandshake(handshake, invalidPublicKey));
});

// TestClientSequenceExhaustionDoesNotWrap 验证客户端发出 uint64 最大序号后拒绝继续发送。
test("客户端序号耗尽后不回绕", () => {
  const maximumSequence = 0xffff_ffff_ffff_ffffn;
  const authenticator = {
    macKey: Buffer.alloc(32, 1),
    nextSequence: maximumSequence - 1n,
  };

  const finalFrame = authenticatedFrame(authenticator, new Uint8Array());
  assert.deepEqual(finalFrame.subarray(0, 8), Buffer.alloc(8, 0xff));
  assert.equal(authenticator.nextSequence, maximumSequence);
  assert.throws(() => authenticatedFrame(authenticator, new Uint8Array()), /client sequence exhausted/);
  assert.equal(authenticator.nextSequence, maximumSequence);
});
