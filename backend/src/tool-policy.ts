import { copyJson } from "@earendil-works/chord/json";
import type { Context } from "@earendil-works/chord/context";
import { AgentDoc, defineDoc, LiveDoc, ResetEntry, ToolTask, type ConversationId, type ConversationRecord, type EntryId,
  type Extension, type Harness, type LiveRun, type SubmissionId, type TaskId, type ToolExecutionApi, type ToolRegistration, type Tx } from "@earendil-works/pi-durable";
import { planChatKind, planTaskKind } from "../../server/src/plan.js";
import { joinReviewsName, spawnReviewsName } from "../../server/src/delegation.js";
import { subagentToolName } from "../../server/src/subagents.js";
import { allowedExecutionPath, bindExecutionWorkspace, readExecutionText, requireExecutionWorkspace, sameExecutionWorkspace, workspaceChanged,
  type ExecutionWorkspace } from "./execution-workspace.js";
import { draftInput, draftPrompt, draftProposalTool, isPlanDraftTool, LegacyDraftTurn, readPlanTool, type DraftMode } from "./plan-draft-intent.js";
import { allowsPlanFileTool, isPlanFileTool, planTurnScope } from "./plan-turn.js";
import { checkPlanWorktree, matchesPlanCheckout, planFixtureTaskKind, planReportName } from "./plan.js";
import { workspaceFileError } from "./workspace-file.js";
import { isPaneTool } from "./pane-contract.js";
import { isPaneLaunchEntry, paneOnlyTurn } from "./pane-intent.js";
import { StartedThread, threadStartName, threadStartRequest } from "./thread-start.js";
import type { WorkspaceTrust } from "./workspace-trust.js";

export const reviewReadTool = "hush_review_read", reviewReportTool = "hush_review_report";
export const assignedToolPolicyKind = "hush.assigned-tool-policy";
const fixtureCheck = "hush_fixture_plan_check";
export type ReviewTarget = { kind: "plan"; planId: string; revision: number } | { kind: "code"; snapshotId: string; assignment?: string };
export type AssignedProfile = { kind: "read-only"; workspace: ExecutionWorkspace }
  | { kind: "subagent-write"; workspace: ExecutionWorkspace }
  | { kind: "worker"; workspace: ExecutionWorkspace; paths: string[] }
  | { kind: "reviewer"; target: ReviewTarget };

type Profile = AssignedProfile | { kind: "worker"; plan: { fileSet: boolean }; workspace: ExecutionWorkspace }
  | { kind: "worker"; fixture: true } | { kind: "planner"; mode: DraftMode; workspace: ExecutionWorkspace | null }
  | { kind: "default"; workspace: ExecutionWorkspace | null } | { kind: "disabled" };
const disabled = { kind: "disabled" } as const;

