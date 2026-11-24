// Pi input admission and staging over public Durable 1.6 APIs.
import type { Context } from "@earendil-works/chord/context";
import type { JsonRepresentation } from "@earendil-works/chord/json";
import type { ImageContent } from "@earendil-works/pi-ai";
import {
  ConversationBusy, defineDoc, defineExtension, GenerationTask, hook, InboxDoc, LiveDoc,
  type Conversation, type Harness, type HookApi, type SubmissionId, type UserInput,
} from "@earendil-works/pi-durable";

export type Mode = "steer" | "followUp";
export type Input = { text: string; images?: JsonRepresentation<ImageContent>[] };
type Pending = {
  steer: Input[];
  followUp: Input[];
  handoff?: { id: SubmissionId; mode: Mode; input: Input };
  restored?: SubmissionId;
};
export type Queues = { steering: string[]; followUp: string[] };
export type Admission = { disposition: "started"; submissionId: SubmissionId } | { disposition: "queued" };

// The owning Harness decides storage: a regular conversation persists this document; an invocation-private
// MemoryStorage does not. A fork does not inherit pending commands.
// Correlate a placement with its staging item by identity, never by matching text. This
// entry kind contributes the same UserMessage to Durable as pi.user, with no payload copy.
export const QueueDelivery = "hush.pi-queue-delivery";

export const PendingInputs = defineDoc<Pending>({
  kind: "hush.pi-pending-inputs",
  version: 1,
  scope: "conversation",
  history: "latest",
  fork: "initial",
  initial: () => ({ steer: [], followUp: [] }),
  checkpointWhen: (value, _ops, info) =>
    (value.steer.length === 0 && value.followUp.length === 0) || info.deltasSinceBase >= 63,
});

const queues = (pending?: Readonly<Pending>): Queues => ({
  steering: pending?.steer.map((item) => item.text) ?? [],
  followUp: pending?.followUp.map((item) => item.text) ?? [],
});

function copyInput(input: Input): Input {
  const images = input.images?.map(({ data, mimeType }): JsonRepresentation<ImageContent> => ({
    type: "image", data, mimeType,
  }));
  return images === undefined ? { text: input.text } : { text: input.text, images };
}

function content(input: Input): UserInput {
  return [{ type: "text", text: input.text }, ...(input.images ?? [])];
}

export class QueueFull extends Error {
  constructor() { super("The pending input queue is full"); }
}

function stage(pending: Pending, mode: Mode, input: Input) {
  const size = pending.steer.length + pending.followUp.length;
  const bytes = (item: Input) => Buffer.byteLength(item.text) + (item.images ?? []).reduce((sum, image) => sum + image.data.length, 0);
  if (size >= 64 || [...pending.steer, ...pending.followUp].reduce((sum, item) => sum + bytes(item), bytes(input)) > 2 * 1024 * 1024) {
    throw new QueueFull();
  }
  pending[mode].push(input);
}

export class LocalQueue {
  private readonly harness: Harness;
  private readonly conversation: Conversation;

  constructor(harness: Harness, conversation: Conversation) {
    this.harness = harness;
    this.conversation = conversation;
  }

  inspect(context: Context): Promise<Queues> {
    return this.harness.snapshot(PendingInputs, this.conversation.id, context).then(queues);
  }

  async seed(context: Context) {
    const pending = await this.harness.snapshot(PendingInputs, this.conversation.id, context);
    return { pending: queues(pending), restored: pending?.restored };
  }

  // Explicit Pi steer/follow_up never starts an idle run, unlike Durable submit.
  async enqueue(mode: Mode, input: Input, context: Context): Promise<{ disposition: "queued" }> {
    const item = copyInput(input);
    await this.conversation.commit(async (tx) => {
      stage(await tx.doc(PendingInputs, this.conversation.id), mode, item);
    }, context);
    return { disposition: "queued" };
  }

  clear(context: Context): Promise<Queues> {
    return this.conversation.commit(async (tx) => {
      const pending = await tx.doc(PendingInputs, this.conversation.id);
      const removed = queues(pending);
      pending.steer.length = 0;
      pending.followUp.length = 0;
      return removed;
    }, context);
  }

