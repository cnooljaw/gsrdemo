import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import type { DescMessage, MessageShape } from "@bufbuild/protobuf";
import { EnvelopeSchema, ErrorResponseSchema } from "./gen/gsr/common/v1/common_pb.js";
import { MsgId, MoleStatePushSchema } from "./gen/demo/business/v1/demo_pb.js";
import type { MoleSnapshot } from "./gen/demo/business/v1/demo_pb.js";

// DemoClient 拥有一个 access 连接及请求关联；游戏节点地址不进入客户端。
export class DemoClient {
  private readonly socket: WebSocket;
  private nextRequestId = 0;
  private readonly pending = new Map<number, {
    expectedId: number;
    resolve: (payload: Uint8Array) => void;
    reject: (error: Error) => void;
    timer: ReturnType<typeof setTimeout>;
  }>();
  snapshot: MoleSnapshot | undefined;

  private constructor(url: string) {
    this.socket = new WebSocket(url);
    this.socket.binaryType = "arraybuffer";
    this.socket.addEventListener("message", (event) => {
      try {
        if (!(event.data instanceof ArrayBuffer)) throw new Error("nonbinary response");
        const envelope = fromBinary(EnvelopeSchema, new Uint8Array(event.data));
        if (envelope.requestId === 0) {
          if (envelope.msgId === MsgId.S2C_MOLE_STATE) {
            const snapshot = fromBinary(MoleStatePushSchema, envelope.payload).snapshot;
            if (snapshot && this.snapshot?.roomId === snapshot.roomId && snapshot.stateVersion > this.snapshot.stateVersion) this.snapshot = snapshot;
          } else if (envelope.msgId === MsgId.S2C_SESSION_REPLACED) {
            this.socket.close();
          }
          return;
        }
        const request = this.pending.get(envelope.requestId);
        if (!request) return;
        clearTimeout(request.timer);
        this.pending.delete(envelope.requestId);
        if (envelope.msgId === 9001) request.reject(new Error(`protocol: ${fromBinary(ErrorResponseSchema, envelope.payload).code}`));
        else if (envelope.msgId !== request.expectedId) request.reject(new Error("response type mismatch"));
        else request.resolve(envelope.payload);
      } catch (error) {
        this.fail(error instanceof Error ? error : new Error(String(error)));
        this.socket.close();
      }
    });
    this.socket.addEventListener("close", () => this.fail(new Error("connection closed")));
    this.socket.addEventListener("error", () => this.fail(new Error("connection error")));
  }

  static async connect(url: string): Promise<DemoClient> {
    const client = new DemoClient(url);
    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => { client.socket.close(); reject(new Error("connection timeout")); }, 5000);
      client.socket.addEventListener("open", () => { clearTimeout(timer); resolve(); }, { once: true });
      client.socket.addEventListener("error", () => { clearTimeout(timer); reject(new Error("connection error")); }, { once: true });
    });
    return client;
  }

  request<Q extends DescMessage, R extends DescMessage>(requestId: number, responseId: number, requestSchema: Q, responseSchema: R, message: MessageShape<Q>): Promise<MessageShape<R>> {
    if (this.socket.readyState !== WebSocket.OPEN) return Promise.reject(new Error("connection closed"));
    const correlation = ++this.nextRequestId;
    return new Promise<MessageShape<R>>((resolve, reject) => {
      const timer = setTimeout(() => { this.pending.delete(correlation); reject(new Error("request timeout; operation outcome may be unknown")); }, 5000);
      this.pending.set(correlation, {
        expectedId: responseId,
        resolve: (payload) => { try { resolve(fromBinary(responseSchema, payload)); } catch (error) { reject(error); } },
        reject, timer,
      });
      try {
        this.socket.send(toBinary(EnvelopeSchema, create(EnvelopeSchema, { msgId: requestId, requestId: correlation, payload: toBinary(requestSchema, message) })));
      } catch (error) {
        clearTimeout(timer);
        this.pending.delete(correlation);
        reject(error);
      }
    });
  }

  install(snapshot: MoleSnapshot | undefined): void {
    if (!snapshot) return;
    if (this.snapshot?.roomId !== snapshot.roomId || snapshot.stateVersion >= this.snapshot.stateVersion) this.snapshot = snapshot;
  }

  private fail(error: Error): void {
    for (const request of this.pending.values()) { clearTimeout(request.timer); request.reject(error); }
    this.pending.clear();
  }

  async close(): Promise<void> {
    if (this.socket.readyState === WebSocket.CLOSED) return;
    await new Promise<void>((resolve) => {
      const timer = setTimeout(resolve, 1000);
      this.socket.addEventListener("close", () => { clearTimeout(timer); resolve(); }, { once: true });
      this.socket.close();
    });
  }
}
