import { createHash, randomBytes, randomUUID } from "node:crypto";
import { setTimeout as delay } from "node:timers/promises";
import { BACKGROUND_CONTEXT, withAbortSignal, type Context } from "@earendil-works/chord/context";
import { AgentDoc, configure, defineDoc, defineDocFamily, defineEntry, defineExtension, defineTask, defineTool, InboxDoc, LiveDoc, SubmissionSettled,
  type AgentChange, type AgentState, type Conversation, type ConversationId, type Cursor, type Harness, type DocumentReader, type LiveState, type Observation, type Observer, type Storage, type SubmissionId, type TaskId, type Tx } from "@earendil-works/pi-durable";
import { planChatKind, planHistoryPageSize, planStepLimit, planRunKind, planTaskKind, planResultKind,
  type PlanAttachment, type PlanChatHost, type PlanClosure, type PlanDefinition, type PlanHistoryQuery,
  type PlanHost, type PlanInspectAt, type PlanProgress, type PlanRun, type PlanRunRecord, type PlanRunState, type PlanSelection, type PlanSnapshot,
  type PlanStep, type PlanTaskReport, type PlanWorkspaceAt, type PlanWorkspaceQuery, type PlanWorktreeCheckout,
  type PlanWorktreeHost, type PlanWorktreeSource } from "../../server/src/plan.js";
import { checkPreparedModel, type NativeHost } from "../../server/src/durable.js";
import type { PlanReviewTarget } from "../../server/src/plan-review.js";
import { object, WireError } from "../../server/src/wire.js";
import { PlanFileError, type PlanFiles, type FileProposal, type PlanFileTarget } from "./plan-files.js";
import type { PlanAcceptance } from "./plan-acceptance.js";
import { PlanCatalog, planInventoryPage, type ActivePlanRun, type WorkspacePlans } from "./plan-catalog.js";
import { observe } from "./workspace-changes.js";
import { ThreadPresentationDoc, threadLabel } from "./thread-presentation.js";
import { checkoutIdentity, planWorktreeIdentity, type PlannedCheckout, type PlanWorktreeIdentity, type ThreadWorktrees } from "./worktrees.js";
import { copyTurnWork, foregroundPlan, PlanForeground, scopedPlanTask, type PlanRunModel, type ScopedRun, type TaskTurn } from "./plan-turn.js";

const cursor = () => randomBytes(32).toString("hex");
const thread = (id: string) => Number(id) as ConversationId;
const invalid = (message: string) => { const e = new WireError("invalid_request"); e.message = message; return e; };
const result = (text: string) => ({ output: [{ type: "text" as const, text }] });
const receiptLimit = 128;
export const planRevisionLimit = 128;
const bindingLimit = 32;

type Pin = { planId: string; revision: number };
type Binding = { cursor: string; pin?: Pin & { liveThread: ConversationId } };
type PlanState = { cursor: string; revision: number; workspace: string; members: ConversationId[];
  fixture: boolean; progress: PlanProgress[]; run?: PlanRun; previousTask?: string;
  closed?: true; halt?: true; lifecycle?: string; publication?: { stage?: string }; reports?: PlanTaskReport[] };
type Receipt = Omit<PlanSelection, "threadId" | "requestId"> & { runId: string; chat?: true };
type ChatRunRef = Pin & { runId: string };
type ChatRunView = Pin & { run: PlanRun; progress: PlanProgress[]; reports: PlanTaskReport[] };
type ChatActivity = { cursor: string; pending: ChatRunRef[]; last?: ChatRunView };
type Assignment = { checks?: string[]; evidence?: string[] };
type WorktreeSelection = Pick<PlanSelection, "planId" | "revision" | "stepIds" | "requestId">;
type WorktreeAllocation = WorktreeSelection & PlannedCheckout & { identity?: PlanWorktreeIdentity; lifecycle?: string };
type WorktreeOptions = {
  checkout: ThreadWorktrees;
  defaults: NonNullable<NativeHost["defaults"]>;
  validateSelection: NonNullable<NativeHost["validateSelection"]>;
  name(ref: NonNullable<AgentState["model"]>, title: string, tasks: readonly string[], signal: AbortSignal): Promise<string | undefined>;
};
const WorktreeLaunch = defineDoc<{ allocation?: WorktreeAllocation }>({
  kind: "hush.plan-worktree-launch", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});
const WorktreePreparation = defineDocFamily<{ receipt?: { source: string; target: PlanWorktreeCheckout; ready?: true } }, null>({
  kind: "hush.plan-worktree-preparation", version: 1, family: true, scope: "session",
  initial: () => ({}), checkpointWhen: () => true,
});
const incompleteWorktree = (id: string) => invalid(`Worktree setup incomplete · inspect thread #${id} in /threads.`);

// A fork copies the binding as of its anchor. Only the original liveThread
// follows the head; the copy's revision is historical and grants no Run.
const Binding = defineDoc<Binding>({
  kind: "hush.plan-binding", version: 1, scope: "conversation", history: "rewindable", fork: "asOf",
  initial: () => ({ cursor: cursor() }), checkpointWhen: () => true,
});
const Marker = defineEntry<Pin>("hush.plan.pin");
const State = defineDocFamily<PlanState, null>({
  kind: "hush.plan-head", version: 1, family: true, scope: "session",
  initial: () => ({ cursor: cursor(), revision: 0, workspace: "", members: [], fixture: false, progress: [] }), checkpointWhen: () => true,
});
export type DraftSource = { entryId: string; text: string };
export type PlanSources = { inputs: DraftSource[]; partial: boolean };
const Definition = defineDocFamily<{ plan?: PlanDefinition; sources?: PlanSources }, null>({
  kind: "hush.plan-revision", version: 1, family: true, scope: "session",
  initial: () => ({}), checkpointWhen: () => true,
});
const Receipts = defineDoc<{ records: { requestId: string; receipt: Receipt }[] }>({
  kind: "hush.plan-fixture-receipts", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ records: [] }), checkpointWhen: () => true,
});
// Run here does not bind a plan to its thread. Follow its native inputs
// separately, oldest first, and don't copy activity into forks.
const ChatActivity = defineDoc<ChatActivity>({
  kind: "hush.plan-chat-activity", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ cursor: cursor(), pending: [] }), checkpointWhen: () => true,
});
const Work = defineDoc<Assignment>({
  kind: "hush.faux-plan-assignment", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});
const WorkspaceCatalog = defineDocFamily<WorkspacePlans, null>({
  kind: "hush.plan-workspace", version: 1, family: true, scope: "session",
  initial: () => ({ cursor: cursor(), rows: [] }), checkpointWhen: () => true,
});
const CatalogReady = defineDoc<{ ready?: true; runOrder?: true }>({
  kind: "hush.plan-workspace-indexed", version: 1, scope: "session", initial: () => ({}), checkpointWhen: () => true,
});
const RunLog = defineDocFamily<{ runs: PlanRunRecord[] }, null>({
  kind: "hush.plan-run-log", version: 1, family: true, scope: "session", initial: () => ({ runs: [] }), checkpointWhen: () => true,
});
const revisionKey = (id: string, revision: number) => `${id}:${revision}`;
const verifiedCount = (state: PlanState) => state.progress.filter(p => p.state === "verified").length;
const reportedCount = (state: PlanState) => state.reports?.filter(r => r.state === "done" &&
  !state.progress.some(p => p.stepId === r.stepId && p.state === "verified")).length ?? 0;

function runReceipt(records: { requestId: string; receipt: Receipt }[], q: PlanSelection, chat?: true) {
  const existing = records.find(r => r.requestId === q.requestId)?.receipt;
  if (!existing) return;
  if (existing.chat === chat && existing.planId === q.planId && existing.revision === q.revision && existing.stepIds.length === q.stepIds.length &&
      existing.stepIds.every((id, i) => id === q.stepIds[i])) return existing.runId;
  throw invalid("Run request ID already belongs to a different selection or Run mode.");
}

async function refreshCatalog(tx: Tx, planId: string, state: PlanState, definition?: PlanDefinition) {
  const catalog = await tx.doc(WorkspaceCatalog, state.workspace, null);
  const index = catalog.rows.findIndex(r => r.id === planId);
  const previous = catalog.rows[index];
  const plan = definition ?? (previous?.revision === state.revision ? previous : await readDefinition(tx, { planId, revision: state.revision }));
  const total = "steps" in plan ? plan.steps.length : plan.total, verified = verifiedCount(state);
  const status = verified === total ? "complete" : state.run?.state === "running" ? "running" : state.run?.state === "failed" ? "attention" : "pending";
  const reportedDone = reportedCount(state);
  let activeRun: ActivePlanRun | undefined;
  if (state.run?.state === "running") {
    const { id, chat } = state.run;
    // Scripted runs predate ownerThreadId on the head; their log has it.
    const ownerThreadId = state.run.ownerThreadId ?? (await logRun(tx, planId, id))?.threadId;
    if (ownerThreadId) activeRun = { id, ownerThreadId, ...(chat && { chat }) };
  }
  const row = { id: planId, title: plan.title, revision: state.revision, total, verified, status, closed: !!state.closed,
    ...(reportedDone && { reportedDone }), ...(activeRun && { activeRun }) } satisfies WorkspacePlans["rows"][number];
  if (previous && previous.revision === row.revision && previous.verified === verified && (previous.reportedDone ?? 0) === reportedDone &&
      previous.status === status && previous.closed === row.closed && previous.activeRun?.id === activeRun?.id &&
      previous.activeRun?.ownerThreadId === activeRun?.ownerThreadId && previous.activeRun?.chat === activeRun?.chat) return;
  if (index < 0) catalog.rows.push(row);
  else catalog.rows[index] = row;
  catalog.cursor = cursor();
}

async function update(tx: Tx, planId: string, state: PlanState, plan?: PlanDefinition) {
  state.cursor = cursor();
  await refreshCatalog(tx, planId, state, plan);
}

async function logRun(tx: Tx, planId: string, runId: string) {
  return (await tx.doc(RunLog, planId, null)).runs.findLast(r => r.id === runId);
}

