import { getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import { defineExtension, defineTool, GenerationTask, hook, type Harness } from "@earendil-works/pi-durable";
import { planFilePath, planFilePathPattern, planScopeFileLimit, planScopeWriteLimit, planScriptLimit, planStepID, planStepLimit, planThinkingLevels, type PlanStep } from "../../server/src/plan.js";
import { WireError } from "../../server/src/wire.js";
import type { PlanFiles } from "./plan-files.js";
import { planTaskInstructions, planTurnTools } from "./plan-turn.js";
import { draftPrompt, draftProposalTool, draftSources, readPlanTool, type DraftMode } from "./plan-draft-intent.js";
import { allowsTool, type ToolPolicy } from "./tool-policy.js";
import { previewPlan, type DraftProposal } from "./plan-draft.js";
import { currentPlan, hasChatAssignment, planReportName, planReportTool, planRevisionLimit, proposePlan } from "./plan.js";

const result = (text: string) => ({ output: [{ type: "text" as const, text }] });
const failed = (text: string) => ({ ...result(text), isError: true });
const workspaceUnavailable = "The workspace is not authorized for plan drafting.";
const advertised = `${draftPrompt} Start with ‘Preview a plan …’ to show a draft without saving it. Saving a plan does not authorize implementation. Do not write a Markdown file as a substitute for saving to /plan.`;
const protectedPrompt = (mode: DraftMode) => `This request is for a draft only. Use workspace-relative paths to inspect small UTF-8 files. Do not implement, run commands, or edit files. Use ${readPlanTool} to read the current plan before replacing a revision. Use baseRevision 0 for a new plan. A closed or completed plan cannot be revised. Create a separate plan only if the user asked for a new one. ${mode === "preview" ? "Show" : "Save"} structured tasks and acceptance checks with ${draftProposalTool(mode)}, not in Markdown or as chat text alone. Describe outcomes and relevant checks in prose. Use dependencies and blockers only as helpful context. Tasks can run in chat with normal tools without declaring file scopes. If the user asks for isolated, verified edits, optionally specify edit.path and edit.contains for one exact replacement in an existing small text file, or scope.write (1–4 existing files), optional scope.read (maximum 8 files total), and scope.acceptance (label and JavaScript body). That body receives frozen files keyed by path, without tools, shell, imports, or network, and must return false before and true after. Isolated scopes cannot create files, write undeclared paths, or run project tests. ${mode === "preview" ? "After showing a preview, say it is not saved and point to Save to /plan. If that action is unavailable, ask the user to start a new message with ‘Create a plan from the preview above’." : "Once saved, tell the user to open /plan. If the save conflicts, report that the plan changed rather than overwriting it."}`;

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

export function planAuthor(harness: () => Harness, directory: (cwd?: string) => string, fixture: boolean, policy: ToolPolicy, files?: PlanFiles) {
  const current = defineTool({
    name: readPlanTool, description: "Read the saved plan, base revision and whether the revision is pinned. Does not edit or run anything.",
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
  const author = (mode: DraftMode) => defineTool({
    name: draftProposalTool(mode), description: mode === "preview"
      ? "Show an unsaved structured plan preview in chat. Use baseRevision 0 for a new plan. The user can save it after inspecting it."
      : "Save a structured draft for an explicit /plan drafting request. Use baseRevision 0 for a new plan. Cannot start implementation.",
    parameters: draft, callers: ["model"], replay: mode === "preview" ? "safe" : "unsafe",
    async execute(args, api, ctx) {
      if (!api.env) return failed(workspaceUnavailable);
      const proposal = args as DraftProposal;
      const { baseRevision, title, steps } = proposal;
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
        return await api.commit(async tx => {
          const sources = fixture ? { inputs: [], partial: true } : await draftSources(tx, api.conversationId, fixture);
          if (mode === "preview") {
            return result(await previewPlan(tx, api.conversationId, directory, proposal, sources)
              ? "Plan preview ready. It is not saved. Use Save to /plan or say ‘ok save it’ in the TUI."
              : "A preview is already available for this turn. It was not replaced.");
          }
          const { revision } = await proposePlan(tx, api.conversationId, directory, fixture, baseRevision, title, steps, sources);
          return result(`Saved ${title} (r${revision}). Open /plan to review the draft.`);
        }, ctx);
      } catch (error) {
        if (!(error instanceof WireError)) throw error;
        if (error.code === "invalid_request") return failed(error.message);
        if (error.code === "busy") return failed("A run is active. The plan was not changed.");
        if (error.code === "limit") return failed("The plan revision limit was reached.");
        throw error;
      }
    },
  });
  return defineExtension({ name: "hush-plan-author", tools: [current, author("save"), author("preview"),
    ...(!fixture ? [planReportTool] : []), ...(files ? planTurnTools(files) : [])], hooks: [
    hook(GenerationTask, { async beforeRequest({ messages }, api, ctx) {
      const tools = getCurrentTools(messages);
      const effective = await policy.prepare(api.conversationId, api.taskId, ctx);
      const chat = effective.kind === "default" && tools.some(t => t.name === planReportName) && await hasChatAssignment(harness(), api.conversationId, ctx);
      const lastSystem = messages.findLastIndex(m => m.role === "system");
      const msg = messages[lastSystem];
      if (msg?.role !== "system") return; // Offering a tool requires a system entry.
      const visible = (t: { name: string }) => allowsTool(effective, t.name, false, chat);
      const instruction = effective.kind === "planner" ? `${advertised}\n\n${protectedPrompt(effective.mode)}`
        : effective.kind === "worker" && "plan" in effective ? planTaskInstructions(effective.plan.fileSet)
        : effective.kind === "disabled" ? "This turn has no tool authority. Do not take further actions." : advertised;
      return { messages: messages.with(lastSystem, { ...msg, sections: { ...msg.sections, hushPlan: instruction },
        toolsAdded: msg.toolsAdded?.filter(visible),
        toolsRemoved: [...msg.toolsRemoved ?? [], ...tools.filter(t => !visible(t)).map(t => ({ name: t.name }))],
      }) };
    } }),
  ] });
}
