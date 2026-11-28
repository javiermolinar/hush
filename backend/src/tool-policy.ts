import { join } from "node:path";
import { copyJson } from "@earendil-works/chord/json";
import type { Context } from "@earendil-works/chord/context";
import { AgentDoc, defineDoc, LiveDoc, ResetEntry, ToolTask, type ConversationId, type ConversationRecord, type EntryId,
  type Extension, type Harness, type LiveRun, type SubmissionId, type TaskId, type ToolExecutionApi, type ToolRegistration, type Tx } from "@earendil-works/pi-durable";
import { planChatKind, planTaskKind } from "../../server/src/plan.js";
import { subagentToolName } from "../../server/src/subagents.js";
import { allowedExecutionPath, bindExecutionWorkspace, delegatePath, readExecutionText, requireExecutionWorkspace, requireSubdirectory,
  sameExecutionWorkspace, workspaceChanged, type ExecutionWorkspace } from "./execution-workspace.js";
import { delegateGitName } from "./delegate-git.js";
import { isPlanTool } from "./plan-author.js";
import { workspaceFileError } from "./workspace-file.js";
import { isPaneTool } from "./pane-contract.js";
import { isPaneLaunchEntry, paneOnlyTurn } from "./pane-intent.js";
import { StartedThread, threadStartName, threadStartRequest } from "./thread-start.js";
import type { WorkspaceTrust } from "./workspace-trust.js";

export const assignedToolPolicyKind = "hush.assigned-tool-policy";
// `pwd` is a subagent's working directory relative to its checkout. It is
// authorized by the checkout's binding, not by a grant of its own.
export type AssignedProfile = { kind: "read-only"; workspace: ExecutionWorkspace; pwd?: string }
  | { kind: "subagent-write"; workspace: ExecutionWorkspace; pwd?: string }
  | { kind: "worker"; workspace: ExecutionWorkspace; paths: string[] };
// Profiles stored by removed features, such as review delegation's
// `reviewer`, match no case below and allow no tools.

type Profile = AssignedProfile | { kind: "default"; workspace: ExecutionWorkspace | null } | { kind: "disabled" };
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

const assignedCwd = (profile: AssignedProfile & { workspace: ExecutionWorkspace }) =>
  "pwd" in profile && profile.pwd ? join(profile.workspace.cwd, profile.pwd) : profile.workspace.cwd;

export async function assignedToolProfile(tx: Tx, child: ConversationRecord) {
  if (!child.owner || child.parent) return;
  const { pin } = await tx.doc(Assigned, child.id);
  if (pin?.owner === child.id && pin.parent === child.owner.conversationId && pin.task === child.owner.taskId) return pin.profile;
}

type TurnKey = { owner: ConversationId; first?: EntryId; continuation?: SubmissionId; cwd: string };
// `mode` was set by the removed restricted planning turn. A turn pinned with
// it keeps no tools rather than gaining the default set.
type TurnPin = TurnKey & { mode?: string; workspace: ExecutionWorkspace | null };
const LegacyDraftTurn = defineDoc<{ first?: EntryId }>({
  kind: "hush.plan-draft-turn", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});
const Native = defineDoc<{ turn?: TurnPin }>({
  kind: "hush.turn-tool-policy", version: 1, scope: "conversation", history: "rewindable", fork: "asOf",
  initial: () => ({}), checkpointWhen: () => true,
});
const sameTurn = (a: TurnKey, b: TurnKey) => a.owner === b.owner && a.first === b.first && a.continuation === b.continuation;
function pinned({ mode, workspace }: TurnPin): Profile {
  const binding = workspace && { ...workspace };
  return mode ? disabled : { kind: "default", workspace: binding };
}

export function allowsTool(profile: Profile, name: string, nested: boolean) {
  if (isPaneTool(name) || name === threadStartName) return profile.kind === "default" && !nested && profile.workspace !== null;
  switch (profile.kind) {
    case "disabled": return false;
    case "worker": // Read-only tools, with an additional per-path grant.
    case "read-only": return nested ? name === "inspect_file" : name === "read" || name === "codemode" ||
      profile.kind === "read-only" && name === delegateGitName;
    case "subagent-write": return !nested && ["read", "bash", "edit", "write"].includes(name);
    case "default": return name !== delegateGitName && (!isPlanTool(name) || !nested) &&
      (name !== subagentToolName || !nested && profile.workspace !== null);
    default: return false;
  }
}

const denial = "The effective tool profile does not allow this call.";
const failure = (text: string) => ({ output: [{ type: "text" as const, text }], isError: true });

type Caller = { generation: TaskId } | { tool: TaskId };
type Decision = { profile: Profile; nested: boolean } | { candidate: TurnKey; workspace?: ExecutionWorkspace | null; nested: boolean };
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

