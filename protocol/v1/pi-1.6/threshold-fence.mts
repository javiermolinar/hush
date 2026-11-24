// The profile exclusively owns input and abort admission for this conversation;
// the Harness has autonomous compaction disabled.
import assert from "node:assert/strict";
import type { Context } from "@earendil-works/chord/context";
import { isContextOverflow, isRecoverableLength } from "@earendil-works/pi-ai/utils/overflow";
import { shouldCompact } from "@earendil-works/pi-coding-agent";
import { ConversationBusy, InboxDoc, type Conversation, type ConversationRetryPolicy, type EntryId, type Harness, type TaskId } from "@earendil-works/pi-durable";
import { estimateCompactionContext, type CompactionId, type CompactionOutcome } from "./compaction-result.mts";
import { LocalQueue, QueueDelivery, type Admission, type Input, type Mode } from "./local-queue.mts";
import { OrderedCompaction, type AutoCompactionReason, type SaveCompaction } from "./ordered-compaction.mts";
import { OrderedRun, type RunDrain } from "./ordered-run.mts";

type Started = Extract<Admission, { disposition: "started" }> & { settled: Promise<void> };
type Queued = Extract<Admission, { disposition: "queued" }>;
type Active = { stopping?: Promise<void>; settled: Promise<void>; recovered: boolean };
export type Policy = Parameters<typeof shouldCompact>[2] & { provider: string; modelId: string; contextWindow: number; maxTokens?: number };

export class PromptCancelled extends Error {
  constructor() { super("Prompt cancelled before admission"); }
}

export class CompactionFence {
  readonly #harness: Harness;
  readonly #conv: Conversation;
  readonly #run: OrderedRun;
  readonly #compactions: OrderedCompaction;
  readonly #queue: LocalQueue;
  #policy: Policy;
  #active?: Active;
  #manual?: Promise<CompactionOutcome>;
  #between?: { task: CompactionId; predecessor: TaskId };
  #commands = Promise.resolve();

  constructor(harness: Harness, conv: Conversation, run: OrderedRun, policy: Policy,
    retry?: Partial<ConversationRetryPolicy>, save?: SaveCompaction) {
    this.#harness = harness;
    this.#conv = conv;
    this.#run = run;
    this.#compactions = new OrderedCompaction(harness, conv, run.journal, retry, save);
    this.#queue = new LocalQueue(harness, conv);
    this.#policy = policy;
  }

