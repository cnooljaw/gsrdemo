import { randomUUID } from "node:crypto";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { EnvelopeSchema, ErrorResponseSchema } from "./gen/gsr/common/v1/common_pb.js";
import { MsgId, ProbeRequestSchema, ProbeResponseSchema, ProbePushSchema } from "./gen/demo/diagnostic/v1/diagnostic_pb.js";
import type { ProbeResponse, ProbePush } from "./gen/demo/diagnostic/v1/diagnostic_pb.js";

function options() {
  let url = "ws://127.0.0.1:7888/ws";
  let text = "hello demo";
  const args = process.argv.slice(2);
  for (let index = 0; index < args.length; index += 2) {
    const value = args[index + 1];
    if (value === undefined) throw new Error("usage: client [--url URL] [--text TEXT]");
    if (args[index] === "--url") url = value;
    else if (args[index] === "--text") text = value;
    else throw new Error("usage: client [--url URL] [--text TEXT]");
  }
  return { url, text };
}

async function probe(url: string, text: string): Promise<void> {
  const operationId = randomUUID();
  const requestId = 7;
  const socket = new WebSocket(url);
  socket.binaryType = "arraybuffer";
  let response: ProbeResponse | undefined;
  let push: ProbePush | undefined;
  let finished = false;
  try {
    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => finish(new Error("probe response/push timed out")), 5000);
      function finish(error?: Error) {
        if (finished) return;
        finished = true;
        clearTimeout(timer);
        if (error) reject(error);
        else resolve();
      }
      function complete() {
        if (!response || !push) return;
        if (response.nodes.join(",") !== "login,online,mole,settlement" || push.node !== "mole") {
          throw new Error("probe node result mismatch");
        }
        console.log(JSON.stringify({ event: "complete", operation_id: operationId, text, nodes: response.nodes }));
        finish();
      }
      socket.addEventListener("open", () => {
        if (finished) return;
        const request = create(ProbeRequestSchema, { operationId, text });
        socket.send(toBinary(EnvelopeSchema, create(EnvelopeSchema, {
          msgId: MsgId.C2S_PROBE_REQUEST, requestId, payload: toBinary(ProbeRequestSchema, request),
        })));
      });
      socket.addEventListener("message", (event) => {
        if (finished) return;
        try {
          if (!(event.data instanceof ArrayBuffer)) throw new Error("expected binary Envelope");
          const envelope = fromBinary(EnvelopeSchema, new Uint8Array(event.data));
          if (envelope.msgId === 9001) {
            const error = fromBinary(ErrorResponseSchema, envelope.payload);
            throw new Error(`protocol error ${error.code}: ${error.message}`);
          }
          if (envelope.msgId === MsgId.S2C_PROBE_RESPONSE) {
            if (envelope.requestId !== requestId || response) throw new Error("response correlation mismatch");
            response = fromBinary(ProbeResponseSchema, envelope.payload);
            if (response.operationId !== operationId || response.text !== text) throw new Error("response payload mismatch");
            console.log(JSON.stringify({ event: "response", code: response.code, failed_node: response.failedNode, nodes: response.nodes, text }));
            if (response.code !== "ok") throw new Error(`probe ${response.code}: ${response.failedNode}`);
          } else if (envelope.msgId === MsgId.S2C_PROBE_PUSH) {
            if (envelope.requestId !== 0 || push) throw new Error("push correlation mismatch");
            push = fromBinary(ProbePushSchema, envelope.payload);
            if (push.operationId !== operationId || push.text !== text) throw new Error("push payload mismatch");
            console.log(JSON.stringify({ event: "push", node: push.node, text }));
          } else {
            throw new Error(`unexpected msg_id ${envelope.msgId}`);
          }
          complete();
        } catch (error) { finish(error instanceof Error ? error : new Error(String(error))); }
      });
      socket.addEventListener("error", () => finish(new Error("WebSocket connection failed")));
      socket.addEventListener("close", () => {
        if (!finished) finish(new Error("connection closed before probe completed"));
      });
    });
  } finally {
    socket.close();
    // 原生 WebSocket 没有 terminate；关闭握手失联时也在 CLI 预算内退出。
    setTimeout(() => process.exit(process.exitCode === undefined ? 0 : Number(process.exitCode)), 1000).unref();
  }
}

try {
  const cfg = options();
  await probe(cfg.url, cfg.text);
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
}
