import { randomUUID } from "node:crypto";
import { realpathSync } from "node:fs";
import { BACKGROUND_CONTEXT, withAbortSignal, type Context } from "@earendil-works/chord/context";
import { AgentDoc, LiveDoc, configure, defineDoc, defineDocFamily, defineExtension, defineTask,
  type Conversation, type ConversationId, type EntryId, type Harness, type TaskId, type Tx } from "@earendil-works/pi-durable";
import { checkpointKind, checkpointLimit, effectKind, effectLimit, importedLabelKind, type CheckpointItem,
  type CheckpointRequest, type CheckpointsHost, type EffectNote, type RewindPreview, type RewindResult } from "../../server/src/checkpoints.js";
import { object, WireError } from "../../server/src/wire.js";
import { RewindGit, type GitSnapshot, type RewindStage } from "./rewind-git.js";
import { ThreadPresentationDoc, threadLabel } from "./thread-presentation.js";
import type { WorkspaceTrust } from "./workspace-trust.js";

type SavedCheckpoint = CheckpointItem & { id: string; snapshot?: GitSnapshot };
const Checkpoints = defineDoc<{ items: SavedCheckpoint[] }>({
  kind: "hush.checkpoints", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ items: [] }), checkpointWhen: () => true,
});
const Effects = defineDoc<{ items: EffectNote[] }>({
  kind: "hush.known-effects", version: 1, scope: "conversation", history: "rewindable", fork: "asOf",
  initial: () => ({ items: [] }), checkpointWhen: () => true,
});
type RestoreInputs = { workspace: string; snapshot: GitSnapshot; stage: RewindStage };
type Preview = { view: RewindPreview; expiresMs: number; restore?: RestoreInputs };
type RewindState = { result?: RewindResult; restore?: RestoreInputs; effects?: EffectNote[]; taskId?: number };
const Rewind = defineDocFamily<RewindState, null>({
  kind: "hush.rewind", version: 1, family: true, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});
const maxPreviews = 256;
const previewLifetime = 10 * 60_000;
const ownerless = { kind: "ownerless" } as const;
const thread = (v: string) => Number(v) as ConversationId;
const entry = (v: string) => Number(v) as EntryId;
const phase = { phase: "stage" } as const;
const ended = { status: "terminal", outcome: { status: "completed", result: null } } as const;
const notFound = () => new WireError("not_found");
const copy = <T>(v: T): T => JSON.parse(JSON.stringify(v)) as T;
const error = (message: string) => { const e = new WireError("invalid_request"); e.message = message; return e; };
const expired = () => error("Rewind preview is absent or expired. Preview again before confirming.");
const changed = () => error("The checkout or recorded effects changed. Preview the rewind again.");
const known = (items: readonly EffectNote[], at: string) => items.filter(item => entry(item.entryId) > entry(at));
const sameEffects = (a: readonly EffectNote[], b: readonly EffectNote[]) =>
  a.length === b.length && a.every((x, i) => x.entryId === b[i]!.entryId && x.text === b[i]!.text);
const detail = (e: unknown) => e instanceof WireError ? e.message : "No verified file restore is available.";
const transcriptOption = "A transcript-only rewind does not change any files.";
const marker = (kind: string, text: string) => ({ kind, data: { text, timestampMs: Date.now() } });

async function visible(tx: Tx, source: ConversationId, at: string) {
  const value = entry(at);
  return (await tx.scanEntries({ conversationId: source, minEntryId: value, maxEntryId: value }, 1)).items[0]?.id === value;
}

export class CheckpointService implements CheckpointsHost {
  readonly extension;
  readonly #task;
  readonly #previews = new Map<string, Preview>();

