import { getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import { defineExtension, defineTool, GenerationTask, hook, type Harness } from "@earendil-works/pi-durable";
import { planStepID, planStepLimit, planThinkingLevels, type PlanOutcome, type PlanStepDefinition } from "../../server/src/plan.js";
import { WireError } from "../../server/src/wire.js";
import { allowsTool, type ToolPolicy } from "./tool-policy.js";
import { inPlanRun, planPromptBytes, planRevisionLimit, reportSteps, savePlan, workspacePlan, workspacePlans, type PlanDraft } from "./plan.js";

const result = (text: string) => ({ output: [{ type: "text" as const, text }] });
const failed = (text: string) => ({ ...result(text), isError: true });
export const readPlanTool = "plan_read", savePlanTool = "plan_save", reportPlanTool = "plan_report";
export const isPlanTool = (name: string) => name === readPlanTool || name === savePlanTool || name === reportPlanTool;
const workspaceUnavailable = "The workspace is not authorized for plans.";
const advertised = `When the user asks for a plan, save it with ${savePlanTool}; don't ask for special wording. To revise a plan, read it with ` +
  `${readPlanTool} (without planId it lists this workspace's plans) and pass its planId and revision as baseRevision. Write each step's prompt ` +
  "as a self-contained outcome with how to check it. Give a step a delegate specification when a subagent should do it, for example " +
  "{ read_only: true } for a review. Done steps stay done; to redo work, add a step with a new ID. Saving a plan does not authorize " +
  "implementation: after saving, tell the user to open /plan, and don't start the work unless asked. If the save conflicts, report that " +
  "the plan changed rather than overwriting it. Do not write a Markdown file as a substitute for saving to /plan.";

const stepID = { type: "string", pattern: "^[A-Za-z0-9_-]+$", maxLength: 64 } as const;
const text = (maxLength: number) => ({ type: "string", minLength: 1, maxLength, pattern: "\\S" } as const);
const saveParameters = { type: "object", additionalProperties: false, required: ["baseRevision", "title", "steps"], properties: {
  planId: { type: "string", minLength: 1, maxLength: 64 },
  baseRevision: { type: "integer", minimum: 0, maximum: planRevisionLimit }, title: text(160),
  steps: { type: "array", minItems: 1, maxItems: planStepLimit, items: {
    type: "object", additionalProperties: false, required: ["id", "title", "prompt"], properties: {
      id: stepID, title: text(160), prompt: text(planPromptBytes),
      requires: { type: "array", maxItems: planStepLimit, uniqueItems: true, items: stepID },
      delegate: { type: "object", additionalProperties: false, properties: {
        read_only: { type: "boolean" }, worktree: { type: "string", minLength: 1, maxLength: 255 },
        pwd: { type: "string", minLength: 1, maxLength: 1024 }, thinking: { type: "string", enum: planThinkingLevels },
      } },
    },
  } },
} } as const;

const plainText = (s: string, bytes: number) => s.isWellFormed() && !s.includes("\0") && Buffer.byteLength(s) <= bytes;

function checkSteps(steps: readonly PlanStepDefinition[]) {
  const prior = new Set<string>();
  for (const step of steps) {
    if (!planStepID(step.id) || prior.has(step.id) || step.requires?.some(dep => !prior.has(dep)))
      return "Step IDs must be unique; prerequisites must appear earlier.";
    if (!plainText(step.prompt, planPromptBytes) || !plainText(step.title, 640)) return `Step ${step.id} has invalid text.`;
    const d = step.delegate;
    if (d && (d.worktree !== undefined && (!plainText(d.worktree, 255) || /[\s\p{Cc}]/u.test(d.worktree)) ||
        d.pwd !== undefined && !plainText(d.pwd, 1024))) return `Step ${step.id} has an invalid delegate worktree or pwd.`;
    prior.add(step.id);
  }
}

export function planAuthor(harness: () => Harness, directory: (cwd?: string) => string, policy: ToolPolicy) {
  const read = defineTool({
    name: readPlanTool, description: "Read a plan of this workspace by planId, with its revision and step statuses. " +
      "Without planId, list this workspace's plans. Does not edit or run anything.",
    parameters: { type: "object", properties: { planId: { type: "string", minLength: 1, maxLength: 64 } }, additionalProperties: false } as const,
    callers: ["model"], replay: "safe", outputLimits: { maxBytes: 512 * 1024 },
    async execute({ planId }, api, ctx) {
      if (!api.env) return failed(workspaceUnavailable);
      try {
        return await api.commit(async tx => result(JSON.stringify(planId === undefined
          ? { plans: (await workspacePlans(tx, api.conversationId, directory)).slice(0, 48) }
          : await workspacePlan(tx, api.conversationId, directory, planId))), ctx);
      } catch (error) {
        if (error instanceof WireError && error.code === "invalid_request") return failed(error.message);
        throw error;
      }
    },
  });
  const save = defineTool({
    name: savePlanTool, description: "Save a structured plan to /plan when the user asks for one. Omit planId and use baseRevision 0 " +
      "for a new plan; pass planId and its current revision to revise one. Cannot start implementation.",
    parameters: saveParameters, callers: ["model"], replay: "unsafe",
    async execute(args, api, ctx) {
      if (!api.env) return failed(workspaceUnavailable);
      const draft = args as PlanDraft;
      const problem = checkSteps(draft.steps) ?? (plainText(draft.title, 640) ? undefined : "The title has invalid text.");
      if (problem) return failed(problem);
      try {
        return await api.commit(async tx => {
          const { planId, revision } = await savePlan(tx, api.conversationId, directory, draft);
          return result(`Saved "${draft.title}" as plan ${planId}, revision ${revision}. Open /plan to review it.`);
        }, ctx);
      } catch (error) {
        if (!(error instanceof WireError)) throw error;
        if (error.code === "invalid_request") return failed(error.message);
        if (error.code === "limit") return failed("The plan revision limit was reached.");
        throw error;
      }
    },
  });
  const report = defineTool({
    name: reportPlanTool, description: "In a plan run, record selected steps as done, or blocked with the reason. " +
      "Leave a step unreported if you did not get to it. Done is final.",
    callers: ["model"], replay: "safe",
    parameters: { type: "object", required: ["steps"], additionalProperties: false, properties: { steps: {
      type: "array", minItems: 1, maxItems: planStepLimit, items: {
        type: "object", required: ["stepId", "status", "detail"], additionalProperties: false, properties: {
          stepId: stepID, status: { type: "string", enum: ["done", "blocked"] }, detail: text(180),
        },
      },
    } } } as const,
    async execute({ steps }: { steps: PlanOutcome[] }, api, ctx) {
      const problem = await api.commit(tx => reportSteps(tx, api.conversationId, steps), ctx);
      return problem ? failed(problem) : result("Recorded.");
    },
  });
  return defineExtension({ name: "hush-plan-author", tools: [read, save, report], hooks: [
    hook(GenerationTask, { async beforeRequest({ messages }, api, ctx) {
      const tools = getCurrentTools(messages);
      const effective = await policy.prepare(api.conversationId, api.taskId, ctx);
      const lastSystem = messages.findLastIndex(m => m.role === "system");
      const msg = messages[lastSystem];
      if (msg?.role !== "system") return; // Offering a tool requires a system entry.
      // Each request offers only the tools its effective profile allows, and
      // plan_report only in a plan run's turn.
      const run = effective.kind === "default" && tools.some(t => t.name === reportPlanTool) &&
        await harness().commit(tx => inPlanRun(tx, api.conversationId), ctx);
      const visible = (t: { name: string }) => allowsTool(effective, t.name, false) && (t.name !== reportPlanTool || run);
      const instruction = effective.kind === "disabled" ? "This turn has no tool authority. Do not take further actions." : advertised;
      return { messages: messages.with(lastSystem, { ...msg, sections: { ...msg.sections, hushPlan: instruction },
        toolsAdded: msg.toolsAdded?.filter(visible),
        toolsRemoved: [...msg.toolsRemoved ?? [], ...tools.filter(t => !visible(t)).map(t => ({ name: t.name }))],
      }) };
    } }),
  ] });
}
