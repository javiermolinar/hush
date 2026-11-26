import { ToolResultEntry, type EntryRecord } from "@earendil-works/pi-durable";
import { reviewEvidenceID, reviewTextLimit, validReviewText, workerReport, type ReviewContract, type ReviewReceipt } from "../../server/src/delegation.js";
import { object } from "../../server/src/wire.js";
import { reviewReadTool } from "./tool-policy.js";

const priorities = ["P0", "P1", "P2", "P3"] as const;
const versions = ["base", "staged", "working"] as const;
type Version = typeof versions[number];
type LineCounts = Record<Version, number>;
const text = (maxLength: number) => ({ type: "string", maxLength } as const);

export const reviewReportParameters = { type: "object", additionalProperties: false, properties: {
  report: text(reviewTextLimit), // compatible with original assignments without an acceptance contract
  summary: text(320),
  findings: { type: "array", maxItems: 6, items: { type: "object", additionalProperties: false,
    required: ["priority", "path", "version", "line", "message"], properties: {
      priority: { type: "string", enum: priorities }, path: text(1024), version: { type: "string", enum: versions },
      line: { type: "integer", minimum: 1 }, message: text(280),
    } } },
  checks: { type: "array", maxItems: 6, items: { type: "object", additionalProperties: false,
    required: ["id", "basis", "note"], properties: {
      id: text(32), basis: { type: "string", enum: ["inspected", "supplied", "unverified"] }, source: text(1024), note: text(180),
    } } },
  limitations: { type: "array", minItems: 1, maxItems: 4, items: text(240) },
} } as const;

// Only the snapshot tool writes these details. The read itself, not a model's
// claim about a tool call, establishes which version and lines it could see.
export function pinnedRead(entry: EntryRecord, snapshotId: string): { path: string; lines: LineCounts } | undefined {
  const result = ToolResultEntry.is(entry) ? entry.model?.[0] : undefined;
  if (result?.role !== "toolResult" || result.toolName !== reviewReadTool || result.isError) return;
  const d = result.details;
  if (!object(d) || d.snapshotId !== snapshotId || !validReviewText(d.path, 1024) || !object(d.lines)) return;
  const { base, staged, working } = d.lines;
  const count = (v: unknown): v is number => typeof v === "number" && Number.isSafeInteger(v) && v >= 0;
  if (count(base) && count(staged) && count(working)) return { path: d.path, lines: { base, staged, working } };
}

export function renderReview(args: unknown, receipt: ReviewReceipt, handle: string, contract: ReviewContract | undefined,
    reads: ReadonlyMap<string, LineCounts>, observed?: string): string | undefined {
  if (!object(args)) return;
  const { summary, findings, checks, limitations, report } = args;
  if (report !== undefined) return !contract && Object.keys(args).length === 1 && workerReport(report) ? report : undefined;
  if (!validReviewText(summary, 320) || !Array.isArray(findings) || findings.length > 6 || !Array.isArray(checks) || checks.length > 6 ||
      !Array.isArray(limitations) || limitations.length < 1 || limitations.length > 4 || !limitations.every(l => validReviewText(l, 240))) return;
  const located: { rank: number; text: string }[] = [];
  for (const finding of findings) {
    if (!object(finding)) return;
    const { priority, path, version, line, message } = finding;
    const rank = priorities.findIndex(p => p === priority), locatedVersion = versions.find(v => v === version);
    if (rank < 0 || !locatedVersion || !validReviewText(path, 1024) || typeof line !== "number" || !Number.isSafeInteger(line) ||
        line < 1 || line > (reads.get(path)?.[locatedVersion] ?? 0) || !validReviewText(message, 280)) return;
    located.push({ rank, text: `${priority} · ${JSON.stringify(path)}:${line} (${version}) · ${message}` });
  }
  const byCheck = new Map<string, string>();
  for (const check of checks) {
    if (!object(check)) return;
    const { id, basis, source, note } = check;
    if (!reviewEvidenceID(id) || !contract?.checks?.some(c => c.id === id) || byCheck.has(id) || !validReviewText(note, 180)) return;
    let label: string;
    if (basis === "inspected" && typeof source === "string" && reads.has(source)) label = `Performed: pinned source inspection (${JSON.stringify(source)})`;
    else if (basis === "supplied" && contract.evidence?.some(e => e.id === source)) label = `Supplied evidence [${source}] (not rerun)`;
    else if (basis === "unverified" && source === undefined) label = "Unverified";
    else return;
    byCheck.set(id, `${label} · ${note}`);
  }
  const { selection: { model, thinking }, snapshotId, base } = receipt;
  const lines = ["Independent review · advisory only", `Worker: ${receipt.workers.find(w => w.handle === handle)!.name} (${handle})`,
    `Pinned selection: ${model.provider}/${model.modelId} · thinking ${thinking}`,
    `Report call: ${observed ?? "model and thinking not observed"}`, `Snapshot: ${snapshotId}`, `Base: ${base}`, "", summary,
    "", "Findings", ...(located.length ? located.sort((a, b) => a.rank - b.rank).map(f => f.text) : ["None reported."]),
    "", "Acceptance checks", ...(contract?.checks?.map(c => `[${c.id}] ${c.description} — ${byCheck.get(c.id) ?? "Unverified (not assessed)"}`) ?? ["No acceptance checks supplied."]),
    "", "Limitations", ...limitations, "No tests executed by this reviewer. This report does not verify or approve work."];
  const rendered = lines.join("\n");
  return workerReport(rendered) ? rendered : undefined;
}
