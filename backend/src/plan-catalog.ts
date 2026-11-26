import { createHash, randomUUID } from "node:crypto";
import { setImmediate as yieldTurn } from "node:timers/promises";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import type { Cursor, Harness, Storage } from "@earendil-works/pi-durable";
import { planWorkspacePageSize, type PlanSummary, type PlanWorkspace } from "../../server/src/plan.js";
import { ChangePulse, observe } from "./workspace-changes.js";

export type ActivePlanRun = { id: string; ownerThreadId: string; chat?: true };
type WorkspacePlan = PlanSummary & { activeRun?: ActivePlanRun };
export type WorkspacePlans = { cursor: string; rows: WorkspacePlan[] };

// Persisted rows may come from another host build. Never spread storage
// fields (including activeRun) into the wire format.
function projectPlanSummary({ id, title, revision, total, verified, status, closed, reportedDone }: PlanSummary,
    chat: boolean, workspace?: string): PlanSummary {
  return { id, title, revision, total, verified, status, closed,
    ...(chat && reportedDone && { reportedDone }), ...(workspace !== undefined && { workspace }) };
}

export function planInventoryPage(workspaces: readonly (readonly [string | undefined, readonly WorkspacePlan[]])[],
    page: number, chat: boolean, threadId?: string): Pick<PlanWorkspace, "plans" | "more"> {
  // A thread can queue multiple Run here plans. Promote the oldest active
  // run, like the chat activity feed, even if its plan is closing or foreign.
  let first: { row: WorkspacePlan; workspace?: string } | undefined, oldest = Infinity;
  if (threadId !== undefined) for (const [workspace, rows] of workspaces) for (const row of rows) {
    const run = row.activeRun;
    if (row.status !== "running" || run?.ownerThreadId !== threadId || run.chat && !chat) continue;
    const id = Number(run.id);
    if (id < oldest) {
      first = { row, workspace };
      oldest = id;
    }
  }
  const plans: PlanSummary[] = [];
  let skip = page * planWorkspacePageSize;
  if (first) {
    if (skip > 0) skip--;
    else plans.push(projectPlanSummary(first.row, chat, first.workspace));
  }
  for (const closed of [false, true]) for (const [workspace, rows] of workspaces) {
    for (let i = rows.length - 1; i >= 0; i--) {
      const row = rows[i]!;
      if (row === first?.row || row.closed !== closed) continue;
      if (skip > 0) { skip--; continue; }
      if (plans.length === planWorkspacePageSize) return { plans, more: true };
      plans.push(projectPlanSummary(row, chat, workspace));
    }
  }
  return { plans, more: false };
}

// Mirror the existing workspace summaries, not the full plans or their
// transcripts. This disposable index starts only when a client asks for all
// workspaces. It does not add a store-wide write to each plan transaction.
export class PlanCatalog {
  readonly #life = new AbortController();
  readonly #epoch = randomUUID();
  readonly #changes = new ChangePulse();
  readonly #workspaces = new Map<string, Readonly<WorkspacePlans>>();
  readonly #ready: Promise<void>;

  constructor(harness: Harness, storage: Storage, kind: string, version: number) {
    const { signal } = this.#life, ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const observer = harness.observeCommits({
      advance: ({ changes }) => {
        if (signal.aborted) return;
        for (const change of changes) {
          if (change.type !== "document" || change.record.scope.kind !== "session" || change.record.kind !== kind ||
              change.record.key === undefined) continue;
          if (change.kind !== "change" || change.version !== version) {
            // Workspace catalogs are never retired or migrated in a live
            // session. Don't keep serving a stale index if that changes.
            this.#life.abort();
            return;
          }
          this.#workspaces.set(change.record.key, change.value as WorkspacePlans);
          this.#changes.emit();
        }
      },
      end: () => this.#life.abort(),
    });
    this.#ready = (async () => {
      let cursor: Cursor | undefined;
      do {
        const page = await storage.scanDocuments({ scope: { kind: "session" }, at: "current", kind }, 32, cursor, ctx);
        for (const { id, key } of page.items) {
          if (key === undefined || this.#workspaces.has(key)) continue;
          const doc = await storage.document(id, "current", ctx);
          // A commit that raced the scan already installed its exact stored
          // replacement. Never clobber it with an earlier storage read.
          if (this.#workspaces.has(key)) continue;
          if (!doc || doc.version !== version) throw new Error("Cannot read workspace plan catalog");
          this.#workspaces.set(key, doc.value as WorkspacePlans);
        }
        cursor = page.next;
        if (cursor) await yieldTurn(undefined, { signal });
      } while (cursor);
    })().catch(error => { observer.detach(); throw error; });
  }

  async page(cwd: string, page: number, after: string | undefined, peer: AbortSignal, chat: boolean, threadId?: string): Promise<PlanWorkspace> {
    const signal = AbortSignal.any([peer, this.#life.signal]);
    await observe(this.#ready, signal);
    for (;;) {
      signal.throwIfAborted();
      const generation = this.#changes.generation;
      const revision = createHash("sha256").update(JSON.stringify([this.#epoch, cwd, page, generation, chat, threadId])).digest("hex");
      if (revision !== after) {
        const other = [...this.#workspaces.keys()].filter(path => path !== cwd).sort();
        const workspaces = [cwd, ...other].map(path => [path === cwd ? undefined : path, this.#workspaces.get(path)?.rows ?? []] as const);
        return { revision, ...planInventoryPage(workspaces, page, chat, threadId) };
      }
      await this.#changes.wait(generation, signal);
    }
  }
}