async function checkThread(tx: Tx, owner: ConversationId) {
  const record = await tx.conversation(owner);
  if (!record || record.owner) throw new WireError("not_found");
}

export async function readPlanRevision(tx: Tx, pin: Pin) {
  const record = await tx.doc(Definition, revisionKey(pin.planId, pin.revision), null);
  if (!record.plan) throw new WireError("internal");
  return { plan: record.plan, sources: record.sources };
}

async function readDefinition(tx: Tx, pin: Pin) { return (await readPlanRevision(tx, pin)).plan; }

export async function notePlanReviewChange(tx: Tx, planId: string) {
  const state = await tx.doc(State, planId, null);
  state.cursor = cursor();
  return state.revision;
}

function combineSources(previous: PlanSources | undefined, draft: PlanSources, revising: boolean): PlanSources {
  const inputs = new Map(previous?.inputs.map(input => [input.entryId, { ...input }]));
  for (const input of draft.inputs) inputs.set(input.entryId, input);
  const ordered = [...inputs.values()];
  // Keep the original request and the most recent amendments under a bounded
  // context budget. Missing old source or omitted content is explicit.
  const kept = ordered.length > 6 ? [ordered[0], ...ordered.slice(-5)] : ordered;
  return { inputs: kept, partial: draft.partial || !!previous?.partial || revising && !previous?.inputs.length || kept.length < ordered.length };
}

async function mark(tx: Tx, owner: ConversationId, pin: Pin) {
  // No model messages: this is a committed fork boundary, not prompt input.
  await tx.appendEntry(Marker, owner, { data: { planId: pin.planId, revision: pin.revision } });
}

function setBinding(binding: Binding, planId: string, revision: number, liveThread: ConversationId) {
  binding.pin = { planId, revision, liveThread };
  binding.cursor = cursor();
}

export async function currentPlan(tx: Tx, owner: ConversationId) {
  await checkThread(tx, owner);
  const { pin } = await tx.doc(Binding, owner);
  if (!pin) return { plan: null, pinned: false };
  const plan = await readDefinition(tx, pin), pinned = pin.liveThread !== owner;
  if (pinned) return { plan, pinned };
  const state = await tx.doc(State, pin.planId, null);
  return { plan, pinned, closed: !!state.closed, complete: verifiedCount(state) === plan.steps.length };
}

export async function proposePlan(tx: Tx, owner: ConversationId, directory: (cwd?: string) => string,
    fixture: boolean, baseRevision: number, title: string, steps: PlanStep[], sources: PlanSources) {
  await checkThread(tx, owner);
  const binding = await tx.doc(Binding, owner), pin = binding.pin;
  if (pin && pin.liveThread !== owner) throw invalid("This revision is pinned. Attach to the live plan before revising it.");
  if (pin) {
    const previous = await tx.doc(State, pin.planId, null);
    const current = await readDefinition(tx, pin);
    if (previous.run?.state === "running") throw new WireError("busy");
    if (previous.closed || verifiedCount(previous) === current.steps.length) {
      if (baseRevision !== 0) throw invalid("Closed or completed plans cannot be revised. Create a new plan or reopen an incomplete one.");
      await unbind(tx, owner, pin, previous);
    } else if (pin.revision !== baseRevision) {
      throw invalid("The plan revision is stale. Read the current plan before revising it.");
    }
  } else if (baseRevision !== 0) {
    throw invalid("The plan revision is stale. Read the current plan before revising it.");
  }
  if (baseRevision >= planRevisionLimit) throw new WireError("limit");
  const planId = baseRevision === 0 ? randomUUID() : pin!.planId;
  const state = await tx.doc(State, planId, null);
  if (baseRevision === 0) {
    state.workspace = directory((await tx.doc(AgentDoc, owner)).cwd);
    state.members = [owner];
    state.fixture = fixture;
  } else if (state.fixture !== fixture) {
    throw invalid("Fixture plans and normal plans cannot be revised on each other's hosts.");
  }
  if (state.revision !== baseRevision || !state.members.includes(owner)) throw new WireError("internal");
  const plan = { id: planId, revision: baseRevision + 1, title, steps };
  const previousSources = baseRevision === 0 ? undefined : (await readPlanRevision(tx, { planId, revision: baseRevision })).sources;
  const record = await tx.doc(Definition, revisionKey(planId, plan.revision), null);
  if (record.plan) throw new WireError("internal");
  record.plan = plan;
  record.sources = combineSources(previousSources, sources, baseRevision > 0);
  state.revision = plan.revision;
  state.progress = [];
  delete state.reports;
  if (state.run) state.previousTask = state.run.id;
  delete state.run;
  await update(tx, planId, state, plan);
  // The bounded member list keeps each attached thread's fork history
  // exact at its next transcript entry, even when a peer made the edit.
  for (const member of state.members) setBinding(await tx.doc(Binding, member), planId, plan.revision, member);
  await mark(tx, owner, { planId, revision: plan.revision });
  return { planId, revision: plan.revision };
}

async function unbind(tx: Tx, owner: ConversationId, pin: Pin & { liveThread: ConversationId }, previous: PlanState) {
  if (pin.liveThread !== owner) return;
  if (previous.run?.state === "running" && (!previous.run.ownerThreadId || previous.run.ownerThreadId === String(owner))) {
    throw new WireError("busy");
  }
  previous.members = previous.members.filter(m => m !== owner);
  previous.cursor = cursor(); // Wake a bound wait so it can follow the new binding.
}

// The only tool offered to a fixture worker. It checks a harness-owned fixture,
// writes no workspace files, and records evidence independently of model prose.
const check = defineTool({
  name: "hush_fixture_plan_check", description: "Run the scripted acceptance checks for this fixture assignment.", replay: "safe",
  parameters: { type: "object", properties: {}, additionalProperties: false } as const,
  async execute(_args, api, ctx) {
    // Give the terminal a running state to inspect. Nothing external is replayed.
    await delay(300, undefined, { signal: ctx.abortSignal });
    return api.commit(async tx => {
      const work = await tx.doc(Work, api.conversationId);
      if (!work.checks) return { ...result("No fixture assignment."), isError: true };
      work.evidence ??= work.checks.map(check => `Fixture passed: ${check}`);
      return result("Fixture checks passed.");
    }, ctx);
  },
});

const worker = defineExtension({ name: "hush-faux-plan-worker", tools: [check] });
const ended = { status: "terminal", outcome: { status: "completed", result: null } } as const;
const aborted = { status: "terminal", outcome: { status: "aborted" } } as const;
type Phase = { phase: "next" } | { phase: "submit"; stepId: string; worker: ConversationId }
  | { phase: "settle"; stepId: string; worker: ConversationId; submission: SubmissionId } | { phase: "settleMain"; stepId: string }
  | { phase: "publishMain"; stepId: string; stage: string; patch: FileProposal[]; evidence: string }
  | { phase: "interrupted"; uncertain: boolean };
const next = { status: "running", checkpoint: { phase: "next" } } as const;
const doneSignal = "settled";
export const planFixtureTaskKind = "hush.plan-fixture-run";
const scopedTask = scopedPlanTask;
type RunInput = Pin & { scoped?: ScopedRun };
// One scoped writer batch per store. Nested workspaces and aliases must
// not bypass exclusivity; disjoint-workspace concurrency is a later slice.
const ExecutionClaim = defineDoc<{ taskId?: string; owner?: ConversationId }>({
  kind: "hush.plan-execution-claim", version: 1, scope: "session",
  initial: () => ({}), checkpointWhen: () => true,
});

const samePlanModel = (agent: Readonly<AgentState>, expected: PlanRunModel) =>
  agent.model?.provider === expected.model.provider && agent.model.modelId === expected.model.modelId &&
  (agent.thinkingLevel ?? "off") === expected.thinking;

function planRunHeading(plan: PlanDefinition, steps: number) {
  return `Plan run · r${plan.revision} · ${plan.title.replace(/\s+/gu, " ").trim()} · ${steps} ${steps === 1 ? "step" : "steps"}`;
}

async function reportTurn(tx: Tx, kind: typeof planRunKind | typeof planResultKind, threadId: string, runId: string,
    text: string, fields: { stepId?: string; planId?: string; revision?: number; stepIds?: string[]; outcome?: string } = {}) {
  await tx.appendEntry(thread(threadId), { kind, data: { runId, ...fields, text, timestampMs: Date.now() } });
}

export async function recoverPlanGeneration(h: Harness, ctx: Context) {
  const claim = await h.snapshot(ExecutionClaim, ctx);
  if (claim?.owner === undefined) return;
  const run = (await h.snapshot(PlanForeground, claim.owner, ctx))?.run;
  if (!run?.turn || claim.taskId !== String(run.id)) return;
  // The plan coordinator is abandoned on restart; its native generation is
  // conversation-owned and must be canceled separately. Never continue it.
  const live = (await h.snapshot(LiveDoc, claim.owner, ctx))?.run;
  if (live?.first === run.turn.entry && live.inputs.includes(run.turn.submission)) await h.abortTask(live.taskId, ctx);
}

function chatRunView({ planId, revision }: Pin, run: PlanRun, state: Pick<PlanState, "progress" | "reports">): ChatRunView {
  return { planId, revision, run: { ...run, stepIds: [...run.stepIds] },
    progress: state.progress.map(p => ({ ...p, ...(p.evidence && { evidence: [...p.evidence] }) })),
    reports: state.reports?.map(r => ({ ...r })) ?? [] };
}