  constructor(private readonly harness: () => Harness, private readonly dataDir: string, private readonly trust: WorkspaceTrust) {
    this.#task = defineTask<{ threadId: string; id: string }, typeof phase, null>({
      name: "hush.rewind-stage", version: 1, initial: () => phase,
      phases: { stage: async (task, runtime, ctx) => {
        const owner = thread(task.input.threadId);
        const record = await this.harness().snapshot(Rewind, owner, task.input.id, ctx);
        if (record?.result?.state !== "staging" || !record.restore || !record.effects) {
          return runtime.commit(() => ended, ctx);
        }
        const { workspace, snapshot, stage } = record.restore;
        const signal = runtime.signal;
        let failure: string | undefined;
        try {
          const git = this.#git();
          await git.stage(workspace, snapshot, task.input.id, stage, signal);
          await git.verify(workspace, snapshot, task.input.id, stage, signal);
        } catch (e) {
          signal.throwIfAborted(); // a close/crash leaves staged work for recovery
          failure = detail(e);
        }
        const finish = () => runtime.commit(async tx => {
          const saved = await tx.doc(Rewind, owner, task.input.id, null);
          const result = saved.result;
          if (!result || result.state !== "staging") return ended;
          if (!failure) {
            const source = await tx.conversation(owner);
            const cwd = (await tx.doc(AgentDoc, owner)).cwd;
            const notes = known((await tx.doc(Effects, owner)).items, result.atEntryId);
            if (!source || source.owner || cwd !== workspace || !(await visible(tx, owner, result.atEntryId)) ||
                !sameEffects(saved.effects ?? [], notes)) failure = changed().message;
          }
          if (failure) {
            result.state = "failed";
            result.reason = failure + " " + transcriptOption;
            return ended;
          }
          const fork = await tx.forkConversation(owner, entry(result.atEntryId), { ownership: ownerless });
          await configure(tx, fork.id, { cwd: stage.cwd });
          (await tx.doc(ThreadPresentationDoc, fork.id)).checkout = {
            cwd: stage.cwd, branch: stage.branch, repository: stage.repository,
          };
          result.state = "ready";
          result.forkThreadId = String(fork.id);
          return ended;
        }, ctx);
        if (failure) return finish();
        try {
          // Bind the publication to one more authoritative grant check. The
          // fork and its owned checkout metadata appear in one Durable commit.
          return await this.trust.withGit(workspace, signal, finish);
        } catch (e) {
          signal.throwIfAborted();
          if (!(e instanceof WireError)) throw e;
          failure = detail(e);
          return finish();
        }
      } },
      abort: (task, runtime, ctx) => runtime.commit(async tx => {
        const r = (await tx.doc(Rewind, thread(task.input.threadId), task.input.id, null)).result;
        if (r?.state === "staging") {
          r.state = "failed";
          r.reason = "Rewind staging was interrupted. " + transcriptOption;
        }
        return { status: "terminal", outcome: { status: "aborted" } };
      }, ctx),
    });
    this.extension = defineExtension({ name: "hush.checkpoint-staging", tasks: [this.#task] });
  }

  #git() { return new RewindGit(realpathSync(this.dataDir), this.trust); }

  async latestFileSnapshot(id: ConversationId, ctx: Context) {
    const items = (await this.harness().snapshot(Checkpoints, id, ctx))?.items;
    return items?.find(item => item.snapshot)?.snapshot;
  }

  async comparisonCommit(workspace: string, snapshot: GitSnapshot, signal: AbortSignal) {
    return this.#git().comparisonCommit(workspace, snapshot, signal);
  }

  async call(q: CheckpointRequest, signal: AbortSignal) {
    const h = this.harness(), ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const source = await h.conversation(thread(q.threadId), ctx);
    if (!source || (await h.commit(tx => tx.conversation(source.id), ctx))?.owner) throw notFound();
    switch (q.op) {
      case "list": {
        const native = await h.snapshot(Checkpoints, source.id, ctx);
        const items = (native?.items ?? []).map(({ snapshot: _, ...item }) => item);
        // Labels from imported history are bookmarks only. A payload looking
        // like a file snapshot here is not a Hush-owned grant or checkpoint.
        const page = await source.entries({}, checkpointLimit, undefined, ctx);
        const imported = page.items.flatMap<CheckpointItem>(e => e.kind === importedLabelKind ? [{
          atEntryId: String(e.id), kind: "imported_label",
          label: object(e.data) && typeof e.data.label === "string" ? threadLabel(e.data.label, 64) || "Pi label" : "Pi label",
        }] : []);
        return { items: [...items, ...imported].sort((a, b) => entry(b.atEntryId) - entry(a.atEntryId)).slice(0, checkpointLimit) };
      }
      case "effect": return source.commit(async tx => {
        const doc = await tx.doc(Effects, source.id);
        if (doc.items.length >= effectLimit) throw new WireError("limit");
        const e = await tx.appendEntry(source.id, marker(effectKind, "Known external effect · " + q.text));
        doc.items.push({ entryId: String(e.id), text: q.text });
        return { entryId: String(e.id) };
      }, ctx);
      case "record": return this.#record(source, q, signal, ctx);
      case "preview": return this.#preview(source, q, signal, ctx);
      case "confirm": return this.#confirm(source, q, ctx);
      case "status": return this.#status(source, q.id, ctx);
    }
  }

  async #status(source: Conversation, key: string, ctx: Context) {
    const doc = await this.harness().snapshot(Rewind, source.id, key, ctx);
    if (!doc?.result) throw notFound();
    if (doc.result.state !== "staging" || doc.taskId === undefined) return doc.result;
    // Durable may terminate a repeatedly interrupted task without running
    // its abort handler. Resolve a stuck receipt without allocating again.
    return source.commit(async tx => {
      const current = await tx.doc(Rewind, source.id, key, null);
      const result = current.result;
      if (!result) throw notFound();
      if (result.state === "staging") {
        const task = current.taskId === undefined ? undefined : await tx.task(current.taskId as TaskId);
        if (!task || task.state.status === "terminal") {
          result.state = "failed";
          result.reason = "Rewind staging did not complete. " + transcriptOption;
        }
      }
      return copy(result);
    }, ctx);
  }

  async #record(source: Conversation, q: Extract<CheckpointRequest, { op: "record" }>, signal: AbortSignal, ctx: Context) {
    const id = randomUUID();
    const initial = await source.commit(async tx => {
      const latest = await tx.latestEntryId(source.id);
      const atEntryId = q.atEntryId ?? (latest === undefined ? "" : String(latest));
      if (!atEntryId || !await visible(tx, source.id, atEntryId)) throw notFound();
      const doc = await tx.doc(Checkpoints, source.id);
      if (doc.items.length >= checkpointLimit) throw new WireError("limit");
      const cwd = (await tx.doc(AgentDoc, source.id)).cwd;
      if (q.kind === "files" && (latest !== entry(atEntryId) || (await tx.doc(LiveDoc, source.id)).run || !cwd)) {
        throw error("File checkpoints require an idle thread at its latest committed entry.");
      }
      return { atEntryId, cwd };
    }, ctx);
    const snapshot = q.kind === "files" ? await this.#git().capture(initial.cwd!, id, signal) : undefined;
    const label = q.label || (snapshot ? "Committed HEAD" : "Bookmark");
    const row = { id, atEntryId: initial.atEntryId, kind: q.kind, label } as const;
    const persist = () => source.commit(async tx => {
      const doc = await tx.doc(Checkpoints, source.id);
      if (doc.items.length >= checkpointLimit) throw new WireError("limit");
      if (snapshot && ((await tx.latestEntryId(source.id)) !== entry(row.atEntryId) ||
          (await tx.doc(AgentDoc, source.id)).cwd !== initial.cwd || (await tx.doc(LiveDoc, source.id)).run)) throw changed();
      doc.items.unshift({ ...row, ...(snapshot && { snapshot }) });
      await tx.appendEntry(source.id, marker(checkpointKind,
        `◇ ${snapshot ? "File checkpoint" : "Bookmark"} · ${label} · entry #${row.atEntryId}`));
      return row;
    }, ctx);
    return snapshot ? this.trust.withGit(initial.cwd!, signal, persist) : persist();
  }

  async #preview(source: Conversation, q: Extract<CheckpointRequest, { op: "preview" }>, signal: AbortSignal, ctx: Context) {
    const seed = await source.commit(async tx => {
      const checkpoint = "checkpointId" in q ? (await tx.doc(Checkpoints, source.id)).items.find(i => i.id === q.checkpointId) : undefined;
      const at = "atEntryId" in q ? q.atEntryId : checkpoint?.atEntryId;
      if (!at || !await visible(tx, source.id, at)) throw notFound();
      return { at, snapshot: checkpoint?.snapshot ? copy(checkpoint.snapshot) : undefined,
        cwd: (await tx.doc(AgentDoc, source.id)).cwd, effects: copy(known((await tx.doc(Effects, source.id)).items, at)) };
    }, ctx);
    const id = randomUUID();
    const view: RewindPreview = { id, threadId: q.threadId, atEntryId: seed.at, mode: "transcript", effects: seed.effects };
    let restore: RestoreInputs | undefined;
    if (q.mode === "files") {
      if (seed.snapshot && seed.cwd) {
        try {
          const stage = await this.#git().prepare(seed.cwd, seed.snapshot, id, signal);
          view.mode = "files";
          view.files = { ...stage, commit: seed.snapshot.commit, ref: seed.snapshot.ref };
          restore = { workspace: seed.cwd, snapshot: seed.snapshot, stage };
        } catch (e) {
          signal.throwIfAborted();
          view.reason = detail(e);
        }
      } else view.reason = "No verified file snapshot was recorded at this bookmark.";
    }
    const now = Date.now();
    for (const [key, p] of this.#previews) if (p.expiresMs < now) this.#previews.delete(key);
    if (this.#previews.size >= maxPreviews) this.#previews.delete(this.#previews.keys().next().value!);
    this.#previews.set(id, { view, expiresMs: now + previewLifetime, restore });
    return view;
  }

  async #confirm(source: Conversation, q: Extract<CheckpointRequest, { op: "confirm" }>, ctx: Context) {
    const preview = this.#previews.get(q.id);
    const result = await source.commit(async tx => {
      const existing = await tx.doc(Rewind, source.id, q.id, null);
      if (existing.result) {
        if (existing.result.mode !== q.mode) throw expired();
        return copy(existing.result);
      }
      if (!preview || preview.expiresMs < Date.now() || preview.view.threadId !== q.threadId || preview.view.mode !== q.mode) throw expired();
      const { atEntryId, effects } = preview.view;
      if (!await visible(tx, source.id, atEntryId) ||
          !sameEffects(effects, known((await tx.doc(Effects, source.id)).items, atEntryId))) throw changed();
      const r: RewindResult = { id: q.id, threadId: q.threadId, atEntryId, mode: q.mode, state: "staging" };
      if (preview.restore) {
        if ((await tx.doc(AgentDoc, source.id)).cwd !== preview.restore.workspace) throw changed();
        existing.effects = copy(effects);
        existing.restore = preview.restore;
        existing.result = r;
        existing.taskId = await tx.createTask(this.#task, { threadId: q.threadId, id: q.id },
          { ownership: { kind: "conversation" }, background: true });
      } else {
        const fork = await tx.forkConversation(source.id, entry(atEntryId), { ownership: ownerless });
        r.state = "ready";
        r.forkThreadId = String(fork.id);
        existing.result = r;
      }
      return r;
    }, ctx);
    this.#previews.delete(q.id);
    if (result.state === "staging") this.harness().resume();
    return result;
  }
}
