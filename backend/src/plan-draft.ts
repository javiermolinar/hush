import { BACKGROUND_CONTEXT, withAbortSignal, type Context } from "@earendil-works/chord/context";
import { AgentDoc, InboxDoc, LiveDoc, ResetEntry, defineDoc, defineDocFamily, defineEntry, type ConversationId, type EntryId,
  type Harness, type SubmissionId, type Tx } from "@earendil-works/pi-durable";
import { planPreviewKind, planPreviewSavedKind, type PlanDraftHost, type PlanDraftReceipt, type PlanDraftTarget,
  type PlanStep } from "../../server/src/plan.js";
import { WireError } from "../../server/src/wire.js";
import { currentPlan, proposePlan, type PlanSources } from "./plan.js";
import { foregroundPlan } from "./plan-turn.js";

export type DraftProposal = { baseRevision: number; title: string; steps: PlanStep[] };
type PreviewData = {
  proposal: DraftProposal; sources: PlanSources; submission: SubmissionId; cwd: string;
  boundPlan?: { id: string; revision: number }; text: string; timestampMs: number;
};
const Preview = defineEntry<PreviewData>(planPreviewKind);
const Pending = defineDoc<{ preview?: EntryId; submission?: SubmissionId }>({
  kind: "hush.plan-pending-preview", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});
const Saved = defineDocFamily<{ receipt?: PlanDraftReceipt }, null>({
  kind: "hush.plan-preview-receipt", version: 1, scope: "session", family: true,
  initial: () => ({}), checkpointWhen: () => true,
});

const invalid = (message: string) => { const error = new WireError("invalid_request"); error.message = message; return error; };
const outdated = () => invalid("That preview is no longer current. Ask for a new preview.");

function previewText({ title, steps, baseRevision }: DraftProposal) {
  const lines = ["Plan preview · " + title.replace(/\s+/g, " ").trim(), `Title: ${title}`,
    baseRevision ? `Revise r${baseRevision} · not saved` : "New plan · not saved"];
  for (const [i, step] of steps.entries()) {
    lines.push("", `${i + 1}. ${step.title} [${step.id}]`, step.description);
    if (step.requires?.length) lines.push(`Requires: ${step.requires.join(", ")}`);
    if (step.after) lines.push(`After: ${step.after}`);
    if (step.blocker) lines.push(`Blocked: ${step.blocker}`);
    if (step.thinking) lines.push(`Thinking: ${step.thinking}`);
    for (const check of step.checks) lines.push(`Check: ${check}`);
    if (step.edit) lines.push(`File: ${step.edit.path}`, `Required text: ${JSON.stringify(step.edit.contains)}`);
    if (step.scope) lines.push(`Write scope: ${step.scope.write.join(", ")}`,
      ...(step.scope.read?.length ? [`Read scope: ${step.scope.read.join(", ")}`] : []),
      `Acceptance: ${step.scope.acceptance.label}`, step.scope.acceptance.script);
  }
  return lines.join("\n");
}

export async function previewPlan(tx: Tx, owner: ConversationId, directory: (cwd?: string) => string,
    proposal: DraftProposal, sources: PlanSources) {
  const base = await currentPlan(tx, owner);
  const expected = base.plan && !base.closed && !base.complete ? base.plan.revision : 0;
  if (base.pinned || proposal.baseRevision !== expected) throw invalid("The preview is not based on the current plan. Read the current plan before drafting.");
  const input = (await tx.doc(LiveDoc, owner)).run?.inputs.at(-1);
  const submission = input !== undefined ? await tx.submission(input) : undefined;
  if (submission?.type !== "input" || submission.status !== "placed") throw outdated();
  const pending = await tx.doc(Pending, owner);
  // One candidate per input, even after a tool retry. Never silently replace
  // a preview the user might already be looking at.
  if (pending.submission === submission.id) return false;
  const text = previewText(proposal);
  // Keep the full preview within the client transcript record budget.
  if (Buffer.byteLength(text) > 96 * 1024) throw invalid("The preview is too large. Shorten it before trying again.");
  const cwd = directory((await tx.doc(AgentDoc, owner)).cwd);
  const entry = await tx.appendEntry(Preview, owner, { data: { proposal, sources, submission: submission.id, cwd,
    ...(base.plan && { boundPlan: { id: base.plan.id, revision: base.plan.revision } }), text, timestampMs: Date.now() } });
  pending.preview = entry.id;
  pending.submission = submission.id;
  return true;
}

export class PlanDrafts implements PlanDraftHost {
  constructor(private readonly harness: Harness, private readonly directory: (cwd?: string) => string,
    private readonly fixture: boolean, private readonly authorize: (cwd: string, ctx: Context) => Promise<unknown>) {}

  async save(target: PlanDraftTarget, signal: AbortSignal) {
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT), owner = Number(target.threadId) as ConversationId;
    const previewId = Number(target.previewId) as EntryId;
    const check = async (tx: Tx) => {
      const thread = await tx.conversation(owner), preview = await tx.entry(Preview, previewId);
      if (!thread || thread.owner || preview?.conversationId !== owner) throw new WireError("not_found");
      const saved = await tx.doc(Saved, target.previewId, null);
      if (saved.receipt) return { kind: "saved", receipt: { ...saved.receipt } } as const;
      const { data } = preview;
      if ((await tx.doc(Pending, owner)).preview !== previewId) throw outdated();
      if ((await tx.doc(LiveDoc, owner)).run || (await tx.doc(InboxDoc, owner)).items.length || await foregroundPlan(tx, owner)) {
        throw new WireError("busy");
      }
      const submission = await tx.submission(data.submission);
      if (submission?.status !== "done") throw outdated();
      // The confirmation applies to the latest conversation topic only. Do
      // not skip over unseen history to prove an old preview is still current.
      const recent = await tx.scanEntries({ conversationId: owner, minEntryId: previewId }, 64);
      if (recent.next || recent.items.some(entry => entry.kind === ResetEntry.kind || entry.model?.some(message => message.role === "user"))) {
        throw outdated();
      }
      const cwd = this.directory((await tx.doc(AgentDoc, owner)).cwd);
      const base = await currentPlan(tx, owner);
      if (cwd !== data.cwd || base.pinned || base.plan?.id !== data.boundPlan?.id || base.plan?.revision !== data.boundPlan?.revision) {
        throw invalid("The workspace or plan changed. Ask for a new preview before saving.");
      }
      return { kind: "pending", data, saved, cwd } as const;
    };
    const before = await this.harness.commit(async tx => {
      const state = await check(tx);
      return state.kind === "saved" ? state : { kind: "pending", cwd: state.cwd } as const;
    }, ctx);
    if (before.kind === "saved") return before.receipt;
    await this.authorize(before.cwd, ctx);
    return this.harness.commit(async tx => {
      const state = await check(tx);
      if (state.kind === "saved") return state.receipt;
      const { proposal, sources } = state.data;
      const receipt = await proposePlan(tx, owner, this.directory, this.fixture, proposal.baseRevision, proposal.title, proposal.steps, sources);
      state.saved.receipt = receipt;
      await tx.appendEntry(owner, { kind: planPreviewSavedKind, data: { previewId: target.previewId, ...receipt,
        text: `Saved ${proposal.title} (r${receipt.revision}). Open /plan to review the draft.`, timestampMs: Date.now() } });
      return receipt;
    }, ctx);
  }
}