const Assigned = defineDoc<{ pin?: { owner: ConversationId; parent: ConversationId; task: TaskId; profile: AssignedProfile } }>({
  kind: assignedToolPolicyKind, version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

// Only host admission can call this, in the child-creation commit. The
// immutable owner edge is checked again at every dispatch.
export async function admitToolProfile(tx: Tx, child: ConversationRecord, profile: AssignedProfile) {
  if (!child.owner || child.parent) throw new Error("Tool profiles require a fresh task-owned conversation.");
  const doc = await tx.doc(Assigned, child.id);
  if (doc.pin) throw new Error("The task-owned tool profile is immutable.");
  doc.pin = { owner: child.id, parent: child.owner.conversationId, task: child.owner.taskId, profile: copyJson(profile) as AssignedProfile };
}

export async function assignedToolProfile(tx: Tx, child: ConversationRecord) {
  if (!child.owner || child.parent) return;
  const { pin } = await tx.doc(Assigned, child.id);
  if (pin?.owner === child.id && pin.parent === child.owner.conversationId && pin.task === child.owner.taskId) return pin.profile;
}

type TurnKey = { owner: ConversationId; first?: EntryId; continuation?: SubmissionId; cwd: string };
type TurnPin = TurnKey & { mode?: DraftMode; workspace: ExecutionWorkspace | null };
const Native = defineDoc<{ turn?: TurnPin }>({
  kind: "hush.turn-tool-policy", version: 1, scope: "conversation", history: "rewindable", fork: "asOf",
  initial: () => ({}), checkpointWhen: () => true,
});
const sameTurn = (a: TurnKey, b: TurnKey) => a.owner === b.owner && a.first === b.first && a.continuation === b.continuation;
function pinned({ mode, workspace }: TurnPin): Profile {
  const binding = workspace && { ...workspace };
  return mode ? { kind: "planner", mode, workspace: binding } : { kind: "default", workspace: binding };
}

export function allowsTool(profile: Profile, name: string, nested: boolean, chatAssignment = true) {
  if (isPaneTool(name) || name === threadStartName) return profile.kind === "default" && !nested && profile.workspace !== null;
  switch (profile.kind) {
    case "disabled": return false;
    case "reviewer": return !nested && profile.target.kind === "code" &&
      (name === reviewReadTool || name === reviewReportTool && !!profile.target.assignment);
    case "planner": return !nested && (name === "read" || name === readPlanTool || name === draftProposalTool(profile.mode));
    case "worker":
      if ("plan" in profile) return !nested && allowsPlanFileTool(name, profile.plan.fileSet);
      if ("fixture" in profile) return !nested && name === fixtureCheck;
      // falls through: read-only tools, with an additional per-path grant.
    case "read-only": return nested ? name === "inspect_file" : name === "read" || name === "codemode";
    case "subagent-write": return !nested && ["read", "bash", "edit", "write"].includes(name);
    case "default": return !isPlanDraftTool(name) && !isPlanFileTool(name) && name !== reviewReadTool && name !== reviewReportTool && name !== fixtureCheck &&
      (name !== planReportName || !nested && chatAssignment) && (!nested || name !== spawnReviewsName && name !== joinReviewsName) &&
      (name !== subagentToolName || !nested && profile.workspace !== null);
  }
}

function denial(profile: Profile, name: string) {
  if (profile.kind === "planner") return profile.mode === "preview" ? "Plan previewing only permits reading and showing the draft."
    : "Plan drafting only permits reading and saving the draft.";
  if (profile.kind === "default" && isPlanDraftTool(name)) return draftPrompt;
  if (profile.kind === "worker" && "plan" in profile) return "This plan turn only permits the assigned file and replacement.";
  if (isPlanFileTool(name)) return "No authorized plan task is running.";
  return "The effective tool profile does not allow this call.";
}
const failure = (text: string) => ({ output: [{ type: "text" as const, text }], isError: true });

type Caller = { generation: TaskId } | { tool: TaskId };
type Decision = { profile: Profile; nested: boolean } | { candidate: TurnKey & { mode?: DraftMode }; workspace?: ExecutionWorkspace | null; nested: boolean };
const fixed = (profile: Profile, nested = false): Decision => ({ profile, nested });

async function toolIsNested(tx: Tx, owner: ConversationId, run: LiveRun, id: TaskId) {
  let task = await tx.task(id);
  let nested = false;
  for (let depth = 0; depth < 16 && task?.kind === ToolTask.definition.name && task.conversationId === owner &&
      !task.abortRequested && task.state.status !== "terminal"; depth++) {
    if (task.owner === run.taskId) return nested;
    if (task.owner === undefined) break;
    nested = true;
    task = await tx.task(task.owner);
  }
}

async function placedDraft(tx: Tx, owner: ConversationId, run: LiveRun, fixture: boolean) {
  const first = run.first !== undefined && draftInput(await tx.entry(run.first), owner, fixture);
  if (first) return first.mode;
  for (const id of run.inputs) {
    const row = await tx.submission(id);
    if (row?.type !== "input" || row.status !== "placed" || row.conversationId !== owner || row.entry === run.first) continue;
    const draft = draftInput(await tx.entry(row.entry), owner, fixture);
    if (draft) return draft.mode;
  }
}

async function restrictedHistory(tx: Tx, conversation: ConversationRecord, fixture: boolean, saved?: TurnPin, before?: EntryId) {
  // Raw scans are not fork-aware. A fork at a draft's user entry can also
  // predate its first policy commit. Walk only the ancestry it can see, with
  // a total bound and no authority when that bound is exhausted.
  let current: ConversationRecord | undefined = conversation, at = before;
  for (let depth = 0; current && depth < 8; depth++) {
    const { items, next } = await tx.scanEntries({ conversationId: current.id, order: "descending", maxEntryId: at }, 64);
    const last = items.find(e => e.kind === "pi.reset" || e.model?.some(m => m.role === "user"));
    if (last) {
      if (last.kind === "pi.reset") return false;
      // Even if the last visible input was a non-drafting steer, a saved
      // planner policy means history alone is not a new admission.
      return !!saved?.mode || last.kind === planTaskKind || last.kind === planChatKind || isPaneLaunchEntry(last) || !!draftInput(last, last.conversationId, fixture);
    }
    if (next) return true;
    if (!current.parent) return false;
    at = at === undefined || current.parent.at < at ? current.parent.at : at;
    current = await tx.conversation(current.parent.conversationId);
  }
  return true;
}

export class ToolPolicy {
  constructor(private readonly harness: () => Harness, private readonly trust: WorkspaceTrust, private readonly directory: (cwd?: string) => string,
    private readonly protectedDirs: readonly string[], private readonly fixture: boolean) {}

  async assignedEnvironment(owner: ConversationId, cwd: string | undefined, read: Pick<Harness, "snapshot">, ctx: Context) {
    const pin = (await read.snapshot(Assigned, owner, ctx))?.pin;
    if (!pin) return undefined; // Ordinary environment rules apply.
    // Restricted file tools have their own I/O path. A writable child may
    // use the regular tools, but not construct an env with a replaced pin.
    if (pin.owner !== owner || pin.profile.kind !== "subagent-write" || this.directory(cwd) !== pin.profile.workspace.cwd) return null;
    await requireExecutionWorkspace(this.trust, pin.profile.workspace, ctx.abortSignal);
    return pin.profile.workspace;
  }

  async threadStartEnvironment(owner: ConversationId, cwd: string | undefined, read: Pick<Harness, "snapshot">, ctx: Context) {
    const pin = (await read.snapshot(StartedThread, owner, ctx))?.first;
    if (!pin || !(await read.snapshot(LiveDoc, owner, ctx))?.run?.inputs.includes(pin.submission)) return undefined;
    if (this.directory(cwd) !== pin.workspace.cwd) return null;
    await requireExecutionWorkspace(this.trust, pin.workspace, ctx.abortSignal);
    return pin.workspace;
  }

  async #decide(tx: Tx, owner: ConversationId, caller: Caller, legacy?: Readonly<{ first?: EntryId }>): Promise<Decision> {
    const conversation = await tx.conversation(owner);
    const run = (await tx.doc(LiveDoc, owner)).run;
    if (!conversation || !run) return fixed(disabled);
    const nested = "tool" in caller ? await toolIsNested(tx, owner, run, caller.tool) : run.taskId === caller.generation ? false : undefined;
    if (nested === undefined) return fixed(disabled);
    if (conversation.owner) {
      const task = await tx.task(conversation.owner.taskId);
      if (!task || task.abortRequested || task.state.status === "terminal") return fixed(disabled, nested);
      const profile = await assignedToolProfile(tx, conversation);
      if (profile) {
        if ("workspace" in profile && this.directory((await tx.doc(AgentDoc, owner)).cwd) !== profile.workspace.cwd) return fixed(disabled, nested);
        return fixed(copyJson(profile) as AssignedProfile, nested);
      }
      // Compatibility for the old faux plan fixture, not a production grant.
      if (this.fixture && task.kind === planFixtureTaskKind) return fixed({ kind: "worker", fixture: true }, nested);
      return fixed(disabled, nested);
    }
    if (!this.fixture) {
      const scope = await planTurnScope(tx, owner, caller);
      if (scope) return fixed(scope.turn && scope.turn.workspace.cwd === this.directory((await tx.doc(AgentDoc, owner)).cwd)
        ? { kind: "worker", plan: { fileSet: "files" in scope.turn.work }, workspace: { ...scope.turn.workspace } } : disabled, nested);
    }
    const cwd = this.directory((await tx.doc(AgentDoc, owner)).cwd);
    const first = run.first === undefined ? undefined : await tx.entry(run.first);
    const source = run.inputs[0] === undefined ? undefined : await tx.submission(run.inputs[0]);
    // A queued reset can be first in the run even though the run starts with
    // a newly placed user submission. The reset's handoff isn't that input.
    const placed = first?.conversationId === owner && ResetEntry.is(first) && source?.type === "input" &&
      source.status === "placed" && source.conversationId === owner && source.entry > first.id ? await tx.entry(source.entry) : first;
    const actualInput = placed?.conversationId === owner && placed.model?.some(m => m.role === "user") ? placed.id : undefined;
    const continuation = actualInput === undefined && source?.type === "continue" ? source.id : undefined;
    if (actualInput === undefined && continuation === undefined) return fixed(disabled, nested);
    // Durable fills run.first with the first assistant tool-call entry on an
    // inputless Continue. The original Continue submission stays stable.
    const turn: TurnKey = { owner, cwd, ...(continuation === undefined ? { first: actualInput! } : { continuation }) };
    const saved = (await tx.doc(Native, owner)).turn;
    const prior = saved && sameTurn(turn, saved) ? saved : undefined;
    if (prior && prior.cwd !== cwd) return fixed(disabled, nested);
    if (!prior && continuation !== undefined && await restrictedHistory(tx, conversation, this.fixture, saved, run.first)) return fixed(disabled, nested);
    // A placed steer can narrow default to planning. It cannot change a
    // preview to a save or replace the workspace bound to the active turn.
    const mode = prior?.mode ?? await placedDraft(tx, owner, run, this.fixture);
    if (prior && mode === prior.mode) return fixed(pinned(prior), nested);
    if (!prior && run.first !== undefined && legacy?.first === run.first) return fixed(disabled, nested);
    const launched = source?.type === "input" && source.requestId === threadStartRequest ? (await tx.doc(StartedThread, owner)).first : undefined;
    if (launched && launched.submission === source?.id) {
      if (launched.workspace.cwd !== cwd) return fixed(disabled, nested);
      return { candidate: { ...turn, ...(mode && { mode }) }, workspace: { ...launched.workspace }, nested };
    }
    return { candidate: { ...turn, ...(mode && { mode }) }, ...(prior && { workspace: prior.workspace && { ...prior.workspace } }), nested };
  }

  async #legacy(owner: ConversationId, ctx: Context) { return this.harness().snapshot(LegacyDraftTurn, owner, ctx); }

  async prepare(owner: ConversationId, generation: TaskId, ctx: Context): Promise<Profile> {
    const h = this.harness(), caller = { generation }, legacy = await this.#legacy(owner, ctx);
    const before = await h.commit(tx => this.#decide(tx, owner, caller, legacy), ctx);
    if ("profile" in before) return before.profile;
    const workspace = before.workspace !== undefined ? before.workspace
      : await bindExecutionWorkspace(this.trust, before.candidate.cwd, ctx.abortSignal).catch(() => null);
    ctx.abortSignal?.throwIfAborted();
    return h.commit(async tx => {
      const now = await this.#decide(tx, owner, caller, legacy);
      if ("profile" in now) return now.profile;
      if (!sameTurn(now.candidate, before.candidate) || now.candidate.cwd !== before.candidate.cwd) return disabled;
      const pin: TurnPin = { ...now.candidate, workspace: now.workspace === undefined ? workspace : now.workspace };
      (await tx.doc(Native, owner)).turn = pin;
      return pinned(pin);
    }, ctx);
  }

  async #authorize(name: string, api: ToolExecutionApi, ctx: Context) {
    const legacy = await this.#legacy(api.conversationId, ctx);
    const decide = async (tx: Tx) => !isPaneTool(name) && await paneOnlyTurn(tx, api.conversationId) ? fixed(disabled) :
      this.#decide(tx, api.conversationId, { tool: api.taskId }, legacy);
    const decision = await api.commit(decide, ctx);
    // A legacy default call may resume under its normal environment, but
    // durably bind that environment before any new execution. A missing
    // planner pin, including a just-placed steer, never admits a proposal.
    let profile: Profile = "profile" in decision ? decision.profile : disabled;
    if ("candidate" in decision && !decision.candidate.mode) {
      const workspace = decision.workspace === undefined
        ? await bindExecutionWorkspace(this.trust, decision.candidate.cwd, ctx.abortSignal).catch(() => null) : decision.workspace;
      ctx.abortSignal?.throwIfAborted();
      profile = await api.commit(async tx => {
        const now = await decide(tx);
        if ("profile" in now) return now.profile;
        if (now.candidate.mode || !sameTurn(now.candidate, decision.candidate) || now.candidate.cwd !== decision.candidate.cwd) return disabled;
        const pin = { ...now.candidate, workspace };
        (await tx.doc(Native, api.conversationId)).turn = pin;
        return pinned(pin);
      }, ctx);
    }
    if (!allowsTool(profile, name, decision.nested)) return { denied: denial(profile, name) } as const;
    if ("workspace" in profile) {
      try {
        if (!profile.workspace) return { denied: workspaceChanged } as const;
        if ((profile.kind === "default" || profile.kind === "subagent-write") && api.env?.cwd !== profile.workspace.root)
          return { denied: workspaceChanged } as const;
        await requireExecutionWorkspace(this.trust, profile.workspace, ctx.abortSignal);
      } catch {
        ctx.abortSignal?.throwIfAborted();
        return { denied: workspaceChanged } as const;
      }
    }
    return { profile } as const;
  }

  async threadStartWorkspace(api: ToolExecutionApi, ctx: Context) {
    const decision = await this.#authorize(threadStartName, api, ctx);
    return decision.profile?.kind === "default" ? decision.profile.workspace : undefined;
  }

  async confirmWorkspace(tx: Tx, owner: ConversationId, workspace: ExecutionWorkspace, ctx: Context) {
    const { root, device, inode, birth } = workspace;
    if (!await matchesPlanCheckout(tx, owner, root, `${device}:${inode}:${birth}`)) return false;
    try {
      await requireExecutionWorkspace(this.trust, workspace, ctx.abortSignal);
      return true;
    } catch { ctx.abortSignal?.throwIfAborted(); return false; }
  }

  // The durable pin is necessary, but revocation doesn't modify it. Check
  // the live grant and worktree binding again at the final admission point.
  async confirmDefaultWorkspace(tx: Tx, api: ToolExecutionApi, expected: ExecutionWorkspace, ctx: Context) {
    return await this.matchesDefaultWorkspace(tx, api, expected) && await this.confirmWorkspace(tx, api.conversationId, expected, ctx);
  }

  async bindThreadWorkspace(cwd: string, ctx: Context) {
    try { return await bindExecutionWorkspace(this.trust, this.directory(cwd), ctx.abortSignal); }
    catch { ctx.abortSignal?.throwIfAborted(); }
  }

  // Subagents consume the actual authorized binding, not a new grant for
  // whatever directory happens to occupy the cwd when the tool runs.
  async subagentWorkspace(api: ToolExecutionApi, ctx: Context) {
    const decision = await this.#authorize(subagentToolName, api, ctx);
    return decision.profile?.kind === "default" ? decision.profile.workspace : undefined;
  }

  async paneWorkspace(name: string, api: ToolExecutionApi, ctx: Context) {
    if (!isPaneTool(name)) return;
    const decision = await this.#authorize(name, api, ctx);
    const workspace = decision.profile?.kind === "default" ? decision.profile.workspace : undefined;
    if (!workspace) return;
    try {
      await checkPlanWorktree(this.harness(), api.conversationId, workspace.root, ctx);
      await requireExecutionWorkspace(this.trust, workspace, ctx.abortSignal);
      return workspace;
    } catch { ctx.abortSignal?.throwIfAborted(); }
  }

  async matchesSubagentWorkspace(tx: Tx, api: ToolExecutionApi, expected: ExecutionWorkspace) {
    return !await paneOnlyTurn(tx, api.conversationId) && this.matchesDefaultWorkspace(tx, api, expected);
  }

  async matchesDefaultWorkspace(tx: Tx, api: ToolExecutionApi, expected: ExecutionWorkspace) {
    const value = await this.#decide(tx, api.conversationId, { tool: api.taskId });
    return !value.nested && "profile" in value && value.profile.kind === "default" && !!value.profile.workspace &&
      sameExecutionWorkspace(value.profile.workspace, expected);
  }

  #protect(tool: ToolRegistration): ToolRegistration {
    const { name } = tool;
    return { ...tool, execute: async (args, api, ctx) => {
      const decision = await this.#authorize(name, api, ctx);
      if (decision.denied !== undefined) return failure(decision.denied);
      const { profile } = decision;
      if ((name === "read" || name === "inspect_file") && profile.kind !== "default" && profile.kind !== "subagent-write" && "workspace" in profile) {
        const { path, offset, limit } = args as { path: string; offset?: number; limit?: number };
        const w = profile.workspace!;
        // Direct read retains line slicing, but restricted profiles do not
        // use the general coding read's path resolution or image ingestion.
        if (!allowedExecutionPath(w.root, path, this.protectedDirs) || "paths" in profile && !profile.paths.includes(path))
          return failure(workspaceFileError);
        try {
          const { text } = await readExecutionText(this.trust, w, path, ctx.abortSignal);
          if (name === "inspect_file") return { output: [], structuredOutput: text };
          const start = Math.max(0, Math.trunc(offset ?? 1) - 1);
          const selected = start || limit !== undefined ? text.split("\n").slice(start, limit === undefined ? undefined : start + Math.max(1, Math.trunc(limit))).join("\n") : text;
          return { output: [{ type: "text", text: selected }], structuredOutput: selected };
        } catch {
          ctx.abortSignal?.throwIfAborted();
          return failure(workspaceFileError);
        }
      }
      const output = await tool.execute(args, api, ctx);
      if (name === "codemode") {
        // Codemode can deliver committed nested results from its replay
        // journal without dispatching that inner tool again.
        const current = await this.#authorize(name, api, ctx);
        if (current.denied !== undefined) return failure(current.denied);
      }
      return output;
    } };
  }

  protect(extension: Extension): Extension {
    // Register guarded definitions, not an agent-selected beforeTool hook:
    // Pi does not rerun that hook after execute intent or an execute park.
    return extension.tools?.length ? { ...extension, tools: extension.tools.map(tool => this.#protect(tool)) } : extension;
  }
}
