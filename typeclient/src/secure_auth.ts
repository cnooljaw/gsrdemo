// 源自 GSR v1.0.2 websocket-gate 示例；本项目使用同一 MAC wire 契约。
import {
  createHash,
  createHmac,
  createPublicKey,
  diffieHellman,
  generateKeyPairSync,
  type KeyObject,
} from "node:crypto";

export const handshakeVersion = 1;
export const handshakeFrameSize = 33;
const publicKeySize = 32;
const sequenceMax = 0xffff_ffff_ffff_ffffn;
const sequenceSize = 8;
const macSize = 16;
const saltLabel = "gsr/wss/hkdf-salt/v1";
const keyInfo = "gsr/wss/c2s-hmac-sha256/v1";
const publicKeyPrefix = Buffer.from("302a300506032b656e032100", "hex");

export interface ClientHandshake {
  privateKey: KeyObject;
  publicKey: Buffer;
  frame: Uint8Array;
}

export interface ClientAuthenticator {
  readonly macKey: Buffer;
  nextSequence: bigint;
}

// createClientHandshake 生成一条连接独占的 X25519 客户端握手帧。
export function createClientHandshake(): ClientHandshake {
  const { privateKey, publicKey } = generateKeyPairSync("x25519");
  const clientPublicKey = rawPublicKey(publicKey);
  const frame = Buffer.alloc(handshakeFrameSize);
  frame[0] = handshakeVersion;
  clientPublicKey.copy(frame, 1);
  return { privateKey, publicKey: clientPublicKey, frame };
}

// completeClientHandshake 校验服务端固定格式握手响应并派生本连接的上行 MAC 密钥。
export function completeClientHandshake(handshake: ClientHandshake, response: Uint8Array): ClientAuthenticator {
  if (response.byteLength !== handshakeFrameSize || response[0] !== handshakeVersion) {
    throw new Error("invalid server handshake");
  }
  const serverPublicKey = Buffer.from(response.slice(1));
  const sharedSecret = diffieHellman({
    privateKey: handshake.privateKey,
    publicKey: publicKeyFromRaw(serverPublicKey),
  });
  return {
    macKey: deriveMACKey(sharedSecret, handshake.publicKey, serverPublicKey),
    nextSequence: 0n,
  };
}

// authenticatedFrame 为一条不透明业务消息添加严格递增的大端序列号和截断 MAC。
export function authenticatedFrame(authenticator: ClientAuthenticator, envelope: Uint8Array): Uint8Array {
  if (authenticator.nextSequence >= sequenceMax) {
    throw new Error("client sequence exhausted");
  }
  const sequence = authenticator.nextSequence + 1n;
  authenticator.nextSequence = sequence;
  const sequenceBytes = Buffer.alloc(sequenceSize);
  sequenceBytes.writeBigUInt64BE(sequence);
  const body = Buffer.concat([sequenceBytes, Buffer.from(envelope)]);
  const mac = createHmac("sha256", authenticator.macKey).update(body).digest().subarray(0, macSize);
  return Buffer.concat([body, mac]);
}

function rawPublicKey(key: KeyObject): Buffer {
  const encoded = key.export({ format: "der", type: "spki" });
  if (!Buffer.isBuffer(encoded) || encoded.length < publicKeySize) {
    throw new Error("invalid X25519 public key");
  }
  return encoded.subarray(encoded.length - publicKeySize);
}

function publicKeyFromRaw(raw: Uint8Array): KeyObject {
  if (raw.byteLength !== publicKeySize) {
    throw new Error("invalid X25519 public key");
  }
  return createPublicKey({
    key: Buffer.concat([publicKeyPrefix, Buffer.from(raw)]),
    format: "der",
    type: "spki",
  });
}

function deriveMACKey(sharedSecret: Uint8Array, clientPublicKey: Uint8Array, serverPublicKey: Uint8Array): Buffer {
  const saltInput = Buffer.concat([
    Buffer.from(saltLabel, "ascii"),
    Buffer.from(clientPublicKey),
    Buffer.from(serverPublicKey),
  ]);
  const salt = createHash("sha256").update(saltInput).digest();
  const prk = createHmac("sha256", salt).update(sharedSecret).digest();
  return createHmac("sha256", prk)
    .update(Buffer.concat([Buffer.from(keyInfo, "ascii"), Buffer.from([1])]))
    .digest();
}