async function finishRun(tx: Tx, planId: string, state: PlanState, outcome: Exclude<PlanRunState, "running">, message?: string) {
  if (!state.run) throw new Error("Missing admitted run");
  if (state.run.state !== "running") return ended;
  state.run.state = outcome;
  if (outcome === "failed" && state.publication) {
    state.run.publicationUncertain = true;
    message = `${message ?? "Run interrupted."} Files may have been published but were not verified.` +
      (state.publication.stage ? ` Check .hush-plan-${state.publication.stage} if present.` : " Inspect changes before retrying.");
  }
  if (message) state.run.failure = message.slice(0, 240).toWellFormed();
  delete state.publication;
  delete state.halt;
  const row = await logRun(tx, planId, state.run.id);
  if (row) {
    row.state = outcome;
    if (state.run.failure) row.failure = state.run.failure;
    if (state.run.publicationUncertain) row.publicationUncertain = true;
    for (const turn of [...row.workers, ...(row.turns ?? [])]) if (turn.state === "running") turn.state = "failed";
    if (row.chat) {
      if (outcome !== "done") delete row.reports;
      const label = outcome === "done" ? "Plan run finished" : outcome === "closed" ? "Plan run closed" : "Plan run interrupted";
      const details = row.reports?.map(r => `${r.stepId}: ${r.state === "done" ? "reported done" : r.state} · ${r.detail}`).join("\n")
        ?? (outcome === "done" ? "No task outcomes reported." : undefined);
      await reportTurn(tx, planResultKind, row.threadId, row.id,
        `${label}${state.run.failure ? "\n" + state.run.failure : ""}${details ? "\n" + details : ""}`, { outcome });
      const activity = await tx.doc(ChatActivity, thread(row.threadId));
      const index = activity.pending.findIndex(r => r.runId === row.id);
      if (index >= 0) {
        activity.pending.splice(index, 1);
        activity.last = chatRunView({ planId, revision: state.revision }, state.run, state);
        activity.cursor = cursor();
      }
    } else if (row.turns) {
      const foreground = await tx.doc(PlanForeground, thread(row.threadId));
      if (String(foreground.run?.id) === row.id) delete foreground.run;
      const verified = row.turns.filter(turn => turn.state === "verified").length;
      const label = outcome === "verified" ? "Plan run verified" : outcome === "closed" ? "Plan run closed" : "Plan run not verified";
      await reportTurn(tx, planResultKind, row.threadId, row.id,
        `${label} · ${verified}/${row.stepIds.length} verified${state.run.failure ? "\n" + state.run.failure : ""}`, { outcome });
    }
  }
  for (const progress of state.progress) if (progress.state === "running") progress.state = "failed";
  await update(tx, planId, state);
  return ended;
}

const failRun = (tx: Tx, planId: string, state: PlanState, message: string) => finishRun(tx, planId, state, "failed", message);

async function verifyStep(tx: Tx, planId: string, state: PlanState, stepId: string, evidence: string[]) {
  const progress = state.progress.find(p => p.stepId === stepId);
  if (!progress || !state.run) throw new Error("Task state is missing");
  progress.state = "verified";
  progress.evidence = evidence.slice();
  const record = await logRun(tx, planId, state.run.id);
  const item = (record?.turns ?? record?.workers)?.find(item => item.stepId === stepId);
  if (item) item.state = "verified";
  if (record?.turns) await reportTurn(tx, planResultKind, record.threadId, record.id, evidence.join("\n"), { stepId, outcome: "verified" });
  if (state.run.stepIds.every(id => state.progress.some(p => p.stepId === id && p.state === "verified"))) {
    return finishRun(tx, planId, state, "verified");
  }
  if (state.halt) return finishRun(tx, planId, state, "closed");
  await update(tx, planId, state);
  return next;
}

function coordinator(name: typeof scopedTask | typeof planFixtureTaskKind, files?: PlanFiles, acceptance?: PlanAcceptance) {
  return defineTask<RunInput, Phase, null>({
    name, version: name === scopedTask ? 2 : 1, initial: () => next.checkpoint,
    ...(name === scopedTask && { migrate(input: unknown, checkpoint: unknown, fromVersion: number) {
      if (fromVersion !== 1 || !object(input) || typeof input.planId !== "string" || typeof input.revision !== "number") {
        throw new Error("Unsupported scoped plan task format");
      }
      // Older coordinators could write directly from settleMain (or settle
      // for the worker prototype). Never replay that phase during upgrade.
      return { input: { planId: input.planId, revision: input.revision }, checkpoint: { phase: "interrupted" as const,
        uncertain: object(checkpoint) && (checkpoint.phase === "settleMain" || checkpoint.phase === "settle") } };
    } }),
    phases: {
      next: async (task, runtime, ctx) => {
        const owner = runtime.conversationId;
        const parent = files && await runtime.conversation(owner, ctx);
        return runtime.commit(async tx => {
          const { planId, revision, scoped } = task.input;
          const state = await tx.doc(State, planId, null);
          if (state.run?.id !== String(task.id) || state.revision !== revision) return ended;
          const plan = await readDefinition(tx, task.input);
          const step = plan.steps.find(s => state.run!.stepIds.includes(s.id) &&
            !state.progress.some(p => p.stepId === s.id && p.state === "verified"));
          if (!step) return finishRun(tx, planId, state, "verified");
          if (state.halt) return finishRun(tx, planId, state, "closed");
          if (name === scopedTask) {
            if (!scoped || !parent) return failRun(tx, planId, state, "Scoped execution is unavailable on this host.");
            const foreground = (await tx.doc(PlanForeground, owner)).run;
            const prepared = scoped.steps?.find(file => file.stepId === step.id);
            if (foreground?.id !== task.id || !prepared) return failRun(tx, planId, state, "The plan task has no authorized assignment.");
            const live = await tx.doc(LiveDoc, owner);
            const agent = await tx.doc(AgentDoc, owner);
            if (live.run || (await tx.doc(InboxDoc, owner)).items.length || !samePlanModel(agent, scoped.model)) {
              return failRun(tx, planId, state, "The thread changed before the next plan task could start.");
            }
            const record = await logRun(tx, planId, String(task.id));
            if (!record?.turns) return failRun(tx, planId, state, "The plan run record is missing.");
            // The typed input, task grant and scheduler checkpoint are admitted
            // together. No normal chat prompt or model response grants authority.
            const submission = await parent.submit(tx, { type: "input", whenBusy: "reject",
              content: "Plan assignment\n" + JSON.stringify({ id: step.id, title: step.title, description: step.description, edit: step.edit, scope: step.scope,
                notice: "This is a plan record, not a standing request. Use it only when the current turn grants scoped plan tools." }),
              entry: { kind: planTaskKind, data: { runId: String(task.id), stepId: step.id,
                text: `Plan task · ${step.title}`, timestampMs: runtime.now() } },
              notify: { taskId: task.id, key: doneSignal + ":" + step.id },
            });
            const generation = live.run as LiveState["run"]; // submit updated this transaction's LiveDoc.
            if (!generation?.first || generation.inputs.length !== 1 || generation.inputs[0] !== submission) {
              throw new Error("Could not bind the scoped plan task to its native generation");
            }
            const entryId = String(generation.first);
            foreground.turn = { entry: generation.first, submission, stepId: step.id,
              workspace: scoped.workspace, work: copyTurnWork(prepared.work) };
            state.progress = state.progress.filter(p => p.stepId !== step.id);
            state.progress.push({ stepId: step.id, state: "running", workerThreadId: String(owner), entryId,
              effectiveThinking: scoped.model.thinking });
            record.turns.push({ stepId: step.id, entryId, state: "running" });
            await update(tx, planId, state, plan);
            return { status: "sleeping", signals: [SubmissionSettled.key(doneSignal + ":" + step.id)],
              checkpoint: { phase: "settleMain", stepId: step.id } };
          }
          const child = await tx.createConversation({ ownership: { kind: "task", taskId: task.id } });
          await configure(tx, child.id, { model: { provider: "faux", modelId: "faux-1" }, thinkingLevel: "off",
            extensions: [worker], tools: [check], modelTools: [check], instructions: "Run only the assigned fixture checks.", maxTurns: 4 });
          (await tx.doc(Work, child.id)).checks = step.checks.slice();
          state.progress = state.progress.filter(p => p.stepId !== step.id);
          state.progress.push({ stepId: step.id, state: "running", workerThreadId: String(child.id), effectiveThinking: "off" });
          await update(tx, planId, state, plan);
          return { status: "running", checkpoint: { phase: "submit", stepId: step.id, worker: child.id } };
        }, ctx);
      },
      submit: async (task, runtime, ctx) => {
        const c = task.state.checkpoint;
        const child = task.input.scoped ? undefined : await runtime.conversation(c.worker, ctx);
        return runtime.commit(async tx => {
          const state = await tx.doc(State, task.input.planId, null);
          if (state.run?.id !== String(task.id) || state.revision !== task.input.revision) return ended;
          if (!child) return failRun(tx, task.input.planId, state, "The earlier plan run was interrupted.");
          if (state.halt) {
            state.progress = state.progress.filter(p => p.stepId !== c.stepId);
            return finishRun(tx, task.input.planId, state, "closed");
          }
          const plan = await readDefinition(tx, task.input);
          const step = plan.steps.find(s => s.id === c.stepId);
          if (!step) throw new Error("Pinned plan step is missing");
          const submission = await child.submit(tx, { type: "input", whenBusy: "reject", requestId: `plan:${task.id}:${step.id}`,
            content: `!plan-fixture-work\n${JSON.stringify(step)}`, notify: { taskId: task.id, key: doneSignal + ":" + step.id } });
          (await logRun(tx, task.input.planId, String(task.id)))?.workers.push({ stepId: step.id, threadId: String(c.worker), state: "running" });
          return { status: "sleeping", signals: [SubmissionSettled.key(doneSignal + ":" + step.id)],
            checkpoint: { ...c, phase: "settle", submission } };
        }, ctx);
      },
      settle: (task, runtime, ctx) => runtime.commit(async tx => {
        const c = task.state.checkpoint;
        const state = await tx.doc(State, task.input.planId, null);
        if (state.run?.id !== String(task.id)) return ended;
        if (task.input.scoped) return failRun(tx, task.input.planId, state, "The earlier plan run was interrupted.");
        const submission = await tx.submission(c.submission);
        const evidence = submission?.status === "done" && submission.conversationId === c.worker ? (await tx.doc(Work, c.worker)).evidence : undefined;
        if (!evidence?.length) return failRun(tx, task.input.planId, state, "The fixture did not finish with verified evidence.");
        return verifyStep(tx, task.input.planId, state, c.stepId, evidence);
      }, ctx),
      settleMain: async (task, runtime, ctx) => {
        const { stepId } = task.state.checkpoint;
        const { planId, revision } = task.input, owner = runtime.conversationId;
        // A restarted coordinator is abandoned. An answer is not proof: the
        // backend still needs a settled native task and the scoped proposal.
        let proven: TaskTurn | undefined;
        await runtime.commit(async tx => {
          const state = await tx.doc(State, planId, null);
          const foreground = (await tx.doc(PlanForeground, owner)).run;
          const turn = foreground?.turn;
          if (state.run?.id !== String(task.id) || state.revision !== revision || foreground?.id !== task.id || turn?.stepId !== stepId) return;
          const submission = await tx.submission(turn.submission);
          if (submission?.status !== "done" || submission.conversationId !== owner || submission.entry !== turn.entry ||
              (await tx.doc(LiveDoc, owner)).run) return;
          if (turn.work) proven = { ...turn, workspace: { ...turn.workspace }, work: copyTurnWork(turn.work) };
        }, ctx);
        const turn: TaskTurn | undefined = proven;
        let prepared: { patch: ReadonlyMap<string, string>; evidence: string } | undefined;
        let failure = "The plan task did not finish with a replacement proposal.";
        if (turn && files) {
          try {
            const work = turn.work;
            if ("files" in work) {
              if (!acceptance) throw new PlanFileError("The isolated acceptance runner is unavailable.");
              const candidate = await files.candidate(turn.workspace, work.files, work.proposals, runtime.signal);
              if (await acceptance.check(work.acceptance.script, candidate.before, runtime.signal)) {
                throw new PlanFileError("Acceptance already passes on the original files. Nothing was published.");
              }
              if (!(await acceptance.check(work.acceptance.script, candidate.after, runtime.signal))) {
                throw new PlanFileError("Isolated acceptance did not pass on the proposed files. Nothing was published.");
              }
              prepared = { patch: candidate.patch, evidence: `Isolated acceptance passed: ${work.acceptance.label} (${candidate.patch.size} files)` };
            } else if (work.patch) {
              const path = work.edit.path;
              const patch = await files.literalCandidate(turn.workspace, work.edit, work.before, work.patch, runtime.signal);
              prepared = { patch, evidence: `Literal check passed: ${path.slice(0, 152)}${path.length > 152 ? "…" : ""}` };
            }
          } catch (error) {
            runtime.signal.throwIfAborted();
            failure = error instanceof PlanFileError ? error.message : "Could not prepare the scoped edit. Nothing was published.";
          }
        }
        return runtime.commit(async tx => {
          const state = await tx.doc(State, planId, null);
          if (state.run?.id !== String(task.id)) return ended;
          if (!prepared) return failRun(tx, planId, state, failure);
          const stage = randomBytes(16).toString("hex");
          state.publication = { stage };
          return { status: "running", checkpoint: { phase: "publishMain", stepId, stage, evidence: prepared.evidence,
            patch: [...prepared.patch].map(([path, text]) => ({ path, text })) } };
        }, ctx);
      },
      publishMain: async (task, runtime, ctx) => {
        const { planId, revision } = task.input, owner = runtime.conversationId;
        const { stepId, stage, patch, evidence } = task.state.checkpoint;
        // Read the committed intent. Recovery never repeats a publication.
        let turn: TaskTurn | undefined;
        await runtime.commit(async tx => {
          const state = await tx.doc(State, planId, null);
          const reservation = (await tx.doc(PlanForeground, owner)).run;
          const input = reservation?.turn;
          if (state.run?.id === String(task.id) && state.revision === revision && state.publication?.stage === stage &&
              reservation?.id === task.id && input?.stepId === stepId && input.work && task.restarts === undefined) {
            turn = { ...input, workspace: { ...input.workspace }, work: copyTurnWork(input.work) };
          }
        }, ctx);
        let failure: PlanFileError | undefined;
        if (turn && files) {
          const work = turn.work;
          const targets = "files" in work ? work.files : [{ path: work.edit.path, before: work.before, writable: true }];
          try {
            await files.publish(turn.workspace, targets, new Map(patch.map(p => [p.path, p.text])), stage, runtime.signal);
            runtime.signal.throwIfAborted();
          } catch (error) {
            runtime.signal.throwIfAborted(); // abort handler uses the committed intent
            failure = error instanceof PlanFileError ? error : new PlanFileError("Publication did not settle cleanly.", true);
          }
        } else failure = new PlanFileError("Publication interrupted.", true);
        return runtime.commit(async tx => {
          const state = await tx.doc(State, planId, null);
          if (state.run?.id !== String(task.id)) return ended;
          if (failure) {
            if (!failure.publicationUncertain) delete state.publication;
            const note = failure.recoveryFiles ? `${failure.message} Check .hush-plan-${stage} if present.` : failure.message;
            return failRun(tx, planId, state, note);
          }
          delete state.publication;
          const foreground = (await tx.doc(PlanForeground, owner)).run;
          if (foreground?.id === task.id) delete foreground.turn;
          return verifyStep(tx, planId, state, stepId, [evidence]);
        }, ctx);
      },
      interrupted: (task, runtime, ctx) => runtime.commit(async tx => {
        const state = await tx.doc(State, task.input.planId, null);
        if (state.run?.id !== String(task.id)) return ended;
        if (task.state.checkpoint.uncertain) state.publication ??= {};
        return failRun(tx, task.input.planId, state, "Earlier plan Run interrupted by upgrade.");
      }, ctx),
    },
    abort: (task, runtime, ctx) => runtime.commit(async tx => {
      const state = await tx.doc(State, task.input.planId, null);
      if (state.run?.id === String(task.id)) {
        if (task.state.checkpoint.phase === "interrupted" && task.state.checkpoint.uncertain) state.publication ??= {};
        await failRun(tx, task.input.planId, state, name === scopedTask
          ? "Run stopped. Earlier changes were not rolled back; inspect the workspace before retrying." : "Fixture run stopped.");
      }
      return aborted;
    }, ctx),
  });
}

