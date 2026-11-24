// Ordered lifecycle adapter for manual and profile-owned compaction.
import assert from "node:assert/strict";
import type { Context } from "@earendil-works/chord/context";
import { retryDelayMs } from "@earendil-works/pi-ai/utils/retry";
import {
  CompactionEntry, CompactionTask, DEFAULT_RETRY_POLICY, LiveDoc,
  type CommitPublication, type Conversation, type ConversationRetryPolicy, type EntryId, type Harness, type TaskId,
} from "@earendil-works/pi-durable";
import { observeCompactionSelections, resolveCompaction, type CompactionId, type CompactionOutcome, type Selection } from "./compaction-result.mts";
import { TraceJournal, type TraceSink } from "./ordered-run.mts";

export type AutoCompactionReason = "threshold" | "overflow";
export type SaveCompaction = (result: Extract<CompactionOutcome, { kind: "applied" }>, ctx: Context) => Promise<void>;
type Series = {
  reason: "manual" | AutoCompactionReason;
  profileOwned: boolean;
  owner?: TaskId;
  started: boolean;
  scheduled: number;
  resumed: number;
  terminal: boolean;
};

export class OrderedCompaction {
  readonly journal: TraceSink;
  readonly #detach: () => void;
  readonly #save?: SaveCompaction;
  readonly #harness: Harness;
  readonly #conv: Conversation;
  readonly #policy: ConversationRetryPolicy;
  #selections = new Map<CompactionId, Selection>();
  #active = new Map<CompactionId, Series>();
  #placed = new Set<EntryId>();
  #profileReservation?: AutoCompactionReason;

