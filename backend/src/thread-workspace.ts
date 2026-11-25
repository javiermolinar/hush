import { stat } from "node:fs/promises";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import { AgentDoc, type ConversationId, type Harness } from "@earendil-works/pi-durable";
import { WireError } from "../../server/src/wire.js";
import { workspaceDirectory } from "../../server/src/workspace-files.js";
import { ThreadPresentationDoc } from "./thread-presentation.js";

// A metadata request addresses a persisted native checkout or, where its
// capability allows, a prospective thread's directory (prospectiveWorkspace).
// Never a process CWD or a thread synthesized for the request.
export async function threadWorkspace(h: Harness, threadId: string, signal: AbortSignal) {
  const id = Number(threadId) as ConversationId;
  const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
  const record = await h.commit(tx => tx.conversation(id), ctx);
  if (!record || record.owner) throw new WireError("not_found");
  const [agent, presentation] = await Promise.all([h.snapshot(AgentDoc, id, ctx), h.snapshot(ThreadPresentationDoc, id, ctx)]);
  const cwd = agent?.cwd;
  if (presentation?.archived || !workspaceDirectory(cwd)) throw new WireError("not_found");
  signal.throwIfAborted();
  return cwd;
}

// The existing directory a thread.new with this cwd would record, before any
// thread exists. Nothing is created or recorded. A peer could already create
// that thread and browse it, so the workspace grant remains the only gate.
export async function prospectiveWorkspace(cwd: string, signal: AbortSignal) {
  if (!workspaceDirectory(cwd)) throw new WireError("not_found");
  const info = await stat(cwd).catch(() => undefined);
  signal.throwIfAborted();
  if (!info?.isDirectory()) throw new WireError("not_found");
  return cwd;
}
