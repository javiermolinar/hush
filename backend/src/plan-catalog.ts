import { createHash, randomUUID } from "node:crypto";
import { setImmediate as yieldTurn } from "node:timers/promises";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import type { Cursor, Harness, Storage } from "@earendil-works/pi-durable";
import { planWorkspacePageSize, type PlanSummary, type PlanWorkspace } from "../../server/src/plan.js";
import { ChangePulse, observe } from "./workspace-changes.js";

export type WorkspacePlans = { cursor: string; rows: PlanSummary[] };

// Persisted rows may come from another host build. Never spread storage
// fields into the wire format; only the catalog supplies a foreign workspace.
export function projectPlanSummary({ id, title, revision, total, verified, status, closed, reportedDone }: PlanSummary,
    chat: boolean, workspace?: string): PlanSummary {
  return { id, title, revision, total, verified, status, closed,
    ...(chat && reportedDone && { reportedDone }), ...(workspace !== undefined && { workspace }) };
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

  async page(cwd: string, page: number, after: string | undefined, peer: AbortSignal, chat: boolean): Promise<PlanWorkspace> {
    const signal = AbortSignal.any([peer, this.#life.signal]);
    await observe(this.#ready, signal);
    for (;;) {
      signal.throwIfAborted();
      const generation = this.#changes.generation;
      const revision = createHash("sha256").update(JSON.stringify([this.#epoch, cwd, page, generation, chat])).digest("hex");
      if (revision !== after) {
        const other = [...this.#workspaces.keys()].filter(path => path !== cwd).sort();
        const ordered = [cwd, ...other];
        const plans: PlanSummary[] = [];
        let skip = page * planWorkspacePageSize;
        for (const closed of [false, true]) for (const workspace of ordered) {
          const rows = this.#workspaces.get(workspace)?.rows ?? [];
          for (let i = rows.length - 1; i >= 0; i--) {
            const row = rows[i]!;
            if (row.closed !== closed) continue;
            if (skip > 0) { skip--; continue; }
            if (plans.length === planWorkspacePageSize) return { revision, plans, more: true };
            plans.push(projectPlanSummary(row, chat, workspace === cwd ? undefined : workspace));
          }
        }
        return { revision, plans, more: false };
      }
      await this.#changes.wait(generation, signal);
    }
  }
}
