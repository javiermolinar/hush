// Ordered projection of public Durable producer sources. No reads from a folded watch.
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { type AssistantMessage, type AssistantMessageEvent, type Message, type ToolResultMessage } from "@earendil-works/pi-ai";
import { retryDelayMs } from "@earendil-works/pi-ai/utils/retry";
import type { JsonAgentSessionEvent } from "@earendil-works/pi-coding-agent";
import {
  AssistantEntry, CompactionTask, DEFAULT_RETRY_POLICY, GenerationTask, SystemEntry, ToolResultEntry, UserEntry,
  type CommitPublication, type Conversation, type ConversationId, type ConversationRetryPolicy, type EntryId,
  type Harness, type TaskId, type ToolExecutionResult, type ToolHookCall,
} from "@earendil-works/pi-durable";

import { PendingInputs, QueueDelivery, type Queues } from "./local-queue.mts";

type Wire = JsonAgentSessionEvent;
export type Frame = { epoch: string; seq: number; event: Wire };
export type Ready = { epoch: string; seq: 0 };

export interface TraceSink {
  readonly failure?: string;
  push(event: Wire): void;
  fail(reason: string): void;
}

// The fixture journal is an observation instrument, not a replay buffer.
export class TraceJournal implements TraceSink {
  readonly frames: Frame[] = [];
  readonly limit: number;
  ready?: Ready;
  failure?: string;

  constructor(limit = 1024) { this.limit = limit; }
  arm() {
    assert(!this.ready, "trace already armed");
    this.ready = { epoch: randomUUID(), seq: 0 };
  }
  fail(reason: string) { this.failure ??= reason; }
  push(event: Wire) {
    if (this.failure) return;
    if (!this.ready) return this.fail("not ready");
    if (this.frames.length >= this.limit) return this.fail("overflow");
    this.frames.push({ epoch: this.ready.epoch, seq: this.frames.length + 1, event: structuredClone(event) });
  }
}

// Feed only a live invocation's next frame. Refusal latches so a late/gapped record can't be retried.
export function acceptTrace(ready: Ready) {
  let next = ready.seq + 1, lost = false;
  return (frame: Frame) => {
    if (lost || frame.epoch !== ready.epoch || frame.seq !== next) {
      lost = true;
      throw new Error("ordered trace lost");
    }
    next++;
    return frame.event;
  };
}

type Answer = { entry?: EntryId; message: AssistantMessage };
type Turn = {
  task: TaskId;
  signal: AbortSignal;
  sawSource: boolean;
  sawStart: boolean;
  providerAbort?: AssistantMessage;
  assistant?: Answer;
};
export type RunDrain = Readonly<{ aborted: boolean; answer?: Answer }>;

/**
 * The fixture owns one new conversation and submits one prompt chain at a time. Match its provider tap
 * by invocation AbortSignal, not by the latest streaming text, shared provider session ID or time.
 * Atomic commits establish membership; order changed entries by immutable ID, never by changes[] position.
 */
export class OrderedRun {
  readonly journal: TraceSink;
  readonly #policy: ConversationRetryPolicy;
  readonly #ownedOverflow: boolean;
  readonly #hostSettlement: boolean;
  #overflow?: { task: TaskId; owner: TaskId };
  #drain?: RunDrain;
  #profileCompaction?: { id?: TaskId; terminal?: boolean };
  #thread?: ConversationId;
  #active = new Set<TaskId>();
  #abortRequested = false;
  #inputs: { message: Message; delivery: boolean }[] = [];
  // After handoff, Pi still presents an input as queued until the user message is delivered.
  #displayed: Queues = { steering: [], followUp: [] };
  #pending: Queues = this.#displayed;
  #transit: { mode: keyof Queues; text: string; receipt?: number }[] = [];
  #restored?: number;
  #attempt: Message[] | undefined;
  #turn: Turn | undefined;
  #toolCalls = new Map<TaskId, { call: ToolHookCall; rejected: boolean }>();
  #toolResults = new Map<EntryId, ToolResultMessage>();
  #retry = 0;
  #last?: Answer;
  #sawRun = false;

  constructor(journal: TraceSink = new TraceJournal(), options: {
    retry?: Partial<ConversationRetryPolicy>;
    ownedOverflow?: boolean;
    hostSettlement?: boolean;
    pending?: Queues;
    restored?: number;
  } = {}) {
    this.journal = journal;
    if (options.pending) this.#pending = this.#displayed = options.pending;
    this.#restored = options.restored;
    this.#policy = { ...DEFAULT_RETRY_POLICY, baseDelayMs: 1, maxRetries: 1, ...options.retry };
    this.#ownedOverflow = options.ownedOverflow ?? false;
    this.#hostSettlement = options.hostSettlement ?? false;
  }