const executeFixture = coordinator(planFixtureTaskKind);
export const fixtureExtensions = [defineExtension({ name: "hush-faux-plan-execution", tasks: [executeFixture] }), worker];
// Keep the task kind registered even without file execution so a restart can
// settle an interrupted Run without authorizing a write.
export const scopedRecovery = defineExtension({ name: "hush-plan-scoped-execution", tasks: [coordinator(scopedTask)] });

export function scopedPlanExecution(files: PlanFiles, acceptance: PlanAcceptance, selectModel: (agent: Readonly<AgentState>, ctx: Context) => Promise<PlanRunModel>) {
  const task = coordinator(scopedTask, files, acceptance);
  return { files, selectModel, task, extension: defineExtension({ name: "hush-plan-scoped-execution", tasks: [task] }) };
}
type ScopedExecution = ReturnType<typeof scopedPlanExecution>;

const ChatInput = defineEntry<Pin & { text: string; timestampMs: number }>(planChatKind);

async function chatAssignment(tx: Tx, owner: ConversationId) {
  const live = (await tx.doc(LiveDoc, owner)).run;
  if (live?.first === undefined) return;
  const entry = await tx.entry(ChatInput, live.first);
  if (!entry || entry.conversationId !== owner) return;
  const { planId, revision } = entry.data;
  const state = await tx.doc(State, planId, null), run = state.run;
  if (state.revision === revision && run?.chat && run.state === "running" && run.ownerThreadId === String(owner) &&
      live.inputs.includes(Number(run.submissionId) as SubmissionId)) return { planId, run };
}

export const planReportName = "hush_plan_report";
export const planReportTool = defineTool({
  name: planReportName,
  description: "Report the outcome and checks or blocker for every selected task of this Run. This is an advisory report, not verification.",
  callers: ["model"], replay: "safe",
  parameters: { type: "object", required: ["tasks"], additionalProperties: false, properties: { tasks: {
    type: "array", minItems: 1, maxItems: planStepLimit, items: {
      type: "object", required: ["stepId", "state", "detail"], additionalProperties: false, properties: {
        stepId: { type: "string", maxLength: 64 }, state: { type: "string", enum: ["done", "blocked", "incomplete"] },
        detail: { type: "string", minLength: 1, maxLength: 180, pattern: "\\S" },
      },
    },
  } } } as const,
  async execute({ tasks }: { tasks: PlanTaskReport[] }, api, ctx) {
    return api.commit(async tx => {
      const assignment = await chatAssignment(tx, api.conversationId);
      if (!assignment) return { ...result("No active plan assignment to report on."), isError: true };
      const { planId, run } = assignment;
      if (tasks.length !== run.stepIds.length || new Set(tasks.map(t => t.stepId)).size !== tasks.length ||
          tasks.some(t => !run.stepIds.includes(t.stepId) || !t.detail.isWellFormed() || /[\p{Cc}\p{Zl}\p{Zp}]/u.test(t.detail))) {
        return { ...result("Report exactly one outcome per selected task, with a single-line detail."), isError: true };
      }
      const record = await logRun(tx, planId, run.id);
      if (!record) throw new WireError("internal");
      record.reports = run.stepIds.map(id => ({ ...tasks.find(t => t.stepId === id)! }));
      return result("Report recorded. It is not an independent verification.");
    }, ctx);
  },
});

export async function hasChatAssignment(h: Harness, owner: ConversationId, ctx: Context) {
  return h.commit(async tx => !!await chatAssignment(tx, owner), ctx);
}

