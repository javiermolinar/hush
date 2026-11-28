import { createHash, randomBytes, randomUUID } from "node:crypto";
import { BACKGROUND_CONTEXT, withAbortSignal, type Context } from "@earendil-works/chord/context";
import { copyJson } from "@earendil-works/chord/json";
import { AgentDoc, configure, defineDoc, defineDocFamily, defineEntry, defineExtension, defineTask, LiveDoc,
  type AgentChange, type AgentState, type Conversation, type ConversationId, type Cursor, type Harness, type JsonObject, type Observation,
  type Observer, type Storage, type SubmissionId, type TaskId, type Tx } from "@earendil-works/pi-durable";
import { planChatKind, planDelegateLimit, planHistoryPageSize, planResultKind, planStepID, planTaskKind,
  type Plan, type PlanActivity, type PlanActivityQuery, type PlanClosure, type PlanDefinition, type PlanDelegate, type PlanGetQuery,
  type PlanHistory, type PlanHistoryQuery, type PlanHost, type PlanList, type PlanListQuery, type PlanOutcome, type PlanRunQuery,
  type PlanRunRecord, type PlanSelection, type PlanSnapshot, type PlanStepDefinition, type PlanWorktreeQuery,
  type PlanWorktreeRun } from "../../server/src/plan.js";
import { checkPreparedModel, type NativeHost } from "../../server/src/durable.js";
import { object, WireError } from "../../server/src/wire.js";
import { bindExecutionWorkspace, type ExecutionWorkspace } from "./execution-workspace.js";
import { migrateWorkspacePlans, PlanCatalog, planCatalogVersion, type PlanRow, type WorkspacePlans } from "./plan-catalog.js";
import { StartedThread, threadStartRequest } from "./thread-start.js";
import { markCheckoutReady, ThreadPresentationDoc, threadLabel } from "./thread-presentation.js";
import { observe } from "./workspace-changes.js";
import type { WorkspaceTrust } from "./workspace-trust.js";
import type { ThreadWorktrees } from "./worktrees.js";

const cursor = () => randomBytes(32).toString("hex");
// Document values are proxies inside a transaction; copy them out as plain JSON.
const copy = <T>(value: T): T => copyJson(value as Parameters<typeof copyJson>[0]) as T;
const thread = (id: string) => Number(id) as ConversationId;
const hash = (value: unknown) => createHash("sha256").update(JSON.stringify(value)).digest("hex");
const invalid = (message: string) => { const e = new WireError("invalid_request"); e.message = message; return e; };
const busy = (message: string) => { const e = new WireError("busy"); e.message = message; return e; };
const zero = "0".repeat(64);
const receiptLimit = 128;
export const planRevisionLimit = 128;
export const planPromptBytes = 4096;

type StepStatus = { stepId: string; status: "done" | "blocked"; reason?: string };
// The thread's run input is `submissionId`; `id` is its tracker task.
type ActiveRun = { id: string; threadId: string; submissionId: string; revision: number; stepIds: string[] };
type Head = { cursor: string; revision: number; workspace: string; statuses: StepStatus[]; runs: ActiveRun[]; closed?: true };
type RunRef = { planId: string; runId: string };
type ThreadRuns = { cursor: string; runs: RunRef[]; last?: RunRef };
type Receipt = { runId: string; planId: string; revision: number; stepIds: string[] };
type WorktreeReceipt = { source: string; threadId: string; title: string; checkout: PlanWorktreeRun["checkout"]; runId?: string };

const list = (v: unknown) => Array.isArray(v) ? v.filter(object) : [];
const text = (v: unknown) => typeof v === "string" ? v : undefined;
const strings = (v: unknown) => Array.isArray(v) ? v.filter((s): s is string => typeof s === "string") : [];

// Version 1 kept backend verification and model reports apart and reset
// both on every revision. Both mean done now; failed and incomplete steps
// are pending. Runs from before the upgrade are not carried over.
function migrateHead(v: JsonObject, from: number): Head {
  if (from !== 1) throw new Error(`Unknown plan head version ${from}`);
  const statuses: StepStatus[] = [];
  for (const p of list(v.progress)) if (p.state === "verified" && typeof p.stepId === "string") statuses.push({ stepId: p.stepId, status: "done" });
  for (const r of list(v.reports)) {
    if (typeof r.stepId !== "string" || statuses.some(s => s.stepId === r.stepId)) continue;
    if (r.state === "done") statuses.push({ stepId: r.stepId, status: "done" });
    else if (r.state === "blocked") statuses.push({ stepId: r.stepId, status: "blocked", ...(typeof r.detail === "string" && { reason: r.detail }) });
  }
  return { cursor: text(v.cursor) ?? zero, revision: typeof v.revision === "number" ? v.revision : 0, workspace: text(v.workspace) ?? "",
    statuses, runs: [], ...(v.closed === true && { closed: true as const }) };
}

// Description, checks and a known blocker become the step prompt. The
// scoped-run fields `edit`, `scope`, `after` and `thinking` are dropped.
function migrateStep(s: Record<string, unknown>): PlanStepDefinition {
  const id = String(s.id), title = text(s.title) ?? id, checks = strings(s.checks), requires = strings(s.requires).filter(planStepID);
  const prompt = [text(s.description) ?? "", ...(checks.length ? ["Checks:", ...checks.map(c => "- " + c)] : []),
    ...(text(s.blocker) ? [`Known blocker: ${s.blocker}`] : [])].join("\n").trim();
  return { id, title, prompt: prompt || title, ...(requires.length && { requires }) };
}