  get drained(): RunDrain | undefined { return this.#drain; }
  get queues(): Readonly<Queues> { return this.#displayed; }

  reserveProfileCompaction(boundary: "prePrompt" | "betweenTurns" | RunDrain) {
    assert(this.#hostSettlement && !this.#profileCompaction && !this.#turn && !this.#overflow,
      "profile compaction has no open boundary");
    if (boundary === "prePrompt") {
      assert(!this.#sawRun && !this.#drain && !this.#active.size, "pre-prompt compaction requires idle");
    } else if (boundary === "betweenTurns") {
      assert(this.#attempt && this.#active.size && !this.#drain, "between-turn compaction requires an active run");
    } else {
      assert(boundary === this.#drain && !boundary.aborted && !this.#active.size, "post-run compaction requires a held drain");
    }
    this.#profileCompaction = {};
  }

  releaseProfileCompaction(id: TaskId) {
    assert(this.#profileCompaction?.id === id && this.#profileCompaction.terminal,
      "profile compaction has no terminal evidence");
    this.#profileCompaction = undefined;
  }

  // A late queued input or recovered overflow continues the outer Pi run without an intermediate settlement.
  finishDrain(drain: RunDrain, disposition: "settle" | "continue" | "abort") {
    assert(drain === this.#drain && !this.#active.size && !this.#profileCompaction && !this.#transit.length,
      "run drain has unresolved work or queue delivery");
    assert(disposition !== "continue" || !drain.aborted, "cannot continue an aborted run");
    this.#drain = undefined;
    if (disposition !== "continue") {
      this.journal.push({ type: "agent_settled", aborted: drain.aborted || disposition === "abort" });
    }
  }

  attach(harness: Harness, conv: Conversation) {
    assert(this.#thread === undefined, "run source only observes one conversation");
    this.#thread = conv.id;
    if (this.journal instanceof TraceJournal) this.journal.arm();
    return harness.observeCommits({
      advance: (batch) => this.#record(() => this.#commit(batch)),
      end: () => this.journal.fail("source ended"),
    });
  }

  beforeRequest(thread: ConversationId, task: TaskId, signal?: AbortSignal, recovered?: TaskId) {
    if (thread !== this.#thread) return;
    this.#record(() => {
      assert(signal && this.#active.has(task), "request has no observed generation invocation");
      assert(!this.#turn && !this.#drain && !this.#profileCompaction, "previous turn, compaction or drain did not close");
      if (this.#overflow) {
        assert(recovered === this.#overflow.task && task === this.#overflow.owner, "overflow placement fence was not verified");
        this.#overflow = undefined;
      } else {
        assert(recovered === undefined, "unexpected overflow placement fence");
      }
      if (!this.#attempt) {
        this.#attempt = [];
        this.#sawRun = true;
        this.journal.push({ type: "agent_start" });
      }
      this.#turn = { task, signal, sawSource: false, sawStart: false };
      this.journal.push({ type: "turn_start" });
      // Durable can commit its system entry after input placement; Pi emits prepared system
      // changes before the user messages at the request boundary. No transcript read here.
      const inputs = this.#inputs.splice(0);
      for (const { message, delivery } of [
        ...inputs.filter(row => row.message.role === "system"), ...inputs.filter(row => row.message.role !== "system"),
      ]) {
        if (delivery) {
          const item = this.#transit.shift();
          assert(item && message.role === "user", "queue delivery without a staging handoff");
          const text = typeof message.content === "string" ? message.content :
            message.content.flatMap(block => block.type === "text" ? [block.text] : []).join("");
          assert(text === item.text, "queue delivery payload does not match its handoff");
          this.#publishQueue();
        }
        this.#instantMessage(message);
      }
    });
  }

  // Clear is observable even when it makes no storage change. This call is evidence
  // from a successful empty clear transaction, not an inference from a state snapshot.
  confirmedEmptyClear(removed: Queues) {
    if (!removed.steering.length && !removed.followUp.length) this.#record(() => this.#publishQueue(true));
  }

  provider(signal: AbortSignal | undefined, event: AssistantMessageEvent) {
    const turn = this.#turn;
    if (!turn || signal !== turn.signal) return; // Includes compactor streams, which aren't assistant messages.
    this.#record(() => {
      turn.sawSource = true;
      if (event.type === "start") {
        assert(!turn.sawStart, "duplicate provider start");
        turn.sawStart = true;
        this.journal.push({ type: "message_start", message: event.partial });
      } else if (event.type === "error" && event.reason === "aborted") {
        turn.providerAbort = structuredClone(event.error);
      } else if (event.type !== "done" && event.type !== "error") {
        assert(turn.sawStart, "provider update before start");
        const { partial, ...delta } = event;
        if (delta.type === "toolcall_start") {
          const call = partial.content[delta.contentIndex];
          assert(call?.type === "toolCall", "missing toolcall_start identity");
          this.journal.push({ type: "message_update", usage: partial.usage,
            assistantMessageEvent: { ...delta, id: call.id, toolName: call.name } });
        } else {
          this.journal.push({ type: "message_update", usage: partial.usage, assistantMessageEvent: delta });
        }
      }
    });
  }

  #beginTool(task: TaskId, call: ToolHookCall, rejected: boolean) {
    assert(this.#turn?.assistant?.message.content.some(b => b.type === "toolCall" && b.id === call.id && b.name === call.name),
      "tool started without a matching committed assistant call");
    assert(!this.#toolCalls.has(task) && !call.parent, "tool has no fresh model-issued call");
    this.#toolCalls.set(task, { call: structuredClone(call), rejected });
    this.journal.push({ type: "tool_execution_start", toolCallId: call.id, toolName: call.name, args: call.arguments });
  }

  beforeTool(thread: ConversationId, task: TaskId, call: ToolHookCall) {
    if (thread === this.#thread) this.#record(() => this.#beginTool(task, call, false));
  }

  afterTool(thread: ConversationId, task: TaskId, call: ToolHookCall, result: ToolExecutionResult) {
    if (thread !== this.#thread || this.#toolCalls.has(task) || this.journal.failure) return;
    // Durable skips beforeTool when it rejects at lookup or argument validation.
    // This is the rejection hook, before commit: Pi announces these attempted calls
    // even though execute() never ran, and their committed result has no duration.
    this.#record(() => {
      assert(result.isError && result.diagnostics?.some(d => d.code === "tool_unavailable" || d.code === "invalid_arguments"),
        "tool result without an observed call or pre-execution rejection");
      this.#beginTool(task, call, true);
    });
  }

  hasExecutableTool(task: TaskId) { return this.#toolCalls.get(task)?.rejected === false; }

  toolOutput(thread: ConversationId, task: TaskId, text: string) {
    if (thread !== this.#thread) return;
    this.#record(() => {
      const running = this.#toolCalls.get(task);
      assert(running && !running.rejected, "tool output without an executable call");
      const { call } = running;
      this.journal.push({ type: "tool_execution_update", toolCallId: call.id, toolName: call.name,
        args: call.arguments, partialResult: { content: [{ type: "text", text }], details: {} } });
    });
  }

  afterTools(thread: ConversationId, task: TaskId, assistant: EntryId, resultIds: readonly EntryId[]) {
    if (thread !== this.#thread) return;
    this.#record(() => {
      assert(this.#turn?.task === task && this.#turn?.assistant?.entry === assistant, "wrong tool round identity");
      const toolResults = resultIds.map(id => {
        const result = this.#toolResults.get(id);
        assert(result, "tool round names an unobserved result");
        this.#toolResults.delete(id);
        this.#instantMessage(result);
        return result;
      });
      this.#endTurn(toolResults);
    });
  }

  #record(callback: () => void) {
    if (this.journal.failure) return;
    try { callback(); }
    catch (error) { this.journal.fail(error instanceof Error ? error.message : String(error)); }
  }

  #instantMessage(message: Message) {
    this.journal.push({ type: "message_start", message });
    this.#endMessage(message);
  }

  #endMessage(message: Message) {
    assert(this.#attempt, "message outside an attempt");
    this.journal.push({ type: "message_end", message });
    this.#attempt.push(message);
  }

  #endTurn(toolResults: ToolResultMessage[]) {
    assert(this.#turn?.assistant, "turn ended without authoritative assistant");
    this.#last = this.#turn.assistant;
    this.journal.push({ type: "turn_end", message: this.#last.message, toolResults });
    this.#turn = undefined;
  }

  #endAttempt(willRetry: boolean) {
    assert(this.#attempt && !this.#turn, "attempt boundary without a complete turn");
    this.journal.push({ type: "agent_end", messages: this.#attempt, willRetry });
    this.#attempt = undefined;
  }

  #publishQueue(force = false) {
    const visible = {
      steering: [...this.#transit.filter(item => item.mode === "steering").map(item => item.text), ...this.#pending.steering],
      followUp: [...this.#transit.filter(item => item.mode === "followUp").map(item => item.text), ...this.#pending.followUp],
    };
    if (force || JSON.stringify(this.#displayed) !== JSON.stringify(visible)) {
      this.#displayed = visible;
      this.journal.push({ type: "queue_update", ...visible });
    }
  }

  #commit(batch: CommitPublication) {
    const entries = batch.changes.flatMap(c => c.type === "entry" && c.value.conversationId === this.#thread ? [c.value] : [])
      .sort((a, b) => a.id - b.id);
    const tasks = batch.changes.flatMap(c => c.type === "task" && c.value.conversationId === this.#thread ? [c.value] : []);
    const compactions = tasks.filter(task => task.kind === CompactionTask.definition.name);
    for (const task of compactions) {
      const input = task.input;
      if (this.#profileCompaction && input && typeof input === "object" && !Array.isArray(input) && input.reason === "manual" &&
          task.owner === undefined && !task.background) {
        if (this.#profileCompaction.id === undefined && task.state.status === "pending") this.#profileCompaction.id = task.id;
        assert(this.#profileCompaction.id === task.id, "compaction is outside the host reservation");
        this.#profileCompaction.terminal = task.state.status === "terminal";
        continue;
      }
      assert(this.#ownedOverflow && input && typeof input === "object" && !Array.isArray(input) &&
        input.reason === "overflow" && task.owner !== undefined, "compaction is outside this run trace");
      if (task.state.status === "pending") {
        assert(!this.#overflow && this.#turn?.task === task.owner, "overflow task has no matching live turn");
        this.#overflow = { task: task.id, owner: task.owner };
      } else {
        assert(this.#overflow?.task === task.id, "compaction changed outside its recovery fence");
      }
    }
    const generation = tasks.filter(task => task.kind === GenerationTask.definition.name);
    for (const task of generation) {
      if (task.state.status === "terminal") this.#active.delete(task.id);
      else this.#active.add(task.id);
      this.#abortRequested ||= task.abortRequested;
    }
    for (const c of batch.changes) {
      if (c.type !== "document" || c.kind !== "change" || c.conversationId !== this.#thread ||
          c.record.kind !== PendingInputs.definition.kind) continue;
      const texts = (key: "steer" | "followUp") => {
        const items = c.value[key];
        assert(Array.isArray(items));
        return items.map(item => {
          assert(item && typeof item === "object" && !Array.isArray(item) && typeof item.text === "string");
          return item.text;
        });
      };
      const after = { steering: texts("steer"), followUp: texts("followUp") };
      const restored = c.value.restored;
      if (typeof restored === "number" && restored !== this.#restored) {
        const index = this.#transit.findIndex(item => item.receipt === restored);
        const item = this.#transit[index];
        assert(item && after[item.mode][0] === item.text, "queue restoration is not tied to the withdrawn handoff");
        this.#transit.splice(index, 1);
        this.#restored = restored;
      }
      const queued = batch.changes.find(change => change.type === "submission" && change.value.conversationId === this.#thread &&
        change.value.type === "input" && change.value.status === "queued");
      const receipt = queued?.type === "submission" ? queued.value.id : undefined;
      if (receipt !== undefined || entries.some(entry => entry.kind === QueueDelivery)) {
        const decremented = (["steering", "followUp"] as const).filter(mode => after[mode].length < this.#pending[mode].length);
        if (decremented.length) {
          assert(decremented.length === 1, "profile handoff must transfer one input");
          const mode = decremented[0];
          assert.deepEqual(after[mode], this.#pending[mode].slice(1));
          const source = c.value.handoff;
          if (receipt !== undefined) {
            assert(source && typeof source === "object" && !Array.isArray(source) && source.id === receipt,
              "staging handoff does not identify its native admission");
          }
          this.#transit.push({ mode, text: this.#pending[mode][0], receipt });
        }
      }
      this.#pending = after;
      this.#publishQueue();
    }
    for (const entry of entries) {
      if (SystemEntry.is(entry) || UserEntry.is(entry) || entry.kind === QueueDelivery) {
        for (const message of entry.model ?? []) this.#inputs.push({ message, delivery: entry.kind === QueueDelivery });
      } else if (AssistantEntry.is(entry)) {
        const message = entry.model?.[0];
        const turn = this.#turn;
        assert(turn && entry.byTaskId === turn.task && turn.sawSource && message?.role === "assistant",
          "assistant commit has no matching pre-fold provider trace");
        assert(!turn.assistant, "turn already has an assistant result");
        turn.assistant = { entry: entry.id, message };
        if (!turn.sawStart) this.journal.push({ type: "message_start", message });
        this.#endMessage(message);
        if (this.#retry && message.stopReason !== "error" && !this.#abortRequested) {
          this.journal.push({ type: "auto_retry_end", success: true, attempt: this.#retry });
          this.#retry = 0;
        }
        const usesTools = message.stopReason === "toolUse" && message.content.some(block => block.type === "toolCall");
        if (!usesTools) this.#endTurn([]);
        const task = generation.find(t => t.id === entry.byTaskId);
        const point = task?.state.status === "sleeping" || task?.state.status === "waiting" ? task.state.checkpoint : undefined;
        if (point && typeof point === "object" && !Array.isArray(point) && point.phase === "prepare" && this.#overflow) {
          assert(point.compacted === this.#overflow.task && task?.id === this.#overflow.owner && message.stopReason === "error",
            "overflow task is not the attempt's blocking child");
          // Pi's willRetry at agent_end describes *generation backoff*, not overflow recovery.
          this.#endAttempt(false);
        } else if (point && typeof point === "object" && !Array.isArray(point) && point.phase === "retry") {
          const attempt = point.attempt;
          assert(typeof attempt === "number" && attempt === this.#retry + 1 && message.stopReason === "error",
            "retry has no matching failed attempt");
          this.#endAttempt(true);
          this.#retry = attempt;
          this.journal.push({ type: "auto_retry_start", attempt, maxAttempts: this.#policy.maxRetries,
            delayMs: retryDelayMs(this.#policy, attempt), errorMessage: message.errorMessage ?? "Unknown error" });
        }
      } else if (ToolResultEntry.is(entry)) {
        const message = entry.model?.[0];
        const running = entry.byTaskId === undefined ? undefined : this.#toolCalls.get(entry.byTaskId);
        assert(message?.role === "toolResult" && running?.call.id === message.toolCallId,
          "tool result has no matching execution source");
        this.#toolCalls.delete(entry.byTaskId!);
        const { durationMs, ...withoutDuration } = message;
        // A Durable result can carry measured phase time even when lookup/validation
        // rejected it. Pi duration is execute() time, not a rejected phase's duration.
        const measured = !running.rejected && durationMs !== undefined;
        this.#toolResults.set(entry.id, measured ? message : withoutDuration);
        this.journal.push({ type: "tool_execution_end", toolCallId: running.call.id, toolName: running.call.name, isError: message.isError,
          result: { content: message.content, details: message.details ?? {} },
          ...(measured ? { durationMs } : {}) });
      }
    }
    const turn = this.#turn;
    if (turn && !turn.assistant && generation.some(task => task.id === turn.task && task.abortRequested &&
        task.state.status === "terminal" && task.state.outcome.status === "aborted")) {
      // A fast abort need not persist a throttled partial. The same invocation's tapped
      // terminal response is valid evidence; a bare abort mark or receipt is not.
      assert(turn.signal.aborted && turn.providerAbort?.stopReason === "aborted",
        "abort has no authoritative assistant result");
      turn.assistant = { message: turn.providerAbort };
      if (!turn.sawStart) this.journal.push({ type: "message_start", message: turn.providerAbort });
      this.#endMessage(turn.providerAbort);
      this.#endTurn([]);
    }
    // An old generation's terminal row can share a commit with a successor. Don't split the Pi run
    // on that row, or on an input receipt: afterRun can hold the generation in finish/drain.
    if (this.#sawRun && this.#active.size === 0) {
      assert(!this.#overflow, "overflow recovery ended before the fixture could verify re-entry");
      assert(!this.#transit.length || this.#hostSettlement && this.#abortRequested,
        "local handoff was interrupted before a verified delivery");
      if (this.#attempt) this.#endAttempt(false);
      if (this.#retry) {
        assert(this.#last, "retry ended without an observed message");
        this.journal.push({ type: "auto_retry_end", success: false, attempt: this.#retry,
          finalError: this.#abortRequested ? "Retry cancelled" : this.#last.message.errorMessage ?? "Unknown error" });
      }
      if (this.#hostSettlement) {
        assert(!this.#drain, "more than one held run drain");
        this.#drain = { aborted: this.#abortRequested, answer: this.#last };
      } else {
        this.journal.push({ type: "agent_settled", aborted: this.#abortRequested });
      }
      this.#retry = 0;
      this.#sawRun = false;
      this.#abortRequested = false;
      this.#last = undefined;
    }
  }
}
