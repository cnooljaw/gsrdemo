import { create, fromBinary, toBinary, type DescMessage, type MessageShape } from "@bufbuild/protobuf";
import { EnvelopeSchema, ErrorResponseSchema, MsgId as CommonMsgId, type Envelope } from "./gen/gsr/common/v1/common_pb.js";
import { BalancePushSchema, BalanceSchema, MsgId, SnapshotPushSchema, type Balance, type Snapshot } from "./gen/demo/v1/game_pb.js";
import { authenticatedFrame, completeClientHandshake, createClientHandshake, type ClientAuthenticator } from "./secure_auth.js";

interface Pending {
  resolve: (envelope: Envelope) => void;
  reject: (error: Error) => void;
  timer: NodeJS.Timeout;
}

// DemoClient 只管理一条连接的协议请求和显示快照，不自行推算入账结果。
export class DemoClient {
  private readonly socket: WebSocket;
  private readonly pending = new Map<number, Pending>();
  private nextID = 0;
  private authenticator?: ClientAuthenticator;
  private handshake?: { resolve: (bytes: Uint8Array) => void; reject: (error: Error) => void };
  snapshot?: Snapshot;
  silver?: Balance;
  onPush: (kind: string, value: unknown) => void = () => {};

  private constructor(url: string, private readonly timeoutMs: number) {
    this.socket = new WebSocket(url);
    this.socket.binaryType = "arraybuffer";
    this.socket.addEventListener("message", (event) => {
      try {
        if (!(event.data instanceof ArrayBuffer)) throw new Error("expected binary WebSocket frame");
        const bytes = new Uint8Array(event.data);
        if (this.handshake) {
          const waiting = this.handshake;
          this.handshake = undefined;
          waiting.resolve(bytes);
          return;
        }
        this.receive(fromBinary(EnvelopeSchema, bytes));
      } catch (error) {
        this.fail(error instanceof Error ? error : new Error(String(error)));
        this.socket.close();
      }
    });
    this.socket.addEventListener("close", () => this.fail(new Error("connection closed; in-flight outcome may be unknown")));
    this.socket.addEventListener("error", () => this.fail(new Error("WebSocket connection failed")));
  }

  static async connect(url: string, timeoutMs = 5_000): Promise<DemoClient> {
    const client = new DemoClient(url, timeoutMs);
    try {
      await new Promise<void>((resolve, reject) => {
        const finish = (error?: Error) => {
          clearTimeout(timer);
          client.socket.removeEventListener("open", opened);
          client.socket.removeEventListener("error", failed);
          client.socket.removeEventListener("close", failed);
          if (error) reject(error); else resolve();
        };
        const opened = () => finish();
        const failed = () => finish(new Error("WebSocket did not open"));
        const timer = setTimeout(() => finish(new Error("connection timed out")), timeoutMs);
        client.socket.addEventListener("open", opened, { once: true });
        client.socket.addEventListener("error", failed, { once: true });
        client.socket.addEventListener("close", failed, { once: true });
      });
      const handshake = createClientHandshake();
      const response = await new Promise<Uint8Array>((resolve, reject) => {
        const timer = setTimeout(() => {
          client.handshake = undefined;
          reject(new Error("MAC handshake timed out"));
        }, timeoutMs);
        client.handshake = {
          resolve: (bytes) => { clearTimeout(timer); resolve(bytes); },
          reject: (error) => { clearTimeout(timer); reject(error); },
        };
        client.socket.send(handshake.frame);
      });
      client.authenticator = completeClientHandshake(handshake, response);
      return client;
    } catch (error) {
      client.close();
      throw error;
    }
  }

  async request<T extends DescMessage>(msgID: number, payload: Uint8Array, responseID: number, schema: T): Promise<MessageShape<T>> {
    if (!this.authenticator || this.socket.readyState !== WebSocket.OPEN) throw new Error("connection is not ready");
    if (this.nextID === 0xffff_ffff) throw new Error("request IDs exhausted; reconnect");
    const requestID = ++this.nextID;
    const result = new Promise<Envelope>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(requestID);
        reject(new Error(`request ${requestID} timed out; outcome may be unknown, keep the original ActionID`));
      }, this.timeoutMs);
      this.pending.set(requestID, { resolve, reject, timer });
      try {
        const envelope = create(EnvelopeSchema, { msgId: msgID, requestId: requestID, payload });
        this.socket.send(authenticatedFrame(this.authenticator!, toBinary(EnvelopeSchema, envelope)));
      } catch (error) {
        clearTimeout(timer);
        this.pending.delete(requestID);
        reject(error instanceof Error ? error : new Error(String(error)));
      }
    });
    const envelope = await result;
    if (envelope.msgId === CommonMsgId.S2C_ERROR_RESPONSE) {
      const response = fromBinary(ErrorResponseSchema, envelope.payload);
      throw new Error(`server error ${response.code}: ${response.message}`);
    }
    if (envelope.msgId !== responseID) throw new Error(`unexpected response ${envelope.msgId}`);
    return fromBinary(schema, envelope.payload);
  }

  // 重入以快照重新建立基线；同一缓存生命周期内只采纳更大的修订号。
  enter(snapshot: Snapshot, silver: Balance): void {
    this.snapshot = snapshot;
    this.silver = silver;
  }

  applyBalance(silver: Balance): boolean {
    if (this.silver && silver.revision <= this.silver.revision) return false;
    this.silver = create(BalanceSchema, {
      ...silver,
      confirmed: silver.confirmed ?? this.silver?.confirmed,
    });
    return true;
  }

  close(): void {
    this.fail(new Error("client closed"));
    if (this.socket.readyState === WebSocket.OPEN || this.socket.readyState === WebSocket.CONNECTING) this.socket.close();
  }

  private receive(envelope: Envelope): void {
    if (envelope.requestId !== 0) {
      const waiting = this.pending.get(envelope.requestId);
      if (!waiting) return; // 超时迟到响应不属于新的请求。
      this.pending.delete(envelope.requestId);
      clearTimeout(waiting.timer);
      waiting.resolve(envelope);
      return;
    }
    if (envelope.msgId === MsgId.S2C_SNAPSHOT_PUSH) {
      const message = fromBinary(SnapshotPushSchema, envelope.payload);
      if (message.snapshot && this.snapshot?.attackName === message.snapshot.attackName) {
        this.snapshot = message.snapshot;
        this.onPush("snapshot", message.snapshot);
      }
    } else if (envelope.msgId === MsgId.S2C_BALANCE_PUSH) {
      const message = fromBinary(BalancePushSchema, envelope.payload);
      if (message.silver && this.applyBalance(message.silver)) this.onPush("balance_correction", message.silver);
    } else {
      throw new Error(`unexpected push ${envelope.msgId}`);
    }
  }

  private fail(error: Error): void {
    this.handshake?.reject(error);
    this.handshake = undefined;
    for (const waiting of this.pending.values()) {
      clearTimeout(waiting.timer);
      waiting.reject(error);
    }
    this.pending.clear();
  }
}