  get busy() { return this.#active !== undefined || this.#manual !== undefined; }
  get isCompacting() { return this.#compactions.isCompacting; }

  setPolicy(policy: Policy) {
    if (this.busy) throw new ConversationBusy(this.#conv.id);
    this.#policy = policy;
  }

  async dispose(ctx: Context) {
    await Promise.allSettled([this.#active?.settled, this.#manual]);
    // A lost producer isn't a cancellation of native work.
    try { await this.#conv.waitForIdle(ctx); }
    finally { this.#compactions.close(); }
  }

  async compact(instructions: string | undefined, ctx: Context) {
    if (this.#manual) throw new ConversationBusy(this.#conv.id);
    if (this.#active) await this.abort(ctx);
    return this.#serial(async () => {
      this.#requireLive();
      if (this.busy) throw new ConversationBusy(this.#conv.id);
      this.#run.reserveProfileCompaction("prePrompt");
      try {
        const id = await this.#conv.compact(instructions, ctx);
        const done = this.#compactions.end(id, ctx).then(result => {
          this.#requireLive();
          this.#run.releaseProfileCompaction(id);
          return result;
        }).catch(error => { this.#fail(error); throw error; }).finally(() => { this.#manual = undefined; });
        this.#manual = done;
        void done.catch(() => {});
        return { done };
      } catch (error) { this.#fail(error); throw error; }
    });
  }

  beforeCompact(task: TaskId) { this.#compactions.beforeCompact(task); }

  async prompt(input: Input, whenBusy: Mode | undefined, ctx: Context): Promise<Started | Queued> {
    const stage = await this.beginPrompt(input, whenBusy, ctx);
    return "started" in stage ? stage.started : stage;
  }

  // Register preflight in command order, but don't hold the peer's admission
  // lane waiting for a pre-prompt compaction: an abort can still reach it.
  beginPrompt(input: Input, whenBusy: Mode | undefined, ctx: Context): Promise<{ started: Promise<Started> } | Queued> {
    return this.#serial(async () => {
      this.#requireLive();
      if (this.#manual) throw new ConversationBusy(this.#conv.id);
      if (this.#active) {
        if (!whenBusy) throw new ConversationBusy(this.#conv.id);
        return this.#queue.enqueue(whenBusy, input, ctx);
      }
      const admission = Promise.withResolvers<Started>();
      const active: Active = { recovered: false, settled: Promise.resolve() };
      this.#active = active;
      active.settled = this.#drive(active, input, ctx, admission);
      void admission.promise.catch(() => {});
      void active.settled.catch(() => {});
      return { started: admission.promise };
    });
  }

  async abort(ctx: Context) {
    const active = await this.#serial(async () => {
      const active = this.#active;
      // Mark while holding the command lane, but don't join within it: a generation
      // hook or the drain coordinator may already be waiting on this same lane.
      if (active) active.stopping ??= this.#queue.abort(ctx);
      return { active, manual: this.#manual, stopManual: this.#manual ? this.#conv.abort(ctx) : undefined };
    });
    await active.active?.stopping;
    await active.active?.settled;
    await active.stopManual;
    await active.manual;
  }

  async afterTools(predecessor: TaskId, assistant: EntryId, results: readonly EntryId[], ctx: Context) {
    if (this.#active) this.#active.recovered = false; // The committed tool-use answer ended that recovery series.
    await this.#atTurnBoundary(predecessor, results.reduce((last, id) => id > last ? id : last, assistant), ctx);
  }

  async afterAnswer(predecessor: TaskId, answer: EntryId, ctx: Context) {
    if (!this.#policy.enabled || !this.#active || this.#active.stopping || this.#run.journal.failure) return;
    // The local queue's onYield hook has already handed this input to Durable. An
    // empty inbox means the run will first close: use the post-answer fence instead.
    const inbox = await this.#harness.snapshot(InboxDoc, this.#conv.id, ctx);
    if (inbox?.items.some(item => item.mode !== "write" && item.entry?.kind === QueueDelivery)) {
      await this.#atTurnBoundary(predecessor, answer, ctx);
    }
  }

  async beforeRequest(successor: TaskId, ctx: Context) {
    const pending = this.#between;
    if (!pending) return;
    try {
      const [previous, next] = await Promise.all([
        this.#harness.getTask(pending.predecessor, ctx), this.#harness.getTask(successor, ctx),
      ]);
      const point = next?.state.checkpoint;
      assert(previous?.state.status === "terminal" && successor !== pending.predecessor && next?.state.status === "running" &&
        point && typeof point === "object" && !Array.isArray(point) && point.phase === "request" &&
        typeof point.cutoff === "number", "compaction has no observed successor request");
      const at = point.cutoff as EntryId;
      await this.#end(pending.task, ctx, { verifyPlacement: async entry => {
        assert.equal((await this.#conv.context(ctx, { at })).head?.id, entry,
          "successor was prepared without the exact summary placement");
      } });
      this.#between = undefined;
    } catch (error) { this.#fail(error); }
  }

  async #atTurnBoundary(predecessor: TaskId, at: EntryId, ctx: Context) {
    if (!this.#policy.enabled || !this.#active || this.#active.stopping || this.#run.journal.failure) return;
    try {
      if (!await this.#aboveThreshold(ctx, at)) return;
      const id = await this.#serial(async () => {
        if (this.#active?.stopping) return;
        assert(!this.#between, "another between-turn compaction is pending");
        const task = await this.#admit("threshold", "betweenTurns", ctx);
        this.#between = { task, predecessor };
        return task;
      });
      // The write may be QUEUED while generation is inside this hook. Waiting for
      // its placement here would deadlock; let Durable apply its next boundary first.
      if (id !== undefined) await this.#harness.waitForTask(id, ctx);
    } catch (error) {
      if (!this.#active?.stopping) this.#fail(error);
    }
  }

  #serial<T>(fn: () => Promise<T>): Promise<T> {
    const result = this.#commands.then(fn);
    this.#commands = result.then(() => {}, () => {});
    return result;
  }

  #requireLive() {
    if (this.#run.journal.failure) throw new Error(`profile trace lost: ${this.#run.journal.failure}`);
  }

  #fail(error: unknown) {
    this.#run.journal.fail(error instanceof Error ? error.message : String(error));
  }

  async #aboveThreshold(ctx: Context, at?: EntryId): Promise<boolean> {
    const view = await this.#conv.context(ctx, at === undefined ? undefined : { at });
    if (at === undefined) {
      const newest = view.entries.findLast(e => e.model?.some(m => m.role === "assistant"));
      if (!newest || (view.head && newest.id <= view.head.id)) return false;
    }
    const tokens = estimateCompactionContext(view);
    assert(tokens !== undefined && this.#policy.contextWindow > 0, "no anchored compaction estimate");
    return shouldCompact(tokens, this.#policy.contextWindow, this.#policy);
  }

  async #reason(drain: RunDrain, ctx: Context): Promise<AutoCompactionReason | undefined> {
    if (drain.aborted || !this.#policy.enabled) return;
    const { provider, modelId, contextWindow } = this.#policy;
    const answer = drain.answer;
    const message = answer?.message;
    assert(answer?.entry !== undefined && message?.provider === provider && message.model === modelId && contextWindow > 0,
      "automatic compaction needs a committed answer from the selected model");
    if (message.stopReason === "aborted") return;
    if (message.stopReason === "length") {
      assert(this.#policy.maxTokens !== undefined, "unproved stop reason for automatic compaction: no output limit");
      if (isRecoverableLength(message, this.#policy.maxTokens)) return "overflow";
    }
    if (isContextOverflow(message, contextWindow)) return "overflow";
    assert(message.stopReason === "stop" || message.stopReason === "error" || message.stopReason === "length",
      "unproved stop reason for automatic compaction");
    if (await this.#aboveThreshold(ctx, answer.entry)) return "threshold";
  }

  async #admit(reason: AutoCompactionReason, boundary: "prePrompt" | "betweenTurns" | RunDrain, ctx: Context) {
    this.#requireLive();
    this.#run.reserveProfileCompaction(boundary);
    return this.#compactions.admitProfile(reason, ctx);
  }

  async #end(task: CompactionId, ctx: Context, options?: Parameters<OrderedCompaction["endProfile"]>[2]) {
    const outcome = await this.#compactions.endProfile(task, ctx, options);
    this.#requireLive();
    this.#run.releaseProfileCompaction(task);
    return outcome;
  }

  async #drive(active: Active, input: Input, ctx: Context, admission: PromiseWithResolvers<Started>) {
    try {
      if (this.#policy.enabled && await this.#aboveThreshold(ctx)) {
        const task = await this.#serial(async () => active.stopping ? undefined : this.#admit("threshold", "prePrompt", ctx));
        if (task !== undefined) await this.#end(task, ctx);
      }
      await this.#serial(async () => {
        this.#requireLive();
        if (active.stopping) throw new PromptCancelled();
        const admitted = await this.#queue.prompt(input, undefined, ctx);
        assert(admitted.disposition === "started");
        admission.resolve({ ...admitted, settled: active.settled });
      });
      for (;;) {
        await this.#conv.waitForIdle(ctx);
        await active.stopping; // Includes reconciling an exactly withdrawn queue handoff.
        this.#requireLive();
        const drain = this.#run.drained;
        assert(drain, "ordinary idle has no observed Pi run boundary");
        if (this.#between) {
          assert(drain.aborted, "between-turn compaction has no successor evidence");
          await this.#end(this.#between.task, ctx);
          this.#between = undefined;
        }
        if (drain.answer?.message.stopReason === "stop") active.recovered = false;
        const reason = active.stopping ? undefined : await this.#reason(drain, ctx);
        const stopped = drain.answer?.message.stopReason;
        const wantsRetry = reason === "overflow" && (stopped === "error" || stopped === "length");
        const compact = await this.#serial(async () => {
          if (!reason || active.stopping) return;
          if (wantsRetry && active.recovered) {
            this.#run.journal.push({ type: "compaction_end", reason, result: undefined, aborted: false, willRetry: false,
              errorMessage: stopped === "length" ? "Truncated response recovery failed after one compact-and-retry attempt." :
                "Context overflow recovery failed after one compact-and-retry attempt. Try reducing context or switching to a larger-context model." });
            return;
          }
          if (wantsRetry && stopped === "length") {
            // Durable treats a length stop as a committed answer. Withdraw only
            // its contribution to context, never the receipt or transcript row.
            const target = drain.answer?.entry;
            assert(target !== undefined, "missing recoverable length entry");
            const edit = await this.#conv.submit({ type: "write", entry: { kind: "hush.pi-recovery-omit",
              edits: [{ action: "omit", target }] } }, ctx);
            const receipt = await edit.wait(ctx);
            assert(receipt.type === "write" && receipt.status === "done", "failed to place the length recovery edit");
            const { entries, contributions } = await this.#conv.context(ctx, { at: receipt.entry });
            assert(contributions[entries.findIndex(entry => entry.id === target)]?.length === 0, "length contribution was not omitted");
          }
          return this.#admit(reason, drain, ctx);
        });
        if (compact !== undefined) await this.#harness.waitForTask(compact, ctx);
        const continued = await this.#serial(async () => {
          // Generation and any compactor are already idle here, so joining a racing
          // abort cannot hold a lane needed by a running generation hook.
          await active.stopping;
          this.#requireLive();
          const aborted = Boolean(active.stopping) || drain.aborted;
          const retry = wantsRetry && !aborted;
          const outcome = compact === undefined ? undefined : await this.#end(compact, ctx, {
            retryProfileOverflow: retry,
            verifyPlacement: retry ? async entry => {
              const view = await this.#conv.context(ctx);
              const last = view.messages.findLast(m => m.role !== "system");
              assert(view.head?.id === entry && (last?.role === "toolResult" || (last?.role === "user" && last !== view.head.model?.[0])),
                "overflow placement has no input to continue");
            } : undefined,
          });
          if (retry && outcome?.kind === "applied") {
            this.#run.finishDrain(drain, "continue");
            active.recovered = true;
            // A fresh native continue receipt, never another input or an alteration to
            // the failed attempt's native receipt. Pi prompt responses report admission.
            await this.#conv.submit({ type: "continue" }, ctx);
            return true;
          }
          if (!aborted) {
            const pending = await this.#queue.inspect(ctx);
            if (pending.steering.length || pending.followUp.length) {
              this.#run.finishDrain(drain, "continue");
              active.recovered = false;
              assert(await this.#queue.resumeAfterDrain(ctx), "staged input disappeared before admission");
              return true;
            }
          }
          this.#run.finishDrain(drain, aborted ? "abort" : "settle");
          this.#active = undefined;
          return false;
        });
        if (!continued) return;
      }
    } catch (error) {
      if (!(error instanceof PromptCancelled)) this.#fail(error);
      admission.reject(error);
      await this.#serial(async () => {
        this.#active = undefined;
      });
      if (!(error instanceof PromptCancelled)) throw error;
    }
  }
}