export function planChatExecution(harness: () => Harness, authorize: (cwd: string | undefined, ctx: Context) => Promise<string>,
    requireModel: NativeHost["requireModel"], agent: AgentChange) {
  const task = defineTask<Pin & { submission: SubmissionId }, { phase: "settle" }, null>({
    name: "hush.plan-chat-tracker", version: 1, initial: () => ({ phase: "settle" }),
    phases: { settle: async (task, runtime, ctx) => {
      // Observe the one native submission, including queued withdrawal. This
      // observer never invokes a model, owns a conversation or resubmits input.
      const { planId, revision, submission } = task.input;
      const wait = withAbortSignal(runtime.signal, ctx);
      const settled = await (await harness().submission(submission, wait))?.wait(wait);
      return runtime.commit(async tx => {
        const state = await tx.doc(State, planId, null);
        if (state.revision !== revision || state.run?.id !== String(task.id)) return ended;
        const row = await logRun(tx, planId, state.run.id);
        if (row && settled?.entry !== undefined) row.entryId = String(settled.entry);
        if (settled?.status !== "done" || settled.conversationId !== runtime.conversationId) {
          if (state.halt && settled?.entry === undefined) return finishRun(tx, planId, state, "closed");
          return failRun(tx, planId, state, settled?.entry === undefined
            ? "Plan input did not start." : "Plan turn did not finish. Inspect any changes before retrying.");
        }
        const reports = row?.reports ?? [];
        state.reports = [...state.reports?.filter(r => !state.run!.stepIds.includes(r.stepId)) ?? [], ...reports];
        return finishRun(tx, planId, state, "done");
      }, ctx);
    } },
    abort: (task, runtime, ctx) => runtime.commit(async tx => {
      const state = await tx.doc(State, task.input.planId, null);
      if (state.run?.id === String(task.id)) await failRun(tx, task.input.planId, state, "Run tracking stopped. Check the initiating thread before retrying.");
      return aborted;
    }, ctx),
  });
  return { task, authorize, requireModel, agent, extension: defineExtension({ name: "hush-plan-chat", tasks: [task] }) };
}
type ChatExecution = ReturnType<typeof planChatExecution>;

export async function checkPlanWorktree(read: DocumentReader, owner: ConversationId, cwd: string, ctx: Context) {
  const launch = (await read.snapshot(WorktreeLaunch, owner, ctx))?.allocation;
  if (!launch) return;
  if (!launch.identity) throw incompleteWorktree(String(owner));
  if (cwd !== launch.cwd || await checkoutIdentity(cwd) !== launch.identity.directory) {
    throw invalid("The prepared worktree changed. Review the checkout before retrying.");
  }
}

// The caller has already bound the physical directory and can compare the
// plan's immutable allocation without a nested Harness snapshot/commit.
export async function matchesPlanCheckout(tx: Tx, owner: ConversationId, cwd: string, directory: string) {
  const { allocation } = await tx.doc(WorktreeLaunch, owner);
  return !allocation || allocation.cwd === cwd && allocation.identity?.directory === directory;
}

export async function isPublicPlanFixture(tx: Tx, child: ConversationId) {
  return Array.isArray((await tx.doc(Work, child)).checks);
}

export async function isPlanWorker(h: Harness, owner: ConversationId, ctx: Context) {
  // Ownership is immutable. Do not grant authority even if the worker's
  // extension, assignment or invocation-private profile changed on reopen.
  return h.commit(async tx => !!(await tx.conversation(owner))?.owner, ctx);
}

// Wait on one address until its projected cursor changes.
async function observed<T>(attach: (observer: Observer<Readonly<T>>) => Promise<Observation<Readonly<T>> | undefined>,
    token: (value: Readonly<T>) => string, after: string | undefined, signal: AbortSignal,
    settle?: (value: Readonly<T>, signal: AbortSignal) => Promise<Readonly<T>> | undefined): Promise<Readonly<T>> {
  const change = Promise.withResolvers<Readonly<T> | undefined>();
  const watching = await attach({
    advance: call => {
      if (call.kind !== "value") change.resolve(undefined);
      else if (token(call.value) !== after) change.resolve(call.value);
    },
    end: () => change.resolve(undefined),
  });
  if (!watching) throw new WireError("internal");
  const stop = new AbortController();
  const abort = () => change.resolve(undefined);
  signal.addEventListener("abort", abort, { once: true });
  try {
    signal.throwIfAborted();
    if (!after || token(watching.value) !== after) return watching.value;
    const terminal = settle?.(watching.value, AbortSignal.any([signal, stop.signal]));
    const value = await (terminal ? Promise.race([change.promise, terminal]) : change.promise);
    signal.throwIfAborted();
    if (value === undefined) throw new WireError("internal");
    return value;
  } finally {
    stop.abort();
    signal.removeEventListener("abort", abort);
    watching.detach();
  }
}

function checkFileClaims<T extends { writable: boolean }>(claims: readonly T[], key: (entry: T) => string) {
  const seen = new Map<string, boolean>();
  for (const claim of claims) {
    const id = key(claim), previous = seen.get(id);
    if (previous !== undefined && (previous || claim.writable)) throw invalid("Selected tasks have overlapping file claims. Run them separately.");
    seen.set(id, claim.writable);
  }
}

export class Plans implements PlanHost, PlanChatHost {
  #catalog?: Promise<void>;
  #allWorkspaces?: PlanCatalog;
  #preparing = new Set<string>();
  readonly worktrees?: PlanWorktreeHost;
  constructor(private readonly harness: Harness, private readonly storage: Storage,
    private readonly directory: (cwd?: string) => string, private readonly fixture: boolean, private readonly scoped?: ScopedExecution,
    private readonly chat?: ChatExecution, worktree?: WorktreeOptions) {
    if (chat && worktree) this.worktrees = {
      prepare: (selection, signal) => this.#prepareWorktree(worktree, chat, selection, signal),
      start: (selection, signal) => this.#runInChat(selection, signal, { path: selection.checkout, validate: worktree.validateSelection }),
    };
  }

