import { createHash, randomUUID } from "node:crypto";
import { setImmediate as yieldTurn } from "node:timers/promises";
import { BACKGROUND_CONTEXT, withAbortSignal } from "@earendil-works/chord/context";
import type { Cursor, Harness, JsonObject, Storage } from "@earendil-works/pi-durable";
import { planListPageSize, type PlanList, type PlanSummary } from "../../server/src/plan.js";
import { object } from "../../server/src/wire.js";
import { ChangePulse, observe } from "./workspace-changes.js";

export type PlanRow = Omit<PlanSummary, "workspace">;
export type WorkspacePlans = { cursor: string; rows: PlanRow[] };
export const planCatalogVersion = 2;

// Version 1 counted backend-verified and model-reported steps separately.
// Both are done now; blocked counts are filled in by the startup reindex.
export function migrateWorkspacePlans(value: JsonObject, from: number): WorkspacePlans {
  if (from !== 1) throw new Error(`Unknown plan catalog version ${from}`);
  const rows = Array.isArray(value.rows) ? (value.rows as unknown[]).filter(object) : [];
  return { cursor: typeof value.cursor === "string" ? value.cursor : "0".repeat(64), rows: rows.map(r => ({
    id: String(r.id), title: String(r.title), revision: Number(r.revision), total: Number(r.total),
    done: Number(r.verified ?? 0) + Number(r.reportedDone ?? 0), blocked: 0, running: false, closed: r.closed === true,
  })) };
}

// Persisted rows may come from another build. Never spread storage fields into the wire format.
function summary({ id, title, revision, total, done, blocked, running, closed }: PlanRow, workspace: string): PlanSummary {
  return { id, title, revision, workspace, total, done, blocked, running, closed };
}

// Open plans first, newest first within each workspace; the caller's workspace leads.
export function planInventoryPage(workspaces: readonly (readonly [string, readonly PlanRow[]])[], page: number): Pick<PlanList, "plans" | "more"> {
  const plans: PlanSummary[] = [];
  let skip = page * planListPageSize;
  for (const closed of [false, true]) for (const [workspace, rows] of workspaces) {
    for (let i = rows.length - 1; i >= 0; i--) {
      const row = rows[i]!;
      if (row.closed !== closed) continue;
      if (skip > 0) { skip--; continue; }
      if (plans.length === planListPageSize) return { plans, more: true };
      plans.push(summary(row, workspace));
    }
  }
  return { plans, more: false };
}

// Mirror the per-workspace summaries, not the plans. This disposable index
// starts only when a client lists plans; it adds no store-wide write.
export class PlanCatalog {
  readonly #life = new AbortController();
  readonly #epoch = randomUUID();
  readonly #changes = new ChangePulse();
  readonly #workspaces = new Map<string, Readonly<WorkspacePlans>>();
  readonly #ready: Promise<void>;

  constructor(harness: Harness, storage: Storage, kind: string) {
    const { signal } = this.#life, ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const observer = harness.observeCommits({
      advance: ({ changes }) => {
        if (signal.aborted) return;
        for (const change of changes) {
          if (change.type !== "document" || change.record.scope.kind !== "session" || change.record.kind !== kind ||
              change.record.key === undefined) continue;
          if (change.kind !== "change" || change.version !== planCatalogVersion) {
            // Workspace catalogs are never retired in a live session. Don't
            // keep serving a stale index if that changes.
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
          if (!doc || doc.version > planCatalogVersion) throw new Error("Cannot read workspace plan catalog");
          this.#workspaces.set(key, doc.version === planCatalogVersion ? doc.value as WorkspacePlans
            : migrateWorkspacePlans(doc.value as JsonObject, doc.version));
        }
        cursor = page.next;
        if (cursor) await yieldTurn(undefined, { signal });
      } while (cursor);
    })().catch(error => { observer.detach(); throw error; });
  }

  async page(cwd: string, page: number, after: string | undefined, peer: AbortSignal): Promise<PlanList> {
    const signal = AbortSignal.any([peer, this.#life.signal]);
    await observe(this.#ready, signal);
    for (;;) {
      signal.throwIfAborted();
      const generation = this.#changes.generation;
      const revision = createHash("sha256").update(JSON.stringify([this.#epoch, cwd, page, generation])).digest("hex");
      if (revision !== after) {
        const other = [...this.#workspaces.keys()].filter(path => path !== cwd).sort();
        const workspaces = [cwd, ...other].map(path => [path, this.#workspaces.get(path)?.rows ?? []] as const);
        return { revision, ...planInventoryPage(workspaces, page) };
      }
      await this.#changes.wait(generation, signal);
    }
  }
}