  async prompt(input: Input, streamingBehavior: Mode | undefined, context: Context): Promise<Admission> {
    const conv = this.conversation;
    const item = copyInput(input);
    const result = await conv.commit(async (tx): Promise<Admission> => {
      const pending = await tx.doc(PendingInputs, conv.id);
      const live = await tx.doc(LiveDoc, conv.id);
      if (live.run !== undefined) {
        if (streamingBehavior === undefined) throw new ConversationBusy(conv.id);
        stage(pending, streamingBehavior, item);
        return { disposition: "queued" };
      }
      // A previous native user queue would change idle placement order. Don't silently mix queue owners.
      if ((await tx.doc(InboxDoc, conv.id)).items.some((row) => row.mode !== "write")) {
        throw new Error("Cannot start a local-queue prompt with native user inputs pending");
      }
      const submissionId = await conv.submit(tx, { type: "input", content: content(item), whenBusy: "reject" });
      const firstSteer = pending.steer[0];
      if (firstSteer !== undefined) {
        // Pi includes the first prequeued steer *after* the prompt but *before* the first request.
        // The same transaction admits the prompt, appends that separate user message and removes it from staging.
        await tx.appendEntry(conv.id, {
          kind: QueueDelivery,
          model: [{ role: "user", content: content(firstSteer), timestamp: Date.now() }],
        });
        pending.steer.shift();
      }
      return { disposition: "started", submissionId };
    }, context);
    if (result.disposition === "started") this.harness.resume();
    return result;
  }

  // At an idle profile-owned boundary, deliver one late input without a synthetic prompt or
  // a second steer. The commit is the only point that transfers it out of staging.
  async resumeAfterDrain(context: Context): Promise<SubmissionId | undefined> {
    const conv = this.conversation;
    const resumed = await conv.commit(async tx => {
      const pending = await tx.doc(PendingInputs, conv.id);
      const mode = pending.steer.length ? "steer" : "followUp";
      const item = pending[mode][0];
      if (item === undefined) return undefined;
      if ((await tx.doc(InboxDoc, conv.id)).items.length) {
        throw new Error("Cannot resume a profile drain with a native inbox item");
      }
      const id = await conv.submit(tx, {
        type: "input", content: content(item), whenBusy: "reject", entry: { kind: QueueDelivery },
      });
      pending[mode].shift();
      return id;
    }, context);
    if (resumed !== undefined) this.harness.resume();
    return resumed;
  }

  // Restore only the exact withdrawn handoff that never became a user entry.
  // A placed input remains native history, even if the run ended before delivery.
  async abort(context: Context): Promise<void> {
    await this.conversation.abort(context);
    await this.reconcile(context);
  }

  // A fresh invocation may inspect the exact saved handoff after recovery.
  // Never retry an admission or restore an input on the strength of a task status.
  async reconcile(context: Context): Promise<void> {
    const conv = this.conversation;
    if (!(await this.harness.snapshot(PendingInputs, conv.id, context))?.handoff) return;
    await conv.commit(async tx => {
      const pending = await tx.doc(PendingInputs, conv.id);
      const handoff = pending.handoff;
      if (!handoff) return;
      const receipt = await tx.submission(handoff.id);
      if (receipt?.type !== "input" || receipt.conversationId !== conv.id || receipt.status === "queued") {
        throw new Error("Queue handoff did not reach a known boundary");
      }
      if (receipt.status === "unanswered" && receipt.reason === "aborted" && receipt.entry === undefined) {
        pending[handoff.mode].unshift(copyInput(handoff.input));
        pending.restored = handoff.id;
      }
      delete pending.handoff;
    }, context);
  }
}

// Keep the queue in a hush-owned document. Transfer at most one item into the native inbox at a supported
// boundary. The commit is the selection fence: clear() before it wins; afterwards the item is no longer
// pending locally. No submit loops or writes into pi.inbox / pi.live.
export function localQueueHooks(getHarness: () => Harness) {
  async function handoff(api: HookApi, context: Context, allowFollowUp: boolean) {
    const conv = await getHarness().conversation(api.conversationId, context);
    if (conv === undefined) return;
    await conv.commit(async (tx) => {
      const pending = await tx.doc(PendingInputs, conv.id);
      const mode = pending.steer.length > 0 ? "steer" : allowFollowUp ? "followUp" : undefined;
      if (mode === undefined) return;
      const input = pending[mode][0];
      if (input === undefined) return;
      const live = await tx.doc(LiveDoc, conv.id);
      if (live.run?.taskId !== api.taskId) return;
      // An abort may have committed before its signal reaches this off-line hook.
      const active = await tx.task(api.taskId);
      if (active?.state.status !== "running" || active.abortRequested) return;
      if ((await tx.doc(InboxDoc, conv.id)).items.some((row) => row.mode !== "write")) return;
      const selected = copyInput(input);
      const id = await conv.submit(tx, {
        type: "input", content: content(selected), whenBusy: mode, whenIdle: "reject", entry: { kind: QueueDelivery },
      });
      pending.handoff = { id, mode, input: selected };
      pending[mode].shift();
    }, context);
  }

  return defineExtension({
    name: "hush.pi-local-queue",
    hooks: [hook(GenerationTask, {
      async afterTools(_answer, _results, api, context) { await handoff(api, context, false); },
      async onYield(_answer, api, context) { await handoff(api, context, true); },
      // A run with a late input during afterRun can still select it. Native selection is not re-opened.
      async afterRun(_run, api, context) { await handoff(api, context, true); },
    })],
  });
}