function migrateDefinition(v: JsonObject, from: number): { plan?: PlanDefinition } {
  if (from !== 1) throw new Error(`Unknown plan revision version ${from}`);
  const p = object(v.plan) ? v.plan : undefined;
  return p ? { plan: { id: String(p.id), revision: Number(p.revision), title: String(p.title), steps: list(p.steps).map(migrateStep) } } : {};
}

function migrateRun(r: Record<string, unknown>): PlanRunRecord {
  const outcomes: PlanOutcome[] = [];
  for (const t of [...list(r.workers), ...list(r.turns)]) {
    if (t.state === "verified" && typeof t.stepId === "string") outcomes.push({ stepId: t.stepId, status: "done", detail: "Verified." });
  }
  for (const x of list(r.reports)) {
    if ((x.state === "done" || x.state === "blocked") && typeof x.stepId === "string" && !outcomes.some(o => o.stepId === x.stepId))
      outcomes.push({ stepId: x.stepId, status: x.state, detail: text(x.detail) ?? "" });
  }
  const state = r.state === "done" || r.state === "verified" ? "finished" : "failed";
  const failure = r.state === "running" ? "Interrupted by a Hush upgrade." : r.state === "closed" ? "The plan was closed." : text(r.failure);
  return { id: String(r.id), revision: Number(r.revision), threadId: String(r.threadId), stepIds: strings(r.stepIds), state, outcomes,
    ...(text(r.entryId) && { entryId: r.entryId as string }), ...(state === "failed" && failure && { failure }),
    ...(r.worktree === true && { worktree: true as const }) };
}

const HeadDoc = defineDocFamily<Head, null>({
  kind: "hush.plan-head", version: 2, family: true, scope: "session",
  initial: () => ({ cursor: cursor(), revision: 0, workspace: "", statuses: [], runs: [] }), migrate: migrateHead, checkpointWhen: () => true,
});
const Definition = defineDocFamily<{ plan?: PlanDefinition }, null>({
  kind: "hush.plan-revision", version: 2, family: true, scope: "session",
  initial: () => ({}), migrate: migrateDefinition, checkpointWhen: () => true,
});
const RunLog = defineDocFamily<{ runs: PlanRunRecord[] }, null>({
  kind: "hush.plan-run-log", version: 2, family: true, scope: "session", initial: () => ({ runs: [] }),
  migrate: (v, from) => { if (from !== 1) throw new Error(`Unknown plan run log version ${from}`); return { runs: list(v.runs).map(migrateRun) }; },
  checkpointWhen: () => true,
});
const WorkspaceCatalog = defineDocFamily<WorkspacePlans, null>({
  kind: "hush.plan-workspace", version: planCatalogVersion, family: true, scope: "session",
  initial: () => ({ cursor: cursor(), rows: [] }), migrate: migrateWorkspacePlans, checkpointWhen: () => true,
});
const CatalogReady = defineDoc<{ ready?: true; runOrder?: true; plan2?: true }>({
  kind: "hush.plan-workspace-indexed", version: 1, scope: "session", initial: () => ({}), checkpointWhen: () => true,
});
// A thread's runs, oldest first. Forks start empty: a run belongs to one thread.
const ThreadRunsDoc = defineDoc<ThreadRuns>({
  kind: "hush.plan-chat-activity", version: 2, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ cursor: cursor(), runs: [] }),
  migrate: (v, from) => {
    if (from !== 1) throw new Error(`Unknown plan activity version ${from}`);
    const last = object(v.last) && object(v.last.run) ? { planId: String(v.last.planId), runId: String(v.last.run.id) } : undefined;
    return { cursor: text(v.cursor) ?? zero, runs: [], ...(last && { last }) };
  },
  checkpointWhen: () => true,
});
const Receipts = defineDoc<{ records: { requestId: string; receipt: Receipt }[] }>({
  kind: "hush.plan-fixture-receipts", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({ records: [] }), checkpointWhen: () => true,
});
const WorktreeRuns = defineDocFamily<{ receipt?: WorktreeReceipt }, null>({
  kind: "hush.plan-worktree-run", version: 1, family: true, scope: "session", initial: () => ({}), checkpointWhen: () => true,
});
const RunInput = defineEntry<{ planId: string; revision: number; text: string; timestampMs: number }>(planChatKind);

const revisionKey = (id: string, revision: number) => `${id}:${revision}`;
const statusOf = (head: Readonly<Head>, id: string) => head.statuses.find(s => s.stepId === id);
const isComplete = (head: Readonly<Head>, plan: PlanDefinition) => plan.steps.every(s => statusOf(head, s.id)?.status === "done");

async function readDefinition(tx: Tx, planId: string, revision: number) {
  const plan = (await tx.doc(Definition, revisionKey(planId, revision), null)).plan;
  if (!plan) throw new WireError("internal");
  return plan;
}

async function checkThread(tx: Tx, owner: ConversationId) {
  const record = await tx.conversation(owner);
  if (!record || record.owner) throw new WireError("not_found");
}

// Statuses hold only the current revision's steps, so counting them needs
// no definition read unless the revision changed.
async function refreshRow(tx: Tx, planId: string, head: Readonly<Head>, plan?: PlanDefinition) {
  const catalog = await tx.doc(WorkspaceCatalog, head.workspace, null);
  const index = catalog.rows.findIndex(r => r.id === planId), previous = catalog.rows[index];
  const { title, total } = plan ? { title: plan.title, total: plan.steps.length } : previous?.revision === head.revision ? previous
    : await readDefinition(tx, planId, head.revision).then(p => ({ title: p.title, total: p.steps.length }));
  const count = (status: StepStatus["status"]) => head.statuses.filter(s => s.status === status).length;
  const next: PlanRow = { id: planId, title, revision: head.revision, total, done: count("done"), blocked: count("blocked"),
    running: head.runs.length > 0, closed: !!head.closed };
  if (previous && JSON.stringify(previous) === JSON.stringify(next)) return;
  if (index < 0) catalog.rows.push(next);
  else catalog.rows[index] = next;
  catalog.cursor = cursor();
}