  constructor(harness: Harness, conversation: Conversation, journal: TraceSink = new TraceJournal(),
    retry: Partial<ConversationRetryPolicy> = { baseDelayMs: 1, maxRetries: 1 }, save?: SaveCompaction) {
    this.#save = save;
    this.#harness = harness;
    this.#conv = conversation;
    this.#policy = { ...DEFAULT_RETRY_POLICY, ...retry };
    this.journal = journal;
    const selections = observeCompactionSelections(harness, (id, selection) => {
      if (selection.conversationId === conversation.id && !journal.failure) this.#selections.set(id, selection);
    });
    const commits = harness.observeCommits({
      advance: (batch) => {
        if (journal.failure) return;
        try { this.#commit(batch); }
        catch (error) { journal.fail(error instanceof Error ? error.message : String(error)); }
      },
      end: () => this.journal.fail("source ended"),
    });
    if (journal instanceof TraceJournal && !journal.ready) journal.arm();
    this.#detach = () => { selections.detach(); commits.detach(); };
  }

  close() { this.#detach(); }
  get isCompacting() { return this.#active.size > 0; }

  #commit(batch: CommitPublication) {
    const thread = this.#conv.id;
    const live = batch.changes.find(c => c.type === "document" && c.kind === "change" &&
      c.conversationId === thread && c.record.kind === LiveDoc.definition.kind);
    const compactions = live?.type === "document" && live.kind === "change" ? live.value.compactions : undefined;
    for (const c of batch.changes) {
      if (c.type === "entry" && c.value.conversationId === thread && CompactionEntry.is(c.value)) {
        this.#placed.add(c.value.id);
      }
    }
    for (const c of batch.changes) {
      if (c.type !== "task" || c.value.conversationId !== thread || c.value.kind !== CompactionTask.definition.name) continue;
      const { input, state, owner } = c.value, id = c.value.id as CompactionId;
      assert(input && typeof input === "object" && !Array.isArray(input) &&
        (input.reason === "manual" || input.reason === "overflow" && owner !== undefined),
        "fixture requires a manual or generation-owned overflow compaction");
      let series = this.#active.get(id);
      if (!series) {
        assert(state.status === "pending" && this.#active.size === 0, "compaction missed its creation fence or overlaps another");
        const profileOwned = this.#profileReservation !== undefined;
        const reason = this.#profileReservation ?? input.reason;
        assert(!profileOwned || input.reason === "manual" && owner === undefined && !c.value.background,
          "profile reservation did not admit an ordinary Conversation.compact task");
        this.#profileReservation = undefined;
        series = { reason, profileOwned, owner, started: !profileOwned, scheduled: 0, resumed: 0, terminal: false };
        this.#active.set(id, series);
        if (series.started) this.journal.push({ type: "compaction_start", reason });
      }
      if (state.status === "terminal") {
        series.terminal = true;
        if (series.scheduled) this.journal.push({ type: "summarization_retry_finished" });
        continue;
      }
      if (state.status !== "sleeping" && state.status !== "running") continue;
      const point = state.checkpoint;
      if (!point || typeof point !== "object" || Array.isArray(point) || typeof point.attempt !== "number") continue;
      if (state.status === "sleeping" && point.phase === "retry" && point.attempt > series.scheduled) {
        assert(series.started, "retry before compaction selection");
        const attempt = point.attempt;
        assert(attempt === series.scheduled + 1 && Array.isArray(compactions), "missing compactor retry decision");
        const status = compactions.find(item => item && typeof item === "object" && !Array.isArray(item) && item.taskId === id);
        assert(status && typeof status === "object" && !Array.isArray(status) && status.retry &&
          typeof status.retry === "object" && !Array.isArray(status.retry) && typeof status.retry.error === "string",
        "retry commit has no owned error evidence");
        series.scheduled = attempt;
        this.journal.push({ type: "summarization_retry_scheduled", attempt, maxAttempts: this.#policy.maxRetries,
          delayMs: retryDelayMs(this.#policy, attempt), errorMessage: status.retry.error });
      }
      if (state.status === "running" && point.phase === "summarize" && point.attempt > series.resumed + 1) {
        assert(point.attempt === series.scheduled + 1, "unobserved compaction backoff");
        series.resumed = point.attempt - 1;
        this.journal.push({ type: "summarization_retry_attempt_start", source: "compaction", reason: series.reason });
      }
    }
  }

  async end(id: CompactionId, ctx: Context) {
    assert.equal(this.#active.get(id)?.reason, "manual", "manual completion required");
    return this.#end(id, ctx);
  }

  // The exclusive profile host owns this trigger. The persisted native task/entry still say
  // manual; the Pi reason describes the host's decision, not a change to Durable metadata.
  async admitProfile(reason: AutoCompactionReason, ctx: Context): Promise<CompactionId> {
    assert(!this.journal.failure && !this.#profileReservation && this.#active.size === 0, "compactor is not ready");
    this.#profileReservation = reason;
    try {
      const id = await this.#conv.compact(undefined, ctx);
      assert.equal(this.#active.get(id)?.reason, reason, "no observed admission for this profile compaction");
      return id;
    } finally {
      this.#profileReservation = undefined;
    }
  }

  // Pinned Pi emits compaction_start after a cut exists. A terminal task with no
  // cut, or aborted before selection, shouldn't produce either compaction event.
  beforeCompact(id: TaskId) {
    if (this.journal.failure) return;
    const series = this.#active.get(id as CompactionId);
    if (!series?.profileOwned || series.started) return;
    series.started = true;
    this.journal.push({ type: "compaction_start", reason: series.reason });
  }

  async endProfile(id: CompactionId, ctx: Context, options: {
    verifyPlacement?: (entry: EntryId) => Promise<void>;
    retryProfileOverflow?: boolean;
  } = {}) {
    assert(this.#active.get(id)?.profileOwned, "profile compaction completion required");
    return this.#end(id, ctx, options);
  }

  // A beforeRequest hook is off the Session line. The successor already committed
  // its request checkpoint, but the provider hasn't run. Resolve the exact owned
  // placement now so compaction_end precedes the continued agent_start.
  async beforeGenerationRequest(owner: TaskId, ctx: Context): Promise<CompactionId | undefined> {
    const match = [...this.#active].find(([, series]) => series.owner === owner && series.reason === "overflow");
    if (!match || this.journal.failure) return;
    const [id, series] = match;
    try {
      const task = await this.#harness.getTask(owner, ctx);
      const point = task?.state.status === "running" ? task.state.checkpoint : undefined;
      assert(series.terminal && point && typeof point === "object" && !Array.isArray(point) &&
        point.phase === "request" && point.compacted === id, "request did not resume from the observed compactor");
      const outcome = await this.#end(id, ctx, { reentered: true });
      if (outcome.kind === "applied" && !this.journal.failure) return id;
    } catch (error) {
      this.journal.fail(error instanceof Error ? error.message : String(error));
    }
  }

  async #end(id: CompactionId, ctx: Context, options: {
    reentered?: boolean;
    retryProfileOverflow?: boolean;
    verifyPlacement?: (entry: EntryId) => Promise<void>;
  } = {}) {
    const series = this.#active.get(id);
    assert(series, "no observed compaction admission");
    const result = await resolveCompaction(this.#harness, this.#conv, id, key => this.#selections.get(key), ctx,
      series.profileOwned ? "committed" : "wait");
    if (this.journal.failure) return result;
    assert(series.terminal, "no observed compaction terminal boundary");
    if (options.reentered && result.kind !== "applied") {
      this.journal.fail("overflow resumed without a verified summary placement");
      return result;
    }
    this.#active.delete(id);
    this.#selections.delete(id);
    if (!series.started) {
      if (result.kind !== "aborted" && !(result.kind === "not_applied" && result.cause === "no_summary")) {
        this.journal.fail("profile compaction ended without observed selection");
      }
      return result;
    }
    if (result.kind === "unavailable" || result.kind === "not_applied" && result.cause === "stale") {
      this.journal.fail(result.kind === "not_applied" ? "compaction write was superseded" : result.message);
      return result;
    }
    if (result.kind === "applied") {
      assert(this.#placed.delete(result.entryId), "summary placement was not observed");
      await options.verifyPlacement?.(result.entryId);
      await this.#save?.(result, ctx);
    }
    const reason = series.reason;
    const willRetry = options.reentered === true ||
      (options.retryProfileOverflow === true && reason === "overflow" && result.kind === "applied");
    const failurePrefix = reason === "threshold" ? "Auto-compaction failed: " :
      reason === "overflow" && series.profileOwned ? "Context overflow recovery failed: " : "";
    this.journal.push({ type: "compaction_end", reason, aborted: result.kind === "aborted", willRetry,
      result: result.kind === "applied" ? result.result : undefined,
      ...(result.kind === "failed" ? { errorMessage: failurePrefix + result.message } :
        result.kind === "not_applied" ? { errorMessage: "Nothing to compact (session too small)" } : {}) });
    return result;
  }
}
