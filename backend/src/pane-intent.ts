import { defineDoc, InboxDoc, LiveDoc, ResetEntry, type Conversation, type ConversationId, type EntryId, type EntryRecord,
  type SubmissionId, type Tx } from "@earendil-works/pi-durable";
import { environmentInputKind } from "../../server/src/client-environment.js";
import { storedPaneAdmission } from "../../server/src/pane-control.js";

export const isPaneLaunchEntry = (entry: EntryRecord | undefined) =>
  entry?.kind === environmentInputKind && storedPaneAdmission(entry.data)?.launch === true;

// A Continue can inherit a denial, never a route or an open budget. The pin
// names the actual Continue receipt; run.first alone loses steers on resume.
const PaneContinue = defineDoc<{ pin?: { submission: SubmissionId; entry: EntryId } }>({
  kind: "hush.pane-continue", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

async function paneEntry(tx: Tx, owner: ConversationId, inputs: readonly SubmissionId[], status: "placed" | "unanswered") {
  let inherited: EntryId | undefined;
  for (let i = inputs.length - 1; i >= 0; i--) {
    const id = inputs[i]!, input = await tx.submission(id);
    if (!input || input.conversationId !== owner || input.status !== status) continue;
    if (input.type === "continue") {
      const { pin } = await tx.doc(PaneContinue, owner);
      if (pin?.submission === id) inherited = pin.entry;
    } else if (input.type === "input" && input.entry !== undefined) {
      const entry = await tx.entry(input.entry);
      if (entry?.conversationId === owner && isPaneLaunchEntry(entry)) return entry.id;
    }
  }
  return inherited;
}

export async function paneOnlyTurn(tx: Tx, owner: ConversationId) {
  const { run } = await tx.doc(LiveDoc, owner);
  return !!run && await paneEntry(tx, owner, run.inputs, "placed") !== undefined;
}

async function afterReset(tx: Tx, owner: ConversationId, pane: EntryId) {
  const head = await tx.latestHeadMarker(owner);
  if (!head || head.id <= pane) return false;
  if (ResetEntry.is(head)) return true;
  // A compaction can follow a reset but is not itself a reset. Look only
  // between the pane entry and the newest head. Keep the denial when the
  // bounded scan cannot prove it was reset.
  const query = { conversationId: owner, minEntryId: pane, maxEntryId: head.id, order: "descending" as const };
  let page = await tx.scanEntries(query, 64);
  for (let i = 0; ; i++) {
    if (page.items.some(entry => entry.id > pane && ResetEntry.is(entry))) return true;
    if (!page.next || i === 3) return false;
    page = await tx.scanEntries(query, 64, page.next);
  }
}

export async function submitPaneAwareContinue(tx: Tx, conv: Conversation) {
  const owner = conv.id, { run, runFailure } = await tx.doc(LiveDoc, owner);
  let entry: EntryId | undefined;
  // A queued user input or reset starts a fresh boundary. Only an inputless
  // retry of the failed run carries the old pane-only restriction.
  if (!run && runFailure && runFailure.reason !== "reset" &&
    !(await tx.doc(InboxDoc, owner)).items.some(item => item.mode !== "write" || item.entry.head === "self")) {
    const pane = await paneEntry(tx, owner, runFailure.inputs, "unanswered");
    if (pane !== undefined && !await afterReset(tx, owner, pane)) entry = pane;
  }
  const record = entry === undefined ? undefined : await tx.doc(PaneContinue, owner);
  const submission = await conv.submit(tx, { type: "continue" });
  if (record && entry !== undefined) record.pin = { submission, entry };
  return submission;
}