  async #definition(pin: Pin, ctx: Context) {
    const plan = (await this.harness.snapshot(Definition, revisionKey(pin.planId, pin.revision), ctx))?.plan;
    if (!plan) throw new WireError("internal");
    return plan;
  }

  async #threadWorkspace(tx: Tx, owner: ConversationId) {
    await checkThread(tx, owner);
    return this.directory((await tx.doc(AgentDoc, owner)).cwd);
  }

  async #workspacePlan(tx: Tx, cwd: string, planId: string, browse = false) {
    const state = await tx.doc(State, planId, null);
    const foreign = state.workspace !== cwd;
    if (!state.revision || foreign && !browse) throw new WireError("not_found");
    return { state, workspace: foreign ? state.workspace : undefined };
  }

  async #locate(tx: Tx, q: PlanAttachment, browse: boolean) {
    return this.#workspacePlan(tx, await this.#threadWorkspace(tx, thread(q.threadId)), q.planId, browse);
  }

  async #inWorkspace(tx: Tx, q: PlanAttachment) { return (await this.#locate(tx, q, false)).state; }

  async reviewTarget(tx: Tx, q: PlanReviewTarget) {
    const state = await this.#inWorkspace(tx, q);
    if (state.fixture || q.revision > state.revision) throw new WireError("not_found");
    return { ...await readPlanRevision(tx, q), headRevision: state.revision, cursor: state.cursor, cwd: state.workspace };
  }

  async waitReviewChange(planId: string, after: string, signal: AbortSignal, settled?: (signal: AbortSignal) => Promise<void>) {
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    await observed<PlanState>(observer => this.harness.observeDoc(State, planId, observer, ctx), state => state.cursor, after, signal,
      settled && ((state, abort) => settled(abort).then(() => state)));
  }

  // Upgrade earlier stores (including summaries without active run order) once
  // without opening conversations or repinning forks. This is off the startup path.
  async #indexExistingPlans() {
    const h = this.harness, ctx = BACKGROUND_CONTEXT;
    if ((await h.snapshot(CatalogReady, ctx))?.runOrder) return;
    let continuation: Cursor | undefined;
    do {
      const page = await this.storage.scanDocuments({ scope: { kind: "session" }, at: "current", kind: State.definition.kind }, 32, continuation, ctx);
      for (const row of page.items) if (row.key) await h.commit(async tx => {
        const state = await tx.doc(State, row.key!, null);
        if (state.revision) await refreshCatalog(tx, row.key!, state);
      }, ctx);
      continuation = page.next;
    } while (continuation);
    await h.commit(async tx => { Object.assign(await tx.doc(CatalogReady), { ready: true, runOrder: true }); }, ctx);
  }

  async workspace(q: PlanWorkspaceQuery, signal: AbortSignal, chat = false) {
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const cwd = await this.harness.commit(tx => this.#threadWorkspace(tx, thread(q.threadId)), ctx);
    return this.#workspacePage(cwd, q, signal, chat, q.threadId);
  }

  async workspaceAt(q: PlanWorkspaceAt, signal: AbortSignal, browse = false) {
    const cwd = await this.#launchDirectory(q.cwd, signal);
    const page = await (browse ? this.#catalogPage(cwd, q, signal, true) : this.#workspacePage(cwd, q, signal, true));
    await this.#launchDirectory(cwd, signal); // A wait may outlive the grant.
    return page;
  }

  async inspectAt(q: PlanInspectAt, signal: AbortSignal, browse = false) {
    const cwd = await this.#launchDirectory(q.cwd, signal);
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const workspace = await this.harness.commit(async tx => (await this.#workspacePlan(tx, cwd, q.planId, browse)).workspace, ctx);
    const snapshot = await this.#watchPlan(q.planId, undefined, q.after, signal, true, true, workspace);
    await this.#launchDirectory(cwd, signal);
    return snapshot;
  }

  async #launchDirectory(path: string, signal: AbortSignal) {
    if (!this.chat) throw new WireError("unsupported_capability", "planChat");
    const cwd = this.directory(path);
    await this.chat.authorize(cwd, withAbortSignal(signal, BACKGROUND_CONTEXT));
    return cwd;
  }

  async #workspacePage(cwd: string, q: Omit<PlanWorkspaceQuery, "threadId">, signal: AbortSignal, chat: boolean, threadId?: string) {
    const h = this.harness, ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    await this.#ensureCatalog(signal);
    if (!(await h.snapshot(WorkspaceCatalog, cwd, ctx))) await h.commit(tx => tx.doc(WorkspaceCatalog, cwd, null), ctx);
    const token = (catalog: Readonly<WorkspacePlans>) => createHash("sha256").update(JSON.stringify([catalog.cursor, q.page, chat, threadId])).digest("hex");
    const catalog = await observed(observer => h.observeDoc(WorkspaceCatalog, cwd, observer, ctx), token, q.after, signal);
    return { revision: token(catalog), ...planInventoryPage([[undefined, catalog.rows]], q.page, chat, threadId) };
  }

  #ensureCatalog(signal: AbortSignal) {
    return observe(this.#catalog ??= this.#indexExistingPlans().catch(error => { this.#catalog = undefined; throw error; }), signal);
  }

  async catalog(q: PlanWorkspaceQuery, signal: AbortSignal, chat = false) {
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const cwd = await this.harness.commit(tx => this.#threadWorkspace(tx, thread(q.threadId)), ctx);
    return this.#catalogPage(cwd, q, signal, chat, q.threadId);
  }

  async #catalogPage(cwd: string, q: Omit<PlanWorkspaceQuery, "threadId">, signal: AbortSignal, chat: boolean, threadId?: string) {
    await this.#ensureCatalog(signal);
    const { kind, version } = WorkspaceCatalog.definition;
    const index = this.#allWorkspaces ??= new PlanCatalog(this.harness, this.storage, kind, version);
    return index.page(cwd, q.page, q.after, signal, chat, threadId);
  }

  async history(q: PlanHistoryQuery, signal: AbortSignal, chat = false, browse = false, worktrees = false) {
    const h = this.harness, ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    // Scope and the page are observed in the same transaction.
    return h.commit(async tx => {
      await this.#locate(tx, q, browse);
      const log = await tx.doc(RunLog, q.planId, null);
      const visible = chat ? log.runs : log.runs.filter(r => !r.chat);
      const end = q.before === undefined ? visible.length : visible.findIndex(r => r.id === q.before);
      if (end < 0) throw new WireError("invalid_request");
      const start = Math.max(0, end - planHistoryPageSize);
      const runs = visible.slice(start, end).reverse().map(({ reports, worktree, ...r }) => ({ ...r, stepIds: r.stepIds.slice(),
        workers: r.workers.map(w => ({ ...w })), ...(r.turns && { turns: r.turns.map(t => ({ ...t })) }),
        ...(worktrees && worktree && { worktree }), ...(r.state === "done" && reports && { reports: reports.map(t => ({ ...t })) }) }));
      return { runs, next: start > 0 ? runs.at(-1)!.id : null };
    }, ctx);
  }

  async setClosed(q: PlanClosure, signal: AbortSignal) {
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    await this.harness.commit(async tx => {
      const state = await this.#inWorkspace(tx, q);
      const plan = await readDefinition(tx, { planId: q.planId, revision: state.revision });
      if (!q.closed && verifiedCount(state) === plan.steps.length) throw invalid("Completed plans cannot be reopened.");
      if (!!state.closed === q.closed) return;
      if (q.closed) {
        state.closed = true;
        if (state.run?.state === "running") {
          state.halt = true;
          if (state.run.chat) {
            const owner = thread(state.run.ownerThreadId!);
            const id = Number(state.run.submissionId) as SubmissionId;
            const submission = await tx.submission(id);
            if (submission?.status === "queued" && submission.conversationId === owner) {
              const inbox = (await tx.doc(InboxDoc, owner)).items;
              const i = inbox.findIndex(item => item.id === id && item.mode === "followUp");
              if (i >= 0) {
                inbox.splice(i, 1);
                tx.settleSubmission(id, { status: "unanswered", reason: "aborted" });
              }
            }
          }
        }
      } else {
        // Reopening is not renewal of the previous Run's authorisation.
        delete state.closed;
      }
      state.lifecycle = cursor();
      await update(tx, q.planId, state, plan);
    }, ctx);
    return {};
  }

  async wait(id: string, after: string | undefined, signal: AbortSignal, named?: string, chat = false, browse = false): Promise<PlanSnapshot> {
    const owner = thread(id), h = this.harness, ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    let binding = await h.snapshot(Binding, owner, ctx);
    let workspace: string | undefined;
    if (named) {
      workspace = await h.commit(async tx => (await this.#locate(tx, { threadId: id, planId: named }, browse)).workspace, ctx);
    } else {
      if (!binding || binding.pin?.liveThread !== owner) {
        if (!binding) await h.commit(async tx => { await checkThread(tx, owner); await tx.doc(Binding, owner); }, ctx);
        binding = await observed(observer => h.observeDoc(Binding, owner, observer, ctx), b => b.cursor, after, signal);
      }
      const { pin } = binding;
      if (!pin || pin.liveThread !== owner) return { revision: binding.cursor,
        plan: pin ? await this.#definition(pin, ctx) : null, runAvailability: pin ? "pinned" : this.fixture ? "fixture" : "disabled",
        progress: [], run: null };
    }
    return this.#watchPlan(named ?? binding!.pin!.planId, owner, after, signal, !!named, chat, workspace);
  }

  async #watchPlan(planId: string, owner: ConversationId | undefined, after: string | undefined, signal: AbortSignal, named: boolean, chat: boolean, workspace?: string): Promise<PlanSnapshot> {
    const h = this.harness, ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const compatible = (state: Readonly<PlanState>) => state.fixture === this.fixture;
    const showChatFields = (state: Readonly<PlanState>) => chat && !!this.chat && compatible(state) && !state.fixture;
    const availability = (state: Readonly<PlanState>) => workspace !== undefined || !compatible(state) ? "disabled"
      : owner === undefined || !state.members.includes(owner) ? "unbound" : state.fixture ? "fixture" : this.scoped ? "scoped" : "disabled";
    const token = (state: Readonly<PlanState>) => createHash("sha256").update(`${state.cursor}:${availability(state)}:${showChatFields(state)}:${workspace ?? ""}`).digest("hex");
    const settle = (state: Readonly<PlanState>, abort: AbortSignal) => {
      if (workspace !== undefined || state.run?.state !== "running" || !compatible(state)) return;
      return this.#settleRun(planId, state.run.id, abort).then(async () => await h.snapshot(State, planId, ctx) ?? state);
    };
    const state = await observed(observer => h.observeDoc(State, planId, observer, ctx), token, after, signal, settle);
    if (!named && owner !== undefined && !state.members.includes(owner)) return this.wait(String(owner), undefined, signal, undefined, chat);
    const runAvailability = availability(state);
    const plan = await this.#definition({ planId, revision: state.revision }, ctx);
    const showChat = showChatFields(state);
    const showProgress = showChat || compatible(state) && (state.fixture || !!this.scoped);
    const run = showProgress && (!state.run?.chat || showChat) ? state.run ?? null : null;
    return { revision: token(state), plan, runAvailability, progress: showProgress ? state.progress : [], run,
      ...(state.closed && { closed: true as const }), ...(run && state.halt && { settling: true as const }),
      ...(workspace !== undefined && { workspace }), ...(showChat && { reports: state.reports ?? [] }),
      ...(showChat && workspace === undefined && { chat: true as const }),
      ...(verifiedCount(state) === plan.steps.length && { complete: true as const }) };
  }

  // An unexpected task fault may settle without its abort callback. Both
  // watches race the task's receipt against doc updates, and detach on EOF.
  async #settleRun(planId: string, runId: string, signal: AbortSignal) {
    const h = this.harness, ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    await h.waitForTask(Number(runId) as TaskId, ctx);
    await h.commit(async tx => {
      const head = await tx.doc(State, planId, null);
      if (head.run?.id === runId && head.run.state === "running") {
        await failRun(tx, planId, head, "Run interrupted. Inspect changes before retrying.");
      }
    }, ctx);
  }

  async activity(id: string, after: string | undefined, signal: AbortSignal): Promise<PlanSnapshot> {
    const h = this.harness, owner = thread(id), ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    if (!(await h.snapshot(ChatActivity, owner, ctx))) await h.commit(async tx => {
      await checkThread(tx, owner);
      await tx.doc(ChatActivity, owner);
    }, ctx);
    await observed<ChatActivity>(observer => h.observeDoc(ChatActivity, owner, observer, ctx), a => a.cursor, after, signal,
      (a, abort) => {
        const ref = a.pending[0];
        if (ref) return this.#settleRun(ref.planId, ref.runId, abort).then(() => a);
      });
    const { cursor, view } = await h.commit(async tx => {
      const { cursor, pending, last } = await tx.doc(ChatActivity, owner);
      const ref = pending[0];
      if (!ref) return { cursor, view: last && chatRunView(last, last.run, last) };
      const state = await tx.doc(State, ref.planId, null);
      if (state.revision !== ref.revision || state.run?.id !== ref.runId) throw new WireError("internal");
      return { cursor, view: chatRunView(ref, state.run, state) };
    }, ctx);
    const base = { revision: cursor, runAvailability: "disabled" as const };
    if (!view) return { ...base, plan: null, progress: [], run: null };
    const { planId, revision, ...activity } = view;
    return { ...base, ...activity, plan: await this.#definition({ planId, revision }, ctx), chat: true as const };
  }

  async attach(q: PlanAttachment, signal: AbortSignal, replace = false) {
    const owner = thread(q.threadId), ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    await this.harness.commit(async tx => {
      const state = await this.#inWorkspace(tx, q);
      const binding = await tx.doc(Binding, owner), pin = binding.pin;
      if (pin?.planId === q.planId && pin.liveThread === owner) return;
      if (pin && pin.planId !== q.planId && !replace) throw invalid("This thread already has a plan.");
      if (state.members.length >= bindingLimit) throw new WireError("limit");
      if (pin && pin.liveThread === owner) await unbind(tx, owner, pin, await tx.doc(State, pin.planId, null));
      state.members.push(owner);
      setBinding(binding, q.planId, state.revision, owner);
      await mark(tx, owner, { planId: q.planId, revision: state.revision });
    }, ctx);
    return {};
  }

  // A durable reservation owns every Git write. Retrying the exact request
  // returns the same checkout (or identifies its incomplete reservation).
  async #prepareWorktree(options: WorktreeOptions, chat: ChatExecution, q: PlanWorktreeSource, signal: AbortSignal): Promise<PlanWorktreeCheckout> {
    const h = this.harness, ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const owner = "threadId" in q ? thread(q.threadId) : undefined, launch = "cwd" in q ? q : undefined;
    const sourceCwd = (tx: Tx) => owner === undefined ? this.directory(launch?.cwd) : this.#threadWorkspace(tx, owner);
    const key = createHash("sha256").update(JSON.stringify([q.planId, q.requestId])).digest("hex");
    const fingerprint = createHash("sha256").update(JSON.stringify([owner, launch?.cwd, launch?.model?.provider,
      launch?.model?.modelId, launch?.thinkingLevel, q.revision, q.stepIds])).digest("hex");
    if (this.#preparing.has(key)) throw new WireError("busy");
    this.#preparing.add(key);
    let target: PlanWorktreeCheckout | undefined;
    try {
      const source = await h.commit(async tx => {
        const receipt = (await tx.doc(WorktreePreparation, key, null)).receipt;
        if (receipt) {
          if (receipt.source !== fingerprint) throw invalid("Worktree request ID already belongs to a different selection or source.");
          if (!receipt.ready) throw incompleteWorktree(receipt.target.threadId);
          return { target: { ...receipt.target, checkout: { ...receipt.target.checkout } } } as const;
        }
        const cwd = await sourceCwd(tx), { state } = await this.#workspacePlan(tx, cwd, q.planId);
        const { plan, steps } = await this.#chatSelection(tx, state, q);
        const agent = owner === undefined ? undefined : await tx.doc(AgentDoc, owner);
        return { cwd, lifecycle: state.lifecycle, title: plan.title, tasks: steps.map(s => s.title),
          model: agent?.model && { ...agent.model }, thinkingLevel: agent?.thinkingLevel };
      }, ctx);
      if (source.target !== undefined) return source.target;
      const defaults = await options.defaults(source.cwd, ctx);
      const model = launch?.model ?? source.model ?? defaults.model;
      const thinkingLevel = launch?.thinkingLevel ?? source.thinkingLevel ?? defaults.thinkingLevel;
      let label: string | undefined;
      const checkout = await options.checkout.create(source.cwd, signal, {
        name: async signal => {
          try {
            await chat.requireModel?.(model ?? undefined);
            signal.throwIfAborted();
            if (model) label = await options.name(model, source.title, source.tasks, signal);
          } catch { signal.throwIfAborted(); } // Unavailable provider or unusable name must not block preparation.
          return { preferred: label, fallback: source.title };
        },
        reserve: async checkout => {
          const title = threadLabel(label ?? source.title, 96) || "Plan worktree";
          const { cwd, root, branch } = checkout;
          const prepared = { cwd, root, branch };
          const threadId = await h.commit(async tx => {
            const { state } = await this.#workspacePlan(tx, await sourceCwd(tx), q.planId);
            if (state.workspace !== source.cwd || state.lifecycle !== source.lifecycle) {
              throw invalid("The plan workspace or lifecycle changed; select the tasks again.");
            }
            await this.#chatSelection(tx, state, q);
            const record = await tx.doc(WorktreePreparation, key, null);
            if (record.receipt) throw new WireError("busy");
            const { id } = await tx.createConversation({ ownership: { kind: "ownerless" } });
            await configure(tx, id, { ...defaults, cwd, model, thinkingLevel });
            const doc = await tx.doc(ThreadPresentationDoc, id);
            doc.title = title;
            doc.checkout = { cwd, branch, repository: checkout.repository };
            (await tx.doc(WorktreeLaunch, id)).allocation = { ...checkout, planId: q.planId, revision: q.revision,
              stepIds: q.stepIds.slice(), requestId: q.requestId, ...(source.lifecycle !== undefined && { lifecycle: source.lifecycle }) };
            record.receipt = { source: fingerprint, target: { threadId: String(id), title, checkout: prepared } };
            return String(id);
          }, ctx);
          target = { threadId, title, checkout: prepared };
        },
      });
      if (!target) throw new WireError("internal");
      const id = thread(target.threadId);
      // A lost peer must not strand a completed Git checkout as incomplete.
      await h.commit(async tx => {
        const launch = (await tx.doc(WorktreeLaunch, id)).allocation;
        const { receipt } = await tx.doc(WorktreePreparation, key, null);
        if (!launch || !receipt || receipt.target.threadId !== String(id)) throw new WireError("internal");
        launch.identity = checkout.identity;
        receipt.ready = true;
      }, withAbortSignal(AbortSignal.timeout(5_000), BACKGROUND_CONTEXT));
      return target;
    } catch (error) {
      if (target) throw incompleteWorktree(target.threadId);
      throw error;
    } finally { this.#preparing.delete(key); }
  }

  async #chatSelection(tx: Tx, state: PlanState, q: WorktreeSelection) {
    if (state.fixture) throw new WireError("unsupported_capability", "planChat");
    if (state.closed) throw invalid("This plan is closed. Reopen it before running tasks.");
    if (state.revision !== q.revision) throw invalid("Plan changed; review and select the current revision.");
    if (state.run?.state === "running") throw new WireError("busy");
    const last = state.run?.id ?? state.previousTask;
    if (last) {
      const task = await tx.task(Number(last) as TaskId);
      if (!task || task.state.status !== "terminal") throw new WireError("busy");
    }
    const plan = await readDefinition(tx, q);
    const steps = plan.steps.filter(step => q.stepIds.includes(step.id));
    if (steps.length !== q.stepIds.length) throw invalid("Selection includes an unknown task.");
    const status = (id: string) => state.progress.some(p => p.stepId === id && p.state === "verified") ? "verified"
      : state.reports?.find(r => r.stepId === id)?.state;
    if (steps.some(s => status(s.id) === "verified")) throw invalid("Selection includes an already verified task.");
    return { plan, steps, status };
  }

  runInChat(q: PlanSelection, signal: AbortSignal) { return this.#runInChat(q, signal); }

  async #runInChat(q: PlanSelection, signal: AbortSignal, worktree?: { path: string; validate: WorktreeOptions["validateSelection"] }) {
    const chat = this.chat;
    if (!chat || this.fixture) throw new WireError("unsupported_capability", "planChat");
    const h = this.harness, owner = thread(q.threadId), ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const target = await h.conversation(owner, ctx);
    if (!target) throw new WireError("not_found");
    const inspect = async (tx: Tx) => {
      const cwd = await this.#threadWorkspace(tx, owner);
      let state: PlanState, launch: WorktreeAllocation | undefined;
      if (worktree) {
        const allocation = (await tx.doc(WorktreeLaunch, owner)).allocation;
        if (!allocation || allocation.cwd !== worktree.path || allocation.planId !== q.planId ||
            allocation.revision !== q.revision || allocation.requestId !== q.requestId ||
            allocation.stepIds.length !== q.stepIds.length || allocation.stepIds.some((id, i) => id !== q.stepIds[i])) {
          throw new WireError("not_found");
        }
        if (!allocation.identity) throw incompleteWorktree(q.threadId);
        launch = allocation;
        state = (await this.#workspacePlan(tx, cwd, q.planId, true)).state;
      } else state = (await this.#workspacePlan(tx, cwd, q.planId)).state;
      if (state.fixture) throw new WireError("unsupported_capability", "planChat");
      const receipts = (await tx.doc(Receipts, owner)).records;
      const receipt = runReceipt(receipts, q, true);
      if (receipt) return { receipt } as const;
      if (launch && launch.cwd !== cwd) throw new WireError("not_found");
      if (launch && state.lifecycle !== launch.lifecycle) throw invalid("The plan was closed or reopened; select the tasks again.");
      if (await foregroundPlan(tx, owner)) throw new WireError("busy");
      if (launch && ((await tx.doc(LiveDoc, owner)).run || (await tx.doc(InboxDoc, owner)).items.length)) throw new WireError("busy");
      if (receipts.length >= receiptLimit) throw new WireError("limit");
      return { state, ...await this.#chatSelection(tx, state, q), receipts, agent: await tx.doc(AgentDoc, owner), launch };
    };
    const before = await h.commit(async tx => {
      const inspected = await inspect(tx);
      if (inspected.receipt !== undefined) return inspected;
      const { cwd, model, thinkingLevel } = inspected.agent, launch = inspected.launch;
      const checkout = launch && { cwd: launch.cwd, root: launch.root, branch: launch.branch, head: launch.head,
        repository: launch.repository, identity: { ...launch.identity! } };
      return { cwd, model: model && { provider: model.provider, modelId: model.modelId }, thinkingLevel,
        lifecycle: inspected.state.lifecycle, launch: checkout };
    }, ctx);
    if ("receipt" in before) return { runId: before.receipt };
    const require = worktree ? (ref: AgentState["model"]) => worktree.validate(ref, before.thinkingLevel ?? "off") : chat.requireModel;
    const verify = await checkPreparedModel(require, before.model, chat.agent);
    let failure: WireError | undefined;
    try {
      const authorized = await chat.authorize(before.cwd, ctx);
      if (before.launch) {
        const identity = authorized === before.launch.cwd && await planWorktreeIdentity(before.launch, signal);
        if (!identity || identity.git !== before.launch.identity?.git || identity.directory !== before.launch.identity.directory) {
          throw invalid("The prepared worktree changed. Review the checkout before retrying.");
        }
      }
    } catch (error) {
      if (!worktree || !(error instanceof WireError)) throw error;
      failure = error; // Prefer a concurrently committed receipt to a stale preflight error.
    }
    const runId = await h.commit(async tx => {
      const inspected = await inspect(tx);
      if (inspected.receipt !== undefined) return inspected.receipt;
      const { state, plan, steps, status, receipts, agent } = inspected;
      if (failure) throw failure;
      if (agent.cwd !== before.cwd || worktree && agent.thinkingLevel !== before.thinkingLevel) throw new WireError("busy");
      if (state.lifecycle !== before.lifecycle) throw invalid("The plan was closed or reopened; select the tasks again.");
      verify?.(agent.model);
      return this.#admitChat(tx, chat, target, q, state, plan, steps, status, receipts, !!worktree);
    }, ctx);
    h.resume();
    return { runId };
  }

  async #admitChat(tx: Tx, chat: ChatExecution, target: Conversation, q: PlanSelection, state: PlanState, plan: PlanDefinition,
      steps: PlanStep[], status: (id: string) => string | undefined, receipts: { requestId: string; receipt: Receipt }[], worktree: boolean) {
    // The selected revision is input data, not a replay of the authoring prompt.
    // Normal tools apply; none of the scoped file scripts or grants are passed.
    const owner = target.id, stepIds = steps.map(s => s.id);
    const content = "Plan implementation\n" + JSON.stringify({ title: plan.title, revision: plan.revision, selected: stepIds,
      tasks: plan.steps.map(({ id, title, description, checks, requires, after, blocker }) =>
        ({ id, title, description, checks, requires, after, blocker, status: status(id) })) }) +
      "\nImplement only the selected outcomes in this turn. Use the other tasks and their dependencies as context, not assignments. " +
      "Inspect what already exists, do setup necessary for the selected work, and run relevant checks. If a real blocker remains, " +
      "continue with independent selected tasks. Do not launch background workers for this run. " +
      `Call ${planReportName} with the outcome and checks or blocker for each selected task, then summarize. ` +
      "Treat this assignment as a record in later turns, not a standing request." + (worktree ?
        " Work in this checkout only. Do not change other checkouts or merge the work back." : "");
    await configure(tx, owner, chat.agent);
    const submission = await target.submit(tx, { type: "input", whenBusy: "followUp", content,
      entry: { kind: planChatKind, data: { planId: q.planId, revision: q.revision, timestampMs: Date.now(),
        text: planRunHeading(plan, stepIds.length) } } });
    const tracker = await tx.createTask(chat.task, { planId: q.planId, revision: q.revision, submission },
      { ownership: { kind: "conversation" }, conversationId: owner, background: true });
    const id = String(tracker);
    delete state.halt;
    state.run = { id, stepIds, state: "running", chat: true, ownerThreadId: q.threadId, submissionId: String(submission) };
    const activity = await tx.doc(ChatActivity, owner);
    activity.pending.push({ planId: q.planId, revision: q.revision, runId: id });
    activity.cursor = cursor();
    (await tx.doc(RunLog, q.planId, null)).runs.push({ id, revision: q.revision, threadId: q.threadId, stepIds, state: "running", chat: true,
      ...(worktree && { worktree: true as const }), workers: [] });
    receipts.push({ requestId: q.requestId, receipt: { runId: id, chat: true, planId: q.planId, revision: q.revision, stepIds: q.stepIds } });
    await update(tx, q.planId, state, plan);
    return id;
  }

  async run(q: PlanSelection, signal: AbortSignal) {
    const h = this.harness, owner = thread(q.threadId), ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const receipt = (records: { requestId: string; receipt: Receipt }[]) => runReceipt(records, q);
    const inspect = async (tx: Tx) => {
      await checkThread(tx, owner);
      const { pin } = await tx.doc(Binding, owner);
      if (!pin || pin.planId !== q.planId) throw invalid("Plan changed; review and select the current revision.");
      if (pin.liveThread !== owner) throw invalid("A pinned revision cannot run. Attach to the live plan first.");
      const state = await tx.doc(State, q.planId, null);
      if (state.fixture !== this.fixture || !state.fixture && !this.scoped) throw new WireError("unsupported_capability", "plan");
      const receipts = (await tx.doc(Receipts, owner)).records;
      const previous = receipt(receipts);
      if (previous !== undefined) return { runId: previous } as const;
      if (state.closed) throw invalid("This plan is closed. Reopen it before running tasks.");
      if (state.revision !== q.revision) throw invalid("Plan changed; review and select the current revision.");
      const plan = await readDefinition(tx, pin);
      const last = state.run?.id ?? state.previousTask;
      if (last) {
        const task = await tx.task(Number(last) as TaskId);
        if (!task || task.state.status !== "terminal") throw new WireError("busy");
        if (state.run?.state === "running") await failRun(tx, q.planId, state, "Run interrupted. Inspect changes before retrying.");
      }
      if (receipts.length >= receiptLimit) throw new WireError("limit");
      const selected = new Set(q.stepIds), steps = plan.steps.filter(step => selected.has(step.id));
      if (steps.length !== selected.size) throw new WireError("invalid_request");
      const verified = new Set(state.progress.filter(p => p.state === "verified").map(p => p.stepId));
      for (const step of steps) {
        if (verified.has(step.id)) throw invalid("Selection includes an already verified step.");
        if (step.blocker) throw invalid("A selected step is blocked.");
        const unfinished = (id: string) => !verified.has(id) && !selected.has(id);
        if (step.requires?.some(unfinished) || step.after && unfinished(step.after)) throw invalid("Selection is missing an unfinished prerequisite.");
      }
      return { state, receipts, steps, plan };
    };
    const selectedModel = async (tx: Tx) => {
      const agent = await tx.doc(AgentDoc, owner);
      return { ...(agent.model && { model: { provider: agent.model.provider, modelId: agent.model.modelId } }),
        cwd: this.directory(agent.cwd), thinkingLevel: agent.thinkingLevel ?? "off" };
    };
    let scoped: ScopedRun | undefined, lifecycle: string | undefined;
    if (!this.fixture && this.scoped) {
      const preview = await h.commit(async tx => {
        const admitted = await inspect(tx);
        if (admitted.runId !== undefined) return admitted;
        const agent = await selectedModel(tx);
        if (admitted.state.workspace !== agent.cwd || (await tx.doc(LiveDoc, owner)).run ||
            (await tx.doc(InboxDoc, owner)).items.length || await foregroundPlan(tx, owner)) throw new WireError("busy");
        return { agent, lifecycle: admitted.state.lifecycle, steps: admitted.steps.map(s => ({
          id: s.id, ...(s.edit && { edit: { ...s.edit } }),
          ...(s.scope && { scope: { write: s.scope.write.slice(), read: s.scope.read?.slice() ?? [], acceptance: { ...s.scope.acceptance } } }),
        })) };
      }, ctx);
      if ("runId" in preview) return { runId: preview.runId };
      lifecycle = preview.lifecycle;
      const claims = preview.steps.flatMap(step => {
        if (step.scope) return [...step.scope.write.map(path => ({ path, writable: true })), ...step.scope.read.map(path => ({ path, writable: false }))];
        if (step.edit) return [{ path: step.edit.path, writable: true }];
        throw invalid("A selected task has no declared file scope and acceptance check.");
      });
      if (claims.length > 48) throw invalid("Select at most 48 total file inputs in a Run.");
      checkFileClaims(claims, entry => entry.path);
      const files = this.scoped.files;
      try {
        const workspace = await files.bind(preview.agent.cwd, signal);
        const prepared: ScopedRun["steps"] = [], collected: PlanFileTarget[] = [];
        for (const { id: stepId, scope, edit } of preview.steps) {
          if (scope) {
            const paths = [...scope.write, ...scope.read];
            const inputs = (await files.snapshot(workspace, paths, signal)).map(row => ({
              path: row.path, before: row.file, writable: scope.write.includes(row.path),
            }));
            collected.push(...inputs);
            prepared.push({ stepId, work: { files: inputs, proposals: [], acceptance: scope.acceptance } });
          } else if (edit) {
            const before = await files.preflight(workspace, edit, signal);
            collected.push({ path: edit.path, before, writable: true });
            prepared.push({ stepId, work: { edit, before } });
          }
        }
        checkFileClaims(collected, entry => entry.before.inode);
        scoped = { workspace, model: await this.scoped.selectModel(preview.agent, ctx), steps: prepared };
      } catch (error) {
        // A concurrent exact retry may win while this preflight reads the
        // workspace. Its receipt takes precedence over a now-stale file.
        const previous = await h.commit(async tx => receipt((await tx.doc(Receipts, owner)).records), ctx);
        if (previous !== undefined) return { runId: previous };
        if (error instanceof PlanFileError) throw invalid(error.message);
        throw error;
      }
    }
    const runId = await h.commit(async tx => {
      const admitted = await inspect(tx);
      if (admitted.runId !== undefined) return admitted.runId;
      const { state, receipts, steps, plan } = admitted;
      let claim;
      if (scoped) {
        if (state.lifecycle !== lifecycle) throw invalid("The plan was closed or reopened; select the tasks again.");
        const agent = await selectedModel(tx);
        if (agent.cwd !== scoped.workspace.cwd || !samePlanModel(agent, scoped.model) || (await tx.doc(LiveDoc, owner)).run ||
            (await tx.doc(InboxDoc, owner)).items.length || await foregroundPlan(tx, owner)) throw new WireError("busy");
        claim = await tx.doc(ExecutionClaim);
        if (claim.taskId) {
          const active = await tx.task(Number(claim.taskId) as TaskId);
          if (!active || active.state.status !== "terminal") throw new WireError("busy");
        }
      } else if (!this.fixture) throw new WireError("unsupported_capability", "plan");
      const taskId = await tx.createTask(scoped ? this.scoped!.task : executeFixture,
        { planId: q.planId, revision: q.revision, ...(scoped && { scoped }) },
        { ownership: { kind: "conversation" }, conversationId: owner, background: !scoped, abandonOnRestart: !!scoped });
      const id = String(taskId), stepIds = steps.map(s => s.id);
      if (claim) {
        claim.taskId = id;
        claim.owner = owner;
        (await tx.doc(PlanForeground, owner)).run = { id: taskId };
        await reportTurn(tx, planRunKind, q.threadId, id, planRunHeading(plan, stepIds.length),
          { planId: q.planId, revision: q.revision, stepIds });
      }
      // A previously failed task is pending again when selected for retry.
      state.progress = state.progress.filter(p => p.state !== "failed" || !stepIds.includes(p.stepId));
      state.run = { id, stepIds, state: "running", ...(scoped && { ownerThreadId: q.threadId }) };
      (await tx.doc(RunLog, q.planId, null)).runs.push({ id, revision: q.revision, threadId: q.threadId,
        stepIds: stepIds.slice(), state: "running", workers: [], ...(scoped && { turns: [] }) });
      await update(tx, q.planId, state, plan);
      receipts.push({ requestId: q.requestId, receipt: { runId: id, planId: q.planId, revision: q.revision, stepIds: q.stepIds } });
      return id;
    }, ctx);
    h.resume();
    return { runId };
  }
}