async function touch(tx: Tx, planId: string, head: Head, plan?: PlanDefinition) {
  head.cursor = cursor();
  await refreshRow(tx, planId, head, plan);
}

function view(planId: string, head: Readonly<Head>, plan: PlanDefinition): Plan {
  return { id: planId, revision: head.revision, title: plan.title, workspace: head.workspace,
    steps: plan.steps.map(step => {
      const s = statusOf(head, step.id);
      return { ...copy(step), status: s?.status ?? "pending", ...(s?.reason !== undefined && { reason: s.reason }) };
    }),
    runs: head.runs.map(({ id, threadId, stepIds }) => ({ id, threadId, stepIds: stepIds.slice() })),
    ...(head.closed && { closed: true as const }), ...(isComplete(head, plan) && { complete: true as const }) };
}

// ----- Model tools -----

// The plans of the calling thread's workspace, newest first.
export async function workspacePlans(tx: Tx, owner: ConversationId, directory: (cwd?: string) => string) {
  await checkThread(tx, owner);
  const cwd = directory((await tx.doc(AgentDoc, owner)).cwd);
  return (await tx.doc(WorkspaceCatalog, cwd, null)).rows.toReversed().map(r => ({ ...r }));
}

export async function workspacePlan(tx: Tx, owner: ConversationId, directory: (cwd?: string) => string, planId: string) {
  await checkThread(tx, owner);
  const cwd = directory((await tx.doc(AgentDoc, owner)).cwd);
  const head = await tx.doc(HeadDoc, planId, null);
  if (!head.revision || head.workspace !== cwd) throw invalid(`No plan ${planId} in this workspace.`);
  return view(planId, head, await readDefinition(tx, planId, head.revision));
}

export type PlanDraft = { planId?: string; baseRevision: number; title: string; steps: PlanStepDefinition[] };

export async function savePlan(tx: Tx, owner: ConversationId, directory: (cwd?: string) => string, draft: PlanDraft) {
  await checkThread(tx, owner);
  const cwd = directory((await tx.doc(AgentDoc, owner)).cwd);
  const stale = () => invalid("The plan revision is stale. Read the current plan before revising it.");
  let planId = draft.planId;
  if (planId === undefined) {
    if (draft.baseRevision !== 0) throw invalid("Pass planId to revise an existing plan; use baseRevision 0 only for a new plan.");
    planId = randomUUID();
  }
  const head = await tx.doc(HeadDoc, planId, null);
  if (draft.planId !== undefined) {
    if (!head.revision || head.workspace !== cwd) throw invalid(`No plan ${planId} in this workspace.`);
    if (head.closed) throw invalid("This plan is closed. Ask the user to reopen it in /plan before revising it.");
    if (head.revision !== draft.baseRevision) throw stale();
  } else head.workspace = cwd;
  if (draft.baseRevision >= planRevisionLimit) throw new WireError("limit");
  const plan: PlanDefinition = { id: planId, revision: draft.baseRevision + 1, title: draft.title, steps: copy(draft.steps) };
  const record = await tx.doc(Definition, revisionKey(planId, plan.revision), null);
  if (record.plan) throw stale();
  record.plan = plan;
  head.revision = plan.revision;
  // Statuses follow step IDs across revisions. A removed step drops its status.
  head.statuses = head.statuses.filter(s => plan.steps.some(step => step.id === s.stepId));
  await touch(tx, planId, head, plan);
  return { planId, revision: plan.revision };
}

// The plan run that the conversation's current turn is working on.
async function runAssignment(tx: Tx, owner: ConversationId) {
  const live = (await tx.doc(LiveDoc, owner)).run;
  if (live?.first === undefined) return;
  const entry = await tx.entry(RunInput, live.first);
  if (!entry || entry.conversationId !== owner) return;
  const planId = entry.data.planId, head = await tx.doc(HeadDoc, planId, null);
  const run = head.runs.find(r => r.threadId === String(owner) && live.inputs.includes(Number(r.submissionId) as SubmissionId));
  if (run) return { planId, head, run };
}

export async function inPlanRun(tx: Tx, owner: ConversationId) { return !!await runAssignment(tx, owner); }

export async function reportSteps(tx: Tx, owner: ConversationId, reports: readonly PlanOutcome[]) {
  const assignment = await runAssignment(tx, owner);
  if (!assignment) return "No plan run is active in this turn.";
  const { planId, head, run } = assignment;
  if (new Set(reports.map(r => r.stepId)).size !== reports.length) return "Report each step once.";
  for (const r of reports) {
    if (!run.stepIds.includes(r.stepId)) return `Step ${r.stepId} is not selected in this run.`;
    if (!r.detail.isWellFormed() || /[\p{Cc}\p{Zl}\p{Zp}]/u.test(r.detail)) return "Each detail must be a single line of text.";
    if (r.status === "blocked" && statusOf(head, r.stepId)?.status === "done") return `Step ${r.stepId} is already done; done is final.`;
  }
  const plan = await readDefinition(tx, planId, head.revision);
  const record = (await tx.doc(RunLog, planId, null)).runs.findLast(r => r.id === run.id);
  if (!record) throw new WireError("internal");
  for (const r of reports) {
    const outcome = { stepId: r.stepId, status: r.status, detail: r.detail };
    const i = record.outcomes.findIndex(o => o.stepId === r.stepId);
    if (i >= 0) record.outcomes[i] = outcome;
    else record.outcomes.push(outcome);
    // A step removed by a later revision keeps only its run outcome.
    if (!plan.steps.some(s => s.id === r.stepId) || statusOf(head, r.stepId)?.status === "done") continue;
    head.statuses = [...head.statuses.filter(s => s.stepId !== r.stepId),
      { stepId: r.stepId, status: r.status, ...(r.status === "blocked" && { reason: r.detail }) }];
  }
  await touch(tx, planId, head, plan);
  // The thread's activity watch follows its own cursor, not the plan's.
  (await tx.doc(ThreadRunsDoc, owner)).cursor = cursor();
}

