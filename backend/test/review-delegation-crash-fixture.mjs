// Disposable process fixture: crash immediately after the named public
// Durable commit has reached SQLite, before returning any lost RPC reply.
import { startBackend } from "../../dist/hush-backend.mjs";

const [raw, boundary] = process.argv.slice(2);
const backend = await startBackend(JSON.parse(raw));
const workers = new Set();
backend.harness.observeCommits({ advance({ changes }) {
  let reached = false;
  for (const change of changes) {
    if (change.type === "task" && change.value.kind === "hush.review-worker") {
      workers.add(change.value.id);
      reached ||= boundary === "reservation" && change.value.state.status === "pending";
      reached ||= boundary === "timeout-decision" && change.value.state.status === "running" && change.value.state.checkpoint.phase === "timeout";
    }
    if (change.type === "document" && change.record.kind === "hush.code-review-snapshot") reached ||= boundary === "snapshot";
    if (change.type === "document" && change.kind === "change" && change.record.kind === "hush.review-releases" &&
        Object.keys(change.value.byHandle).length) reached ||= boundary === "release-decision";
    if (change.type === "conversation" && workers.has(change.value.owner?.taskId)) reached ||= boundary === "child-open";
    if (change.type === "submission" && change.value.requestId?.startsWith("review:")) {
      reached ||= boundary === "child-start" && change.value.status === "placed";
      reached ||= boundary === "child-settled" && change.value.status === "done";
    }
    if (change.type === "entry" && change.value.kind === "hush.worker.notice") reached ||= boundary === "notice";
    if (change.type === "entry" && change.value.kind === "pi.tool-result" && change.value.model?.[0]?.toolName === "hush_join_reviews")
      reached ||= boundary === "joined";
  }
  if (reached) process.kill(process.pid, "SIGKILL");
}, end() {} });
process.stdout.write("ready\n");
process.on("SIGTERM", () => { void backend.close(); });
await backend.closed;
