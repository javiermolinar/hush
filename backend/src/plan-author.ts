import type { Context } from "@earendil-works/chord/context";
import { getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import { defineDoc, defineExtension, defineTool, GenerationTask, hook, LiveDoc, ToolTask, UserEntry,
  type ConversationId, type EntryId, type EntryRecord, type Harness, type Tx } from "@earendil-works/pi-durable";
import { environmentInputKind } from "../../server/src/client-environment.js";
import { planFilePath, planFilePathPattern, planScopeFileLimit, planScopeWriteLimit, planScriptLimit, planStepID, planStepLimit, planThinkingLevels, type PlanStep } from "../../server/src/plan.js";
import { WireError } from "../../server/src/wire.js";
import type { PlanFiles } from "./plan-files.js";
import { allowsPlanFileTool, inspectPlanTurn, isPlanFileTool, planTaskInstructions, planTurnTools } from "./plan-turn.js";
import { currentPlan, planRevisionLimit, proposePlan, type PlanSources } from "./plan.js";

const readTool = "hush_plan_current", writeTool = "hush_plan_propose";
const planTool = (name: string) => name === readTool || name === writeTool;
const draftTools = new Set(["read", readTool, writeTool]);
const result = (text: string) => ({ output: [{ type: "text" as const, text }] });
const failed = (text: string) => ({ ...result(text), isError: true });
const workspaceUnavailable = "The workspace is not authorized for plan drafting.";
const draftPrompt = "Start your request with ‘Create a plan …’ or ‘Revise the plan …’ to save a draft in /plan.";
const advertised = `${draftPrompt} Saving a plan does not authorize implementation.`;
const protectedPrompt = `This request is for a draft only. You may inspect files to draft it. Do not implement, run commands, or edit files. Use ${readTool} to read the current plan before replacing a revision. Use baseRevision 0 for a new plan. A closed or completed plan cannot be revised. Create a separate plan only if the user asked for a new one. Save structured tasks and acceptance checks with ${writeTool}, not in Markdown or as chat text alone. If one task can be done by one exact replacement in an existing small text file, specify edit.path (workspace-relative) and edit.contains (literal text that should be newly present after the edit). Use it only when a literal check is sufficient. Alternatively declare scope.write (1–4 existing files), optional scope.read (read-only inputs, maximum 8 files total), and scope.acceptance (label and JavaScript body). It receives the frozen files object keyed by exact path, without tools, shell, imports, or network. It must return false on the original files and true on the proposed contents. No edits to undeclared paths, duplicate paths, or file creation. Do not use scope for checks that need the real workspace, a project test runner or a service; leave such tasks as drafts. Once saved, tell the user to open /plan. If the save conflicts, report that the plan changed rather than overwriting it.`;

// Recognize a small set of explicit draft requests, not arbitrary intent or
// phrases inside workspace/tool text. Plan: is the unambiguous form.
const explicitDraft = /^(?:(?:can|could|would|will) you\s+)?(?:please[ ,]+)?(?:plan\s*:|(?:create|draft|write|make|prepare|outline|propose|revise|update|amend|edit)\s+(?:(?:a|an|the|this|my|our)\s+)?(?:existing\s+)?plan(?=$|\s|:|[.,?!](?:\s|$)))/i;
const DraftTurn = defineDoc<{ first?: EntryId }>({
  kind: "hush.plan-draft-turn", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

function draftInput(entry: EntryRecord | undefined, owner: ConversationId, fixture: boolean) {
  if (!entry || entry.conversationId !== owner || !UserEntry.is(entry) && entry.kind !== environmentInputKind) return;
  const msg = entry.model?.find(m => m.role === "user");
  if (!msg) return;
  const text = typeof msg.content === "string" ? msg.content : msg.content.filter(b => b.type === "text").map(b => b.text).join("\n");
  const request = text.trimStart();
  if (!explicitDraft.test(request) && !(fixture && (request === "!plan-fixture" || request === "!plan-fixture-revise"))) return;
  const limit = 3000;
  return { entryId: String(entry.id), text: text.slice(0, limit).toWellFormed(),
    partial: text.length > limit || typeof msg.content !== "string" && msg.content.some(b => b.type !== "text") };
}

async function draftSources(tx: Tx, owner: ConversationId, fixture: boolean): Promise<PlanSources> {
  const { run } = await tx.doc(LiveDoc, owner);
  const ids = new Set<EntryId>();
  if (run?.first !== undefined) ids.add(run.first);
  for (const id of run?.inputs ?? []) {
    const row = await tx.submission(id);
    if (row?.type === "input" && row.status === "placed" && row.conversationId === owner) ids.add(row.entry);
  }
  const inputs: PlanSources["inputs"] = [];
  let partial = false;
  // Never read tool output or unrelated chat as source requirements, even if
  // the drafting model used that context.
  for (const id of ids) {
    const source = draftInput(await tx.entry(id), owner, fixture);
    if (!source) continue;
    inputs.push({ entryId: source.entryId, text: source.text });
    partial ||= source.partial;
  }
  return { inputs, partial: partial || inputs.length === 0 };
}

// Use only placed inputs. Queued work cannot change the currently running
// turn. Remember a recognized run by its first entry so later Continue
// keeps the restriction even when the draft prompt arrived via steer.
async function drafting(h: Harness, owner: ConversationId, fixture: boolean, ctx: Context) {
  const prior = await h.snapshot(DraftTurn, owner, ctx);
  return h.commit(async tx => {
    const { run } = await tx.doc(LiveDoc, owner);
    if (!run?.first) return false;
    if (run.first === prior?.first) return true;
    let matched = !!draftInput(await tx.entry(run.first), owner, fixture);
    for (const id of run.inputs) {
      if (matched) break;
      const row = await tx.submission(id);
      if (row?.type === "input" && row.status === "placed" && row.conversationId === owner && row.entry !== run.first) {
        matched = !!draftInput(await tx.entry(row.entry), owner, fixture);
      }
    }
    if (matched) (await tx.doc(DraftTurn, owner)).first = run.first;
    return matched;
  }, ctx);
}

const stepID = { type: "string", pattern: "^[A-Za-z0-9_-]+$", maxLength: 64 } as const;
const text = (maxLength: number) => ({ type: "string", minLength: 1, maxLength, pattern: "\\S" } as const);
const paths = (max: number, min: number) => ({ type: "array", minItems: min, maxItems: max, uniqueItems: true,
  items: { type: "string", maxLength: 160, pattern: planFilePathPattern } }) as const;
const draft = { type: "object", additionalProperties: false, required: ["baseRevision", "title", "steps"], properties: {
  baseRevision: { type: "integer", minimum: 0, maximum: planRevisionLimit }, title: text(160),
  steps: { type: "array", minItems: 1, maxItems: planStepLimit, items: {
    type: "object", additionalProperties: false, required: ["id", "title", "description", "checks"], properties: {
      id: stepID, title: text(160), description: text(1024),
      checks: { type: "array", minItems: 1, maxItems: 6, items: text(160) },
      requires: { type: "array", maxItems: planStepLimit, uniqueItems: true, items: stepID }, after: stepID,
      blocker: text(160), thinking: { type: "string", enum: planThinkingLevels },
      edit: { type: "object", additionalProperties: false, required: ["path", "contains"], properties: {
        path: { type: "string", maxLength: 160, pattern: planFilePathPattern }, contains: text(160),
      } },
      scope: { type: "object", additionalProperties: false, required: ["write", "acceptance"], properties: {
        read: paths(planScopeFileLimit - 1, 0), write: paths(planScopeWriteLimit, 1),
        acceptance: { type: "object", additionalProperties: false, required: ["label", "script"], properties: {
          label: text(120), script: text(planScriptLimit),
        } },
      } },
    },
  } },
} } as const;

function ordered(steps: readonly PlanStep[]) {
  const prior = new Set<string>();
  for (const step of steps) {
    if (!planStepID(step.id) || prior.has(step.id) || step.requires?.some(dep => !prior.has(dep)) ||
        step.after !== undefined && !prior.has(step.after)) return false;
    prior.add(step.id);
  }
  return true;
}

export function planAuthor(harness: () => Harness, directory: (cwd?: string) => string, fixture: boolean, files?: PlanFiles) {
  const current = defineTool({
    name: readTool, description: "Read the saved plan, base revision and whether the revision is pinned. Does not edit or run anything.",
    parameters: { type: "object", properties: {}, additionalProperties: false } as const,
    callers: ["model"], replay: "safe", outputLimits: { maxBytes: 512 * 1024 },
    async execute(_args, api, ctx) {
      if (!api.env) return failed(workspaceUnavailable);
      return api.commit(async tx => {
        const value = await currentPlan(tx, api.conversationId);
        return result(JSON.stringify({ ...value, baseRevision: value.plan?.revision ?? 0 }));
      }, ctx);
    },
  });
  const propose = defineTool({
    name: writeTool, description: "Save a structured draft for an explicit /plan drafting request. Use baseRevision 0 for a new plan. Cannot start implementation.",
    parameters: draft, callers: ["model"], replay: "unsafe",
    async execute(args, api, ctx) {
      if (!api.env) return failed(workspaceUnavailable);
      const { baseRevision, title, steps } = args as { baseRevision: number; title: string; steps: PlanStep[] };
      if (!ordered(steps)) return failed("Step IDs must be unique; prerequisites must appear earlier.");
      if (steps.some(s => s.edit && (!planFilePath(s.edit.path) || !s.edit.contains.isWellFormed() || s.edit.contains.includes("\0")))) {
        return failed("Use a workspace-relative file and a valid literal check.");
      }
      for (const { scope, edit } of steps) {
        if (!scope) continue;
        const paths = [...scope.write, ...scope.read ?? []];
        const { script, label } = scope.acceptance;
        if (edit || paths.length > planScopeFileLimit || new Set(paths).size !== paths.length || !paths.every(planFilePath) ||
            !script.isWellFormed() || script.includes("\0") || Buffer.byteLength(script) > planScriptLimit ||
            !label.isWellFormed() || /[\p{Cc}\p{Zl}\p{Zp}]/u.test(label)) {
          return failed("Use one file scope with unique bounded paths and a bounded acceptance script, or a literal edit, not both.");
        }
      }
      try {
        const revision = await api.commit(async tx => {
          const sources = fixture ? { inputs: [], partial: true } : await draftSources(tx, api.conversationId, fixture);
          return proposePlan(tx, api.conversationId, directory, fixture, baseRevision, title, steps, sources);
        }, ctx);
        return result(`Saved ${title} (r${revision}). Open /plan to review the draft.`);
      } catch (error) {
        if (!(error instanceof WireError)) throw error;
        if (error.code === "invalid_request") return failed(error.message);
        if (error.code === "busy") return failed("A run is active. The plan was not changed.");
        if (error.code === "limit") return failed("The plan revision limit was reached.");
        throw error;
      }
    },
  });
  return defineExtension({ name: "hush-plan-author", tools: [current, propose, ...(files ? planTurnTools(files) : [])], hooks: [
    hook(GenerationTask, { async beforeRequest({ messages }, api, ctx) {
      const tools = getCurrentTools(messages);
      const scope = fixture ? undefined : await inspectPlanTurn(harness(), api.conversationId, { generation: api.taskId }, ctx);
      if (!scope && !tools.some(t => planTool(t.name) || isPlanFileTool(t.name))) return;
      const draft = !scope && await drafting(harness(), api.conversationId, fixture, ctx);
      const lastSystem = messages.findLastIndex(m => m.role === "system");
      const msg = messages[lastSystem];
      if (msg?.role !== "system") return; // Offering a tool requires a system entry.
      const visible = (t: { name: string }) => scope ? scope.authorized && allowsPlanFileTool(t.name, scope.fileSet)
        : draft ? draftTools.has(t.name) : !planTool(t.name) && !isPlanFileTool(t.name);
      const instruction = scope ? scope.authorized ? planTaskInstructions(scope.fileSet) : "This plan task no longer has authority. Do not take further actions."
        : draft ? `${advertised}\n\n${protectedPrompt}` : advertised;
      // Durable dispatch uses the stored loadout. Filter the request and also
      // check the scope when the tool is dispatched, including nested calls.
      return { messages: messages.with(lastSystem, { ...msg, sections: { ...msg.sections, hushPlan: instruction },
        toolsAdded: msg.toolsAdded?.filter(visible),
        toolsRemoved: [...msg.toolsRemoved ?? [], ...tools.filter(t => !visible(t)).map(t => ({ name: t.name }))],
      }) };
    } }),
    hook(ToolTask, { async beforeTool(call, api, ctx) {
      const scope = fixture ? undefined : await inspectPlanTurn(harness(), api.conversationId, { tool: api.taskId }, ctx);
      if (scope) {
        if (!scope.authorized || !allowsPlanFileTool(call.name, scope.fileSet) || call.parent) return { block: "This plan turn only permits the assigned file and replacement." };
        return;
      }
      if (isPlanFileTool(call.name)) return { block: "No authorized plan task is running." };
      if (call.name === "read") return;
      const draft = await drafting(harness(), api.conversationId, fixture, ctx);
      if (draft && !draftTools.has(call.name)) return { block: "Plan drafting only permits reading and saving the draft." };
      if (!draft && planTool(call.name)) return { block: draftPrompt };
    } }),
  ] });
}