// `subagent({ step })` takes the step's specification from the run's pinned
// revision; the model cannot change it.
export async function planStepDelegate(tx: Tx, owner: ConversationId, stepId: string): Promise<{ error: string } | { prompt: string } & PlanDelegate> {
  const assignment = await runAssignment(tx, owner);
  if (!assignment) return { error: "No plan run is active in this turn." };
  if (!assignment.run.stepIds.includes(stepId)) return { error: `Step ${stepId} is not selected in this run.` };
  const plan = await readDefinition(tx, assignment.planId, assignment.run.revision);
  const step = plan.steps.find(s => s.id === stepId);
  if (!step?.delegate) return { error: `Step ${stepId} has no delegate specification.` };
  return { prompt: `Plan "${plan.title}", step ${step.id}: ${step.title}\n\n${step.prompt}`, ...copy(step.delegate) };
}

// ----- Runs -----

// A Continue must not replay a plan run input after its run ended, also when
// a fork inherits it: its reports and step delegates would have no run.
export async function checkPlanContinue(tx: Tx, owner: ConversationId, operation: "input" | "continue" | "configure" | "compact" | "reset") {
  if (operation !== "continue") return;
  const planInput = (kind?: string) => kind === planTaskKind || kind === planChatKind;
  const failed = (await tx.doc(LiveDoc, owner)).runFailure?.first;
  let plan = failed !== undefined && planInput((await tx.entry(failed))?.kind);
  if (!plan && (await tx.conversation(owner))?.parent) {
    const { items } = await tx.scanEntries({ conversationId: owner, order: "descending" }, 64);
    const input = items.find(e => e.kind === "pi.reset" || e.model?.some(m => m.role === "user"));
    plan = input ? planInput(input.kind) : items.length === 64;
  }
  if (plan) throw invalid("A plan run can only be retried from /plan. Select its steps again.");
}

function runHeading(plan: PlanDefinition, steps: number) {
  return `Plan run · r${plan.revision} · ${plan.title.replace(/\s+/gu, " ").trim()} · ${steps} ${steps === 1 ? "step" : "steps"}`;
}

function runPrompt(planId: string, head: Readonly<Head>, plan: PlanDefinition, selected: string[], worktree: boolean) {
  const steps = view(planId, head, plan).steps.map(({ id, title, prompt, requires, delegate, status, reason }) =>
    ({ id, title, prompt, requires, delegate, status, reason }));
  return "Plan run\n" + JSON.stringify({ plan: planId, title: plan.title, revision: plan.revision, selected, steps }) +
    "\nWork on the selected steps in this turn. Use the other steps as context, not assignments. Inspect what already exists, " +
    "do the setup the selected work needs, and run relevant checks. For a selected step with a delegate specification, call subagent " +
    "with only its step ID. Report each selected step you finish or cannot finish with plan_report: done, or blocked with the reason. " +
    "Leave a step unreported if you did not get to it. If a blocker remains, continue with independent selected steps. " +
    "Do not start other threads for this run. Treat this assignment as a record in later turns, not a standing request." +
    (worktree ? " Work in this checkout only. Do not change other checkouts or merge the work back." : "");
}

function resultText(record: PlanRunRecord, steps: readonly PlanStepDefinition[]) {
  const label = record.state === "finished" ? "Plan run finished" : "Plan run stopped";
  const count = (status: PlanOutcome["status"]) => record.outcomes.filter(o => o.status === status).length;
  const pending = record.stepIds.length - record.outcomes.length;
  const counts = [`${count("done")} done`, ...(count("blocked") ? [`${count("blocked")} blocked`] : []), ...(pending ? [`${pending} not reported`] : [])];
  const lines = record.outcomes.map(o => `${steps.find(s => s.id === o.stepId)?.title ?? o.stepId}: ${o.status} · ${o.detail}`);
  return [`${label} · ${counts.join(" · ")}`, ...(record.failure ? [record.failure] : []), ...lines].join("\n");
}

async function finishRun(tx: Tx, planId: string, runId: string, failure?: string) {
  const head = await tx.doc(HeadDoc, planId, null);
  const run = head.runs.find(r => r.id === runId);
  if (!run) return;
  head.runs = head.runs.filter(r => r !== run);
  const owner = thread(run.threadId);
  const record = (await tx.doc(RunLog, planId, null)).runs.findLast(r => r.id === runId);
  if (record) {
    record.state = failure === undefined ? "finished" : "failed";
    if (failure !== undefined) record.failure = failure.slice(0, 240).toWellFormed();
    const plan = await readDefinition(tx, planId, run.revision);
    await tx.appendEntry(owner, { kind: planResultKind, data: { runId, planId, stepIds: run.stepIds.slice(), outcome: record.state,
      text: resultText(record, plan.steps), timestampMs: Date.now() } });
  }
  const runs = await tx.doc(ThreadRunsDoc, owner);
  runs.runs = runs.runs.filter(r => r.runId !== runId);
  runs.last = { planId, runId };
  runs.cursor = cursor();
  await touch(tx, planId, head);
}

