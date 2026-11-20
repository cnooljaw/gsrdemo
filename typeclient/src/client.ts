import { create, fromBinary, toBinary, type DescMessage, type MessageShape } from "@bufbuild/protobuf";
import { EnvelopeSchema, ErrorResponseSchema, MsgId as CommonMsgId, type Envelope } from "./gen/gsr/common/v1/common_pb.js";
import { type LandlordSnapshot, BalancePushSchema, BalanceSchema, MsgId, SnapshotPushSchema, type Balance, type Snapshot,
  LandlordSeatPushSchema, LandlordReadyPushSchema, LandlordDealPushSchema, LandlordBidPushSchema, LandlordPlayPushSchema, LandlordSettlementPushSchema, LandlordTrusteePushSchema, LandlordTurnPushSchema,
} from "./gen/demo/v1/game_pb.js";
import { applyLandlordEvent, type LandlordEvent } from "./landlord_state.js";
import { decodeDhmjEvent, type DhmjEvent } from "./dhmj_state.js";
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
  landlord?: LandlordSnapshot;
  private landlordEvents: LandlordEvent[] = [];
  private landlordOverflow = false;
  private landlordDiscardedThrough = 0n;
  private landlordWake?: () => void;
  private landlordClock = 0;
  private readonly connectionClosed = new AbortController();
  readonly closed = this.connectionClosed.signal;
  onDhmjPush?: (event: DhmjEvent) => void;
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

  // 进入和恢复使用完整场景；保留同时在途的较新事件，按序接续。
  setLandlord(snapshot: LandlordSnapshot): void {
    if (this.landlord?.roomName === snapshot.roomName && this.landlord.revision > snapshot.revision) return;
    if (this.landlord && this.landlord.roomName !== snapshot.roomName) this.landlordDiscardedThrough = 0n;
    this.landlord = snapshot;
    this.landlordClock = performance.now();
    this.landlordOverflow = snapshot.revision < this.landlordDiscardedThrough;
    this.landlordEvents = this.landlordEvents.filter(event => event.value.roomName === snapshot.roomName && event.value.revision > snapshot.revision);
    this.drainLandlordEvents();
    this.landlordWake?.();
  }

  leaveLandlord(): void {
    this.landlord = undefined;
    this.landlordEvents = [];
    this.landlordOverflow = false;
    this.landlordDiscardedThrough = 0n;
    this.landlordClock = 0;
  }

  // 服务端的截止时间与采样时间相减，只用单调时钟推算经过时间，不依赖客户端墙上时钟。
  landlordRecoveryWait(): number {
    const view = this.landlord;
    if (!view || view.phase === "finished" || view.serverMs === 0n) return 25_000;
    const target = view.canAct ? view.deadlineMs : view.startMs;
    if (target === 0n) return 25_000;
    const remaining = Number(target - view.serverMs) - (performance.now() - this.landlordClock);
    return Math.max(1000, Math.min(25_000, remaining + 1000));
  }

  get landlordNeedsSync(): boolean {
    return this.landlordOverflow || !!(this.landlord && this.landlordEvents.some(event => event.value.roomName === this.landlord!.roomName && event.value.revision > this.landlord!.revision + 1n));
  }

  async waitLandlord(revision: bigint, timeoutMs: number): Promise<boolean> {
    if (this.socket.readyState !== WebSocket.OPEN) throw new Error("connection closed; landlord scene must be restored");
    if (!this.landlord || this.landlord.revision !== revision || this.landlordNeedsSync) return true;
    return await new Promise<boolean>(resolve => {
      const timer = setTimeout(() => { this.landlordWake = undefined; resolve(false); }, timeoutMs);
      this.landlordWake = () => { clearTimeout(timer); this.landlordWake = undefined; resolve(true); };
    });
  }

  private receiveLandlordEvent(event: LandlordEvent): void {
    if (!event.value.roomName || event.value.revision === 0n) throw new Error("invalid landlord event");
    if (this.landlord && event.value.roomName !== this.landlord.roomName) return;
    if (this.landlord && event.value.revision <= this.landlord.revision) return;
    if (this.landlordEvents.length >= 128) {
      for (const item of this.landlordEvents) {
        if (item.value.revision > this.landlordDiscardedThrough) this.landlordDiscardedThrough = item.value.revision;
      }
      if (event.value.revision > this.landlordDiscardedThrough) this.landlordDiscardedThrough = event.value.revision;
      this.landlordEvents = [];
      this.landlordOverflow = true;
    } else if (!this.landlordEvents.some(item => item.value.roomName === event.value.roomName && item.value.revision === event.value.revision)) {
      this.landlordEvents.push(event);
    }
    this.drainLandlordEvents();
    this.landlordWake?.();
  }

  private drainLandlordEvents(): void {
    const view = this.landlord;
    if (!view || this.landlordOverflow) return;
    for (;;) {
      const index = this.landlordEvents.findIndex(event => event.value.roomName === view.roomName && event.value.revision === view.revision + 1n);
      if (index < 0) break;
      const [event] = this.landlordEvents.splice(index, 1);
      try {
        applyLandlordEvent(view, event!);
      } catch {
        this.landlordOverflow = true;
        this.landlordDiscardedThrough = event!.value.revision;
        return;
      }
      if (event!.kind === "deal" || event!.kind === "bid" || event!.kind === "play" || event!.kind === "trustee" || event!.kind === "turn") {
        this.landlordClock = performance.now();
      }
      this.onPush(`landlord_event_${event!.kind}`, event!.value);
    }
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
    const dhmj = this.onDhmjPush && decodeDhmjEvent(envelope);
    if (dhmj) {
      this.onDhmjPush?.(dhmj);
    } else if (envelope.msgId === MsgId.S2C_LANDLORD_SEAT_PUSH) {
      this.receiveLandlordEvent({ kind: "seat", value: fromBinary(LandlordSeatPushSchema, envelope.payload) });
    } else if (envelope.msgId === MsgId.S2C_LANDLORD_READY_PUSH) {
      this.receiveLandlordEvent({ kind: "ready", value: fromBinary(LandlordReadyPushSchema, envelope.payload) });
    } else if (envelope.msgId === MsgId.S2C_LANDLORD_DEAL_PUSH) {
      this.receiveLandlordEvent({ kind: "deal", value: fromBinary(LandlordDealPushSchema, envelope.payload) });
    } else if (envelope.msgId === MsgId.S2C_LANDLORD_BID_PUSH) {
      this.receiveLandlordEvent({ kind: "bid", value: fromBinary(LandlordBidPushSchema, envelope.payload) });
    } else if (envelope.msgId === MsgId.S2C_LANDLORD_PLAY_PUSH) {
      this.receiveLandlordEvent({ kind: "play", value: fromBinary(LandlordPlayPushSchema, envelope.payload) });
    } else if (envelope.msgId === MsgId.S2C_LANDLORD_SETTLEMENT_PUSH) {
      this.receiveLandlordEvent({ kind: "settlement", value: fromBinary(LandlordSettlementPushSchema, envelope.payload) });
    } else if (envelope.msgId === MsgId.S2C_LANDLORD_TRUSTEE_PUSH) {
      this.receiveLandlordEvent({ kind: "trustee", value: fromBinary(LandlordTrusteePushSchema, envelope.payload) });
    } else if (envelope.msgId === MsgId.S2C_LANDLORD_TURN_PUSH) {
      this.receiveLandlordEvent({ kind: "turn", value: fromBinary(LandlordTurnPushSchema, envelope.payload) });
    } else if (envelope.msgId === MsgId.S2C_SNAPSHOT_PUSH) {
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
    this.connectionClosed.abort(error);
    this.handshake?.reject(error);
    this.handshake = undefined;
    for (const waiting of this.pending.values()) {
      clearTimeout(waiting.timer);
      waiting.reject(error);
    }
    this.pending.clear();
    this.landlordWake?.();
  }
}
