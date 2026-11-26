import { defineDoc, LiveDoc, UserEntry, type ConversationId, type EntryId, type EntryRecord, type Tx } from "@earendil-works/pi-durable";
import { environmentInputKind } from "../../server/src/client-environment.js";
import type { PlanSources } from "./plan.js";

export const readPlanTool = "hush_plan_current", proposePlanTool = "hush_plan_propose", previewPlanTool = "hush_plan_preview";
export const isPlanDraftTool = (name: string) => name === readPlanTool || name === proposePlanTool || name === previewPlanTool;
export type DraftMode = "save" | "preview";
export const draftProposalTool = (mode: DraftMode) => mode === "preview" ? previewPlanTool : proposePlanTool;
export const draftPrompt = "Start your request with ‘Create a plan …’ or ‘Revise the plan …’ to save a draft in /plan.";

// Preview wording grants no authority to save. Both modes are based only on
// placed user inputs, never on instructions in model or tool output.
const explicitDraft = /^(?:(?:can|could|would|will) you\s+)?(?:please[ ,]+)?(?:plan\s*:|(?:create|draft|write|make|prepare|outline|propose|revise|update|amend|edit)\s+(?:(?:a|an|the|this|my|our)\s+)?(?:(?:new|existing)\s+)?plan(?=$|\s|:|[.,?!](?:\s|$)))/i;
const previewDraft = /^(?:preview\s+|let['’]?s\s+(?:work\s+on\s+(?:(?:creating|drafting|writing)\s+)?|(?:create|draft|write)\s+))(?:(?:a|an|the|this|my|our)\s+)?(?:(?:new|existing)\s+)?plan(?=$|\s|:|[.,?!](?:\s|$))/i;
export const LegacyDraftTurn = defineDoc<{ first?: EntryId; mode?: DraftMode }>({
  kind: "hush.plan-draft-turn", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

export function planDraftMode(text: string, fixture = false): DraftMode | undefined {
  const request = text.trimStart();
  return explicitDraft.test(request) || fixture && (request === "!plan-fixture" || request === "!plan-fixture-revise") ? "save"
    : previewDraft.test(request) ? "preview" : undefined;
}

export function draftInput(entry: EntryRecord | undefined, owner: ConversationId, fixture: boolean) {
  if (!entry || entry.conversationId !== owner || !UserEntry.is(entry) && entry.kind !== environmentInputKind) return;
  const msg = entry.model?.find(m => m.role === "user");
  if (!msg) return;
  const text = typeof msg.content === "string" ? msg.content : msg.content.filter(b => b.type === "text").map(b => b.text).join("\n");
  const mode = planDraftMode(text, fixture);
  if (!mode) return;
  const limit = 3000;
  return { mode, entryId: String(entry.id), text: text.slice(0, limit).toWellFormed(),
    partial: text.length > limit || typeof msg.content !== "string" && msg.content.some(b => b.type !== "text") };
}

export async function draftSources(tx: Tx, owner: ConversationId, fixture: boolean): Promise<PlanSources> {
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