const ended = { status: "terminal", outcome: { status: "completed", result: null } } as const;
const aborted = { status: "terminal", outcome: { status: "aborted" } } as const;

// Follows one run input to its settlement. It never invokes a model, owns a
// conversation or resubmits input. Trackers from before the upgrade find no
// active run and end.
function runTracker(harness: () => Harness) {
  return defineTask<{ planId: string; revision: number; submission: SubmissionId }, { phase: "settle" }, null>({
    name: "hush.plan-chat-tracker", version: 1, initial: () => ({ phase: "settle" }),
    phases: { settle: async (task, runtime, ctx) => {
      const { planId, submission } = task.input, wait = withAbortSignal(runtime.signal, ctx);
      const settled = await (await harness().submission(submission, wait))?.wait(wait);
      return runtime.commit(async tx => {
        const record = (await tx.doc(RunLog, planId, null)).runs.findLast(r => r.id === String(task.id));
        if (record && settled?.entry !== undefined) record.entryId = String(settled.entry);
        await finishRun(tx, planId, String(task.id), settled?.status === "done" && settled.conversationId === runtime.conversationId ? undefined
          : settled?.entry === undefined ? "The run input did not start." : "The run turn did not finish. Inspect any changes before retrying.");
        return ended;
      }, ctx);
    } },
    abort: (task, runtime, ctx) => runtime.commit(async tx => {
      await finishRun(tx, task.input.planId, String(task.id), "Run tracking stopped. Check the thread before retrying.");
      return aborted;
    }, ctx),
  });
}

// Wait on one address until its projected token changes.
async function observed<T>(attach: (observer: Observer<Readonly<T>>) => Promise<Observation<Readonly<T>> | undefined>,
    token: (value: Readonly<T>) => string, after: string | undefined, signal: AbortSignal,
    settle?: (value: Readonly<T>, signal: AbortSignal) => Promise<unknown> | undefined): Promise<Readonly<T>> {
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
    // A faulted tracker settles without its abort callback; fail its run.
    void settle?.(watching.value, AbortSignal.any([signal, stop.signal]))?.catch(() => {});
    const value = await change.promise;
    signal.throwIfAborted();
    if (value === undefined) throw new WireError("internal");
    return value;
  } finally {
    stop.abort();
    signal.removeEventListener("abort", abort);
    watching.detach();
  }
}

export type PlanRuns = {
  authorize(cwd: string | undefined, ctx: Context): Promise<string>;
  requireModel: NativeHost["requireModel"];
  agent: AgentChange;
  trust: WorkspaceTrust;
};
export type PlanWorktrees = {
  checkout: ThreadWorktrees;
  defaults: NonNullable<NativeHost["defaults"]>;
  validateSelection: NonNullable<NativeHost["validateSelection"]>;
  name(ref: NonNullable<AgentState["model"]>, title: string, steps: readonly string[], signal: AbortSignal): Promise<string | undefined>;
};

export class Plans implements PlanHost {
  readonly extension;
  readonly #tracker;
  #indexed?: Promise<void>;
  #catalog?: PlanCatalog;
  // In-flight worktree runs by request key. A retry shares the running call.
  #preparing = new Map<string, { fingerprint: string; run: Promise<PlanWorktreeRun> }>();