async function restrictedHistory(tx: Tx, conversation: ConversationRecord, saved?: TurnPin, before?: EntryId) {
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
      return !!saved?.mode || last.kind === planTaskKind || last.kind === planChatKind || isPaneLaunchEntry(last);
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
    private readonly protectedDirs: readonly string[], private readonly checkouts?: string) {}

  async assignedEnvironment(owner: ConversationId, cwd: string | undefined, read: Pick<Harness, "snapshot">, ctx: Context) {
    const pin = (await read.snapshot(Assigned, owner, ctx))?.pin;
    if (!pin) return undefined; // Ordinary environment rules apply.
    // Restricted file tools have their own I/O path. A writable child may
    // use the regular tools, but not construct an env with a replaced pin.
    if (pin.owner !== owner || pin.profile.kind !== "subagent-write" || this.directory(cwd) !== assignedCwd(pin.profile)) return null;
    await requireExecutionWorkspace(this.trust, pin.profile.workspace, ctx.abortSignal);
    return requireSubdirectory(pin.profile.workspace, pin.profile.pwd);
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
        if ("workspace" in profile && this.directory((await tx.doc(AgentDoc, owner)).cwd) !== assignedCwd(profile)) return fixed(disabled, nested);
        return fixed(copyJson(profile) as AssignedProfile, nested);
      }
      return fixed(disabled, nested);
    }
    const cwd = this.directory((await tx.doc(AgentDoc, owner)).cwd);
    const first = run.first === undefined ? undefined : await tx.entry(run.first);
    // A scoped plan turn from before the upgrade lost its file tools; it
    // must not continue with the thread's normal tools.
    if (first?.conversationId === owner && first.kind === planTaskKind) return fixed(disabled, nested);
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
    if (!prior && continuation !== undefined && await restrictedHistory(tx, conversation, saved, run.first)) return fixed(disabled, nested);
    // A placed steer cannot replace the workspace bound to the active turn.
    if (prior) return fixed(pinned(prior), nested);
    if (!prior && run.first !== undefined && legacy?.first === run.first) return fixed(disabled, nested);
    const launched = source?.type === "input" && source.requestId === threadStartRequest ? (await tx.doc(StartedThread, owner)).first : undefined;
    if (launched && launched.submission === source?.id) {
      if (launched.workspace.cwd !== cwd) return fixed(disabled, nested);
      return { candidate: { ...turn }, workspace: { ...launched.workspace }, nested };
    }
    return { candidate: { ...turn }, nested };
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
    // durably bind that environment before any new execution.
    let profile: Profile = "profile" in decision ? decision.profile : disabled;
    if ("candidate" in decision) {
      const workspace = decision.workspace === undefined
        ? await bindExecutionWorkspace(this.trust, decision.candidate.cwd, ctx.abortSignal).catch(() => null) : decision.workspace;
      ctx.abortSignal?.throwIfAborted();
      profile = await api.commit(async tx => {
        const now = await decide(tx);
        if ("profile" in now) return now.profile;
        if (!sameTurn(now.candidate, decision.candidate) || now.candidate.cwd !== decision.candidate.cwd) return disabled;
        const pin = { ...now.candidate, workspace };
        (await tx.doc(Native, api.conversationId)).turn = pin;
        return pinned(pin);
      }, ctx);
    }
    if (!allowsTool(profile, name, decision.nested)) return { denied: denial } as const;
    if ("workspace" in profile) {
      try {
        if (!profile.workspace) return { denied: workspaceChanged } as const;
        const root = profile.kind === "subagent-write" && profile.pwd ? join(profile.workspace.root, profile.pwd) : profile.workspace.root;
        if ((profile.kind === "default" || profile.kind === "subagent-write") && api.env?.cwd !== root)
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

  async confirmWorkspace(workspace: ExecutionWorkspace, ctx: Context) {
    try {
      await requireExecutionWorkspace(this.trust, workspace, ctx.abortSignal);
      return true;
    } catch { ctx.abortSignal?.throwIfAborted(); return false; }
  }

  // The durable pin is necessary, but revocation doesn't modify it. Check
  // the live grant and worktree binding again at the final admission point.
  async confirmDefaultWorkspace(tx: Tx, api: ToolExecutionApi, expected: ExecutionWorkspace, ctx: Context) {
    return await this.matchesDefaultWorkspace(tx, api, expected) && await this.confirmWorkspace(expected, ctx);
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

  // A read-only subagent's checkout and working directory, for its own tools.
  async readOnlyWorkspace(name: string, api: ToolExecutionApi, ctx: Context) {
    const decision = await this.#authorize(name, api, ctx);
    return decision.profile?.kind === "read-only" ? { workspace: decision.profile.workspace, pwd: decision.profile.pwd } : undefined;
  }

  async paneWorkspace(name: string, api: ToolExecutionApi, ctx: Context) {
    if (!isPaneTool(name)) return;
    const decision = await this.#authorize(name, api, ctx);
    const workspace = decision.profile?.kind === "default" ? decision.profile.workspace : undefined;
    if (!workspace) return;
    try {
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
        const { offset, limit } = args as { path: string; offset?: number; limit?: number };
        const w = profile.workspace!, path = "pwd" in profile && profile.pwd ? delegatePath(profile.pwd, (args as { path: unknown }).path) : (args as { path: unknown }).path;
        // Direct read retains line slicing, but restricted profiles do not
        // use the general coding read's path resolution or image ingestion.
        if (!allowedExecutionPath(w.root, path, this.protectedDirs, this.checkouts) || "paths" in profile && !profile.paths.includes(path))
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
