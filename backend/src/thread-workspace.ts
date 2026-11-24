import { isAbsolute } from "node:path";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { AgentDoc, type ConversationId, type Harness } from "@earendil-works/pi-durable";
import { WireError } from "../../server/src/wire.js";
import { ThreadPresentationDoc } from "./thread-presentation.js";

// A metadata request addresses a persisted native checkout, never a peer's
// filesystem path, process CWD, or a thread synthesized for the request.
export async function threadWorkspace(h: Harness, threadId: string, signal: AbortSignal) {
  const id = Number(threadId) as ConversationId;
  const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
  const record = await h.commit(tx => tx.conversation(id), ctx);
  if (!record || record.owner) throw new WireError("not_found");
  const [agent, presentation] = await Promise.all([h.snapshot(AgentDoc, id, ctx), h.snapshot(ThreadPresentationDoc, id, ctx)]);
  const cwd = agent?.cwd;
  if (presentation?.archived || !cwd || !isAbsolute(cwd) || !cwd.isWellFormed() || Buffer.byteLength(cwd) > 4096 || /[\x00-\x1f\x7f-\x9f]/.test(cwd)) {
    throw new WireError("not_found");
  }
  signal.throwIfAborted();
  return cwd;
}