  constructor(private readonly harness: () => Harness, private readonly storage: () => Storage,
    private readonly directory: (cwd?: string) => string, private readonly runs: PlanRuns, private readonly worktrees: PlanWorktrees) {
    this.#tracker = runTracker(harness);
    this.extension = defineExtension({ name: "hush-plan-chat", tasks: [this.#tracker] });
  }

  #ctx(signal: AbortSignal) { return withAbortSignal(signal, BACKGROUND_CONTEXT); }

  // Reads require a trusted directory. A wait may outlive the grant, so
  // callers check it again before replying.
  async #launch(path: string, signal: AbortSignal) {
    const cwd = this.directory(path);
    await this.runs.authorize(cwd, this.#ctx(signal));
    return cwd;
  }

  async #head(tx: Tx, planId: string) {
    const head = await tx.doc(HeadDoc, planId, null);
    if (!head.revision) throw new WireError("not_found");
    return head;
  }

  // Upgrade earlier stores once without opening conversations. This is off the startup path.
  async #index() {
    const h = this.harness(), ctx = BACKGROUND_CONTEXT;
    if ((await h.snapshot(CatalogReady, ctx))?.plan2) return;
    let continuation: Cursor | undefined;
    do {
      const page = await this.storage().scanDocuments({ scope: { kind: "session" }, at: "current", kind: HeadDoc.definition.kind }, 32, continuation, ctx);
      for (const { key } of page.items) if (key) await h.commit(async tx => {
        const head = await tx.doc(HeadDoc, key, null);
        if (head.revision) await refreshRow(tx, key, head);
      }, ctx);
      continuation = page.next;
    } while (continuation);
    await h.commit(async tx => { Object.assign(await tx.doc(CatalogReady), { ready: true, runOrder: true, plan2: true }); }, ctx);
  }

  async list(q: PlanListQuery, signal: AbortSignal): Promise<PlanList> {
    const cwd = await this.#launch(q.cwd, signal);
    await observe(this.#indexed ??= this.#index().catch(error => { this.#indexed = undefined; throw error; }), signal);
    const catalog = this.#catalog ??= new PlanCatalog(this.harness(), this.storage(), WorkspaceCatalog.definition.kind);
    const page = await catalog.page(cwd, q.page, q.after, signal);
    await this.#launch(cwd, signal);
    return page;
  }

  async get(q: PlanGetQuery, signal: AbortSignal): Promise<PlanSnapshot> {
    const cwd = await this.#launch(q.cwd, signal), h = this.harness(), ctx = this.#ctx(signal);
    await h.commit(tx => this.#head(tx, q.planId), ctx);
    const token = (head: Readonly<Head>) => hash([head.cursor, cwd]);
    const head = await observed(observer => h.observeDoc(HeadDoc, q.planId, observer, ctx), token, q.after, signal,
      (value, abort) => this.#settle(q.planId, value.runs, abort));
    const plan = await h.commit(async tx => view(q.planId, head, await readDefinition(tx, q.planId, head.revision)), ctx);
    await this.#launch(cwd, signal);
    return { revision: token(head), plan };
  }

  async #settle(planId: string, runs: readonly ActiveRun[], signal: AbortSignal) {
    const h = this.harness(), ctx = this.#ctx(signal);
    await Promise.all(runs.map(async run => {
      await h.waitForTask(Number(run.id) as TaskId, ctx);
      await h.commit(tx => finishRun(tx, planId, run.id, "Run interrupted. Inspect changes before retrying."), ctx);
    }));
  }

  async history(q: PlanHistoryQuery, signal: AbortSignal): Promise<PlanHistory> {
    await this.#launch(q.cwd, signal);
    return this.harness().commit(async tx => {
      await this.#head(tx, q.planId);
      const runs = (await tx.doc(RunLog, q.planId, null)).runs;
      const end = q.before === undefined ? runs.length : runs.findIndex(r => r.id === q.before);
      if (end < 0) throw new WireError("invalid_request");
      const start = Math.max(0, end - planHistoryPageSize);
      const page = runs.slice(start, end).reverse().map(r => copy(r));
      return { runs: page, next: start > 0 ? page.at(-1)!.id : null };
    }, this.#ctx(signal));
  }

  async close(q: PlanClosure, signal: AbortSignal) {
    const cwd = await this.#launch(q.cwd, signal);
    await this.harness().commit(async tx => {
      const head = await this.#head(tx, q.planId);
      if (head.workspace !== cwd) throw new WireError("not_found");
      if (!!head.closed === q.closed) return;
      if (q.closed && head.runs.length) throw busy("A run is active. Stop it before closing the plan.");
      if (q.closed) head.closed = true;
      else delete head.closed;
      await touch(tx, q.planId, head);
    }, this.#ctx(signal));
    return {};
  }

  async activity(q: PlanActivityQuery, signal: AbortSignal): Promise<PlanActivity> {
    const h = this.harness(), owner = thread(q.threadId), ctx = this.#ctx(signal);
    await h.commit(async tx => { await checkThread(tx, owner); await tx.doc(ThreadRunsDoc, owner); }, ctx);
    const runs = await observed<ThreadRuns>(observer => h.observeDoc(ThreadRunsDoc, owner, observer, ctx), r => r.cursor, q.after, signal,
      (value, abort) => {
        const ref = value.runs[0];
        return ref && h.snapshot(HeadDoc, ref.planId, ctx).then(head => this.#settle(ref.planId, head?.runs.filter(r => r.id === ref.runId) ?? [], abort));
      });
    const ref = runs.runs[0] ?? runs.last;
    if (!ref) return { revision: runs.cursor, run: null };
    const run = await h.commit(async tx => {
      const record = (await tx.doc(RunLog, ref.planId, null)).runs.findLast(r => r.id === ref.runId);
      const head = await tx.doc(HeadDoc, ref.planId, null);
      if (!record || !head.revision) return null;
      const plan = await readDefinition(tx, ref.planId, record.revision);
      const steps = record.stepIds.flatMap(id => {
        const step = plan.steps.find(s => s.id === id), s = statusOf(head, id);
        return step ? [{ id, title: step.title, status: s?.status ?? "pending" as const, ...(s?.reason !== undefined && { reason: s.reason }) }] : [];
      });
      return { ...copy(record), planId: ref.planId, title: plan.title, steps };
    }, ctx);
    return { revision: runs.cursor, run };
  }

  // Selection checks shared by both run forms; the caller holds the head.
  async #select(tx: Tx, head: Readonly<Head>, planId: string, q: PlanSelection, cwd: string) {
    if (head.workspace !== cwd) throw new WireError("not_found");
    if (head.closed) throw invalid("This plan is closed. Reopen it before running steps.");
    if (head.revision !== q.revision) throw invalid("Plan changed; review and select the current revision.");
    const plan = await readDefinition(tx, planId, head.revision);
    const steps = plan.steps.filter(step => q.stepIds.includes(step.id));
    if (steps.length !== q.stepIds.length) throw invalid("Selection includes an unknown step.");
    if (steps.some(s => statusOf(head, s.id)?.status === "done")) throw invalid("Selection includes a done step.");
    if (steps.some(s => head.runs.some(r => r.stepIds.includes(s.id)))) throw busy("A selected step is already in an active run.");
    if (steps.filter(s => s.delegate).length > planDelegateLimit) throw invalid(`A run can delegate at most ${planDelegateLimit} steps.`);
    return { plan, stepIds: steps.map(s => s.id) };
  }

  async #admit(tx: Tx, target: Conversation, planId: string, head: Head, plan: PlanDefinition, stepIds: string[], requestId: string,
      worktree?: ExecutionWorkspace) {
    const owner = target.id, threadId = String(owner);
    await configure(tx, owner, this.runs.agent);
    const submission = await target.submit(tx, { type: "input", whenBusy: worktree ? "reject" : "followUp",
      content: runPrompt(planId, head, plan, stepIds, !!worktree), ...(worktree && { requestId: threadStartRequest }),
      entry: { kind: planChatKind, data: { planId, revision: plan.revision, timestampMs: Date.now(), text: runHeading(plan, stepIds.length) } } });
    // A new worktree thread's first turn runs in the checkout it was created with.
    if (worktree) (await tx.doc(StartedThread, owner)).first = { submission, workspace: { ...worktree } };
    const id = String(await tx.createTask(this.#tracker, { planId, revision: plan.revision, submission },
      { ownership: { kind: "conversation" }, conversationId: owner, background: true }));
    head.runs.push({ id, threadId, submissionId: String(submission), revision: plan.revision, stepIds });
    const runs = await tx.doc(ThreadRunsDoc, owner);
    runs.runs.push({ planId, runId: id });
    runs.cursor = cursor();
    (await tx.doc(RunLog, planId, null)).runs.push({ id, revision: plan.revision, threadId, submissionId: String(submission), stepIds,
      state: "running", outcomes: [], ...(worktree && { worktree: true as const }) });
    const receipts = (await tx.doc(Receipts, owner)).records;
    receipts.push({ requestId, receipt: { runId: id, planId, revision: plan.revision, stepIds: stepIds.slice() } });
    await touch(tx, planId, head, plan);
    return id;
  }

  async run(q: PlanRunQuery, signal: AbortSignal) {
    const h = this.harness(), owner = thread(q.threadId), ctx = this.#ctx(signal);
    const target = await h.conversation(owner, ctx);
    if (!target) throw new WireError("not_found");
    const inspect = async (tx: Tx) => {
      await checkThread(tx, owner);
      const records = (await tx.doc(Receipts, owner)).records;
      const prior = records.find(r => r.requestId === q.requestId)?.receipt;
      if (prior) {
        // A selection is a set; the receipt stores it in plan order.
        if (prior.planId !== q.planId || prior.revision !== q.revision || prior.stepIds.toSorted().join() !== q.stepIds.toSorted().join())
          throw invalid("Run request ID already belongs to a different selection.");
        return { receipt: prior.runId };
      }
      if (records.length >= receiptLimit) throw new WireError("limit");
      const agent = await tx.doc(AgentDoc, owner), head = await this.#head(tx, q.planId);
      return { receipt: undefined, agent, head, ...await this.#select(tx, head, q.planId, q, this.directory(agent.cwd)) };
    };
    const before = await h.commit(async tx => {
      const inspected = await inspect(tx);
      if (inspected.receipt !== undefined) return { receipt: inspected.receipt };
      const { cwd, model } = inspected.agent!;
      return { receipt: undefined, cwd, model: model && { provider: model.provider, modelId: model.modelId } };
    }, ctx);
    if (before.receipt !== undefined) return { runId: before.receipt };
    const verify = await checkPreparedModel(this.runs.requireModel, before.model, this.runs.agent);
    await this.runs.authorize(before.cwd, ctx);
    const runId = await h.commit(async tx => {
      const inspected = await inspect(tx);
      if (inspected.receipt !== undefined) return inspected.receipt;
      if (inspected.agent!.cwd !== before.cwd) throw new WireError("busy");
      verify?.(inspected.agent!.model);
      return this.#admit(tx, target, q.planId, inspected.head!, inspected.plan!, inspected.stepIds!, q.requestId);
    }, ctx);
    h.resume();
    return { runId };
  }

  // One call creates a checkout from committed HEAD, a thread in it, and the
  // run. A retry of the same request finishes or returns the same run; one
  // that arrives while the first is still working waits for its result.
  runInWorktree(q: PlanWorktreeQuery, signal: AbortSignal): Promise<PlanWorktreeRun> {
    const owner = "threadId" in q ? q.threadId : undefined, launch = "cwd" in q ? q : undefined;
    const key = hash([q.planId, q.requestId]);
    // A selection is a set, as in run's receipts.
    const fingerprint = hash([owner, launch?.cwd, launch?.model?.provider, launch?.model?.modelId, launch?.thinkingLevel, q.revision, q.stepIds.toSorted()]);
    const pending = this.#preparing.get(key);
    if (pending) {
      if (pending.fingerprint !== fingerprint) return Promise.reject(invalid("Worktree request ID already belongs to a different selection or source."));
      // Waiting doesn't tie the shared call to this peer's connection.
      return observe(pending.run, signal);
    }
    const run = this.#runInWorktree(q, signal, key, fingerprint).finally(() => this.#preparing.delete(key));
    this.#preparing.set(key, { fingerprint, run });
    return run;
  }

  async #runInWorktree(q: PlanWorktreeQuery, signal: AbortSignal, key: string, fingerprint: string): Promise<PlanWorktreeRun> {
    const h = this.harness(), ctx = this.#ctx(signal), options = this.worktrees;
    const owner = "threadId" in q ? thread(q.threadId) : undefined, launch = "cwd" in q ? q : undefined;
    const incomplete = (id: string) => invalid(`Worktree setup incomplete · inspect thread #${id} in /threads.`);
    let reserved: WorktreeReceipt | undefined, ready = false;
    // `reserve` assigns it during the checkout; read it through a call.
    const reservation = () => { if (!reserved) throw new WireError("internal"); return reserved; };
    try {
      const sourceCwd = async (tx: Tx) => {
        if (owner === undefined) return this.directory(launch!.cwd);
        await checkThread(tx, owner);
        return this.directory((await tx.doc(AgentDoc, owner)).cwd);
      };
      const source = await h.commit(async tx => {
        const stored = (await tx.doc(WorktreeRuns, key, null)).receipt;
        if (stored && stored.source !== fingerprint) throw invalid("Worktree request ID already belongs to a different selection or source.");
        if (stored) return { receipt: copy(stored) as WorktreeReceipt };
        const cwd = await sourceCwd(tx), head = await this.#head(tx, q.planId);
        const { plan, stepIds } = await this.#select(tx, head, q.planId, q, cwd);
        const agent = owner === undefined ? undefined : await tx.doc(AgentDoc, owner);
        return { receipt: undefined, cwd, title: plan.title, steps: plan.steps.filter(s => stepIds.includes(s.id)).map(s => s.title),
          model: agent?.model && { ...agent.model }, thinkingLevel: agent?.thinkingLevel };
      }, ctx);
      if (source.receipt?.runId !== undefined) {
        const { runId, threadId, title, checkout } = source.receipt;
        return { runId, threadId, title, checkout };
      }
      let model: AgentState["model"], thinkingLevel: AgentState["thinkingLevel"];
      if (source.receipt !== undefined) {
        reserved = source.receipt;
        const checkout = (await h.snapshot(ThreadPresentationDoc, thread(reserved.threadId), ctx))?.checkout;
        if (checkout?.cwd !== reserved.checkout.cwd || !checkout.ready) throw incomplete(reserved.threadId);
        ready = true;
      } else {
        const defaults = await options.defaults(source.cwd, ctx);
        model = launch?.model ?? source.model ?? defaults.model ?? undefined;
        thinkingLevel = launch?.thinkingLevel ?? source.thinkingLevel ?? defaults.thinkingLevel ?? undefined;
        await options.validateSelection(model, thinkingLevel ?? "off");
        const grant = await bindExecutionWorkspace(this.runs.trust, source.cwd, signal);
        let label: string | undefined;
        await options.checkout.create(source.cwd, signal, {
          grant: grant.grant,
          name: async signal => {
            try { if (model) label = await options.name(model, source.title, source.steps, signal); }
            catch { signal.throwIfAborted(); } // An unavailable provider or unusable name must not block the run.
            return { preferred: label, fallback: source.title };
          },
          // Publish a discoverable thread before Git writes the checkout.
          reserve: async planned => {
            const title = threadLabel(label ?? source.title, 96) || "Plan worktree";
            reserved = await h.commit(async tx => {
              const head = await this.#head(tx, q.planId);
              if (await sourceCwd(tx) !== source.cwd) throw new WireError("busy");
              await this.#select(tx, head, q.planId, q, source.cwd);
              const record = await tx.doc(WorktreeRuns, key, null);
              if (record.receipt) throw new WireError("busy");
              const { id } = await tx.createConversation({ ownership: { kind: "ownerless" } });
              await configure(tx, id, { ...defaults, cwd: planned.cwd, model, thinkingLevel });
              const presentation = await tx.doc(ThreadPresentationDoc, id);
              presentation.title = title;
              presentation.checkout = { cwd: planned.cwd, branch: planned.branch, repository: planned.repository };
              record.receipt = { source: fingerprint, threadId: String(id), title,
                checkout: { cwd: planned.cwd, root: planned.root, branch: planned.branch } };
              return copy(record.receipt);
            }, ctx);
          },
          // Not tied to this request: a dropped peer still records the finished checkout.
          created: async planned => {
            const threadId = reserved?.threadId;
            if (threadId) await h.commit(tx => markCheckoutReady(tx, thread(threadId), planned.cwd), BACKGROUND_CONTEXT);
          },
        });
        ready = true;
      }
      const receipt = reservation();
      const child = thread(receipt.threadId), target = await h.conversation(child, ctx);
      if (!target) throw incomplete(receipt.threadId);
      // A checkout Hush creates inherits the source's grant.
      const workspace = await bindExecutionWorkspace(this.runs.trust, receipt.checkout.cwd, signal);
      const before = await h.snapshot(AgentDoc, child, ctx);
      const selected = { provider: before?.model?.provider, modelId: before?.model?.modelId, thinking: before?.thinkingLevel };
      await options.validateSelection(before?.model, before?.thinkingLevel ?? "off");
      const runId = await h.commit(async tx => {
        const record = (await tx.doc(WorktreeRuns, key, null)).receipt;
        if (record?.threadId !== receipt.threadId) throw new WireError("internal");
        if (record.runId !== undefined) return record.runId;
        const head = await this.#head(tx, q.planId);
        const { plan, stepIds } = await this.#select(tx, head, q.planId, q, head.workspace);
        const agent = await tx.doc(AgentDoc, child);
        if (agent.cwd !== workspace.cwd) throw incomplete(receipt.threadId);
        // The thread is discoverable while it waits; admit only the validated selection.
        if (agent.model?.provider !== selected.provider || agent.model?.modelId !== selected.modelId || agent.thinkingLevel !== selected.thinking)
          throw busy("The worktree thread's model changed. Retry the run.");
        record.runId = await this.#admit(tx, target, q.planId, head, plan, stepIds, q.requestId, workspace);
        return record.runId;
      }, ctx);
      h.resume();
      return { runId, threadId: receipt.threadId, title: receipt.title, checkout: { ...receipt.checkout } };
    } catch (error) {
      // A reserved thread whose checkout didn't finish stays discoverable.
      signal.throwIfAborted();
      if (reserved !== undefined && !ready) throw incomplete(reservation().threadId);
      throw error;
    }
  }
}

