// Timing helpers shared by the performance cases. Targets are enforced by
// default (a quiet native host) and only reported with HUSH_PERF_MODE=report
// (hosted CI). A known miss names the plan step that fixes it and is reported
// until that step lands.
import assert from "node:assert/strict";

export const reportOnly = process.env.HUSH_PERF_MODE === "report";

export function samples(fallback, min = 20) {
  const n = Number(process.env.HUSH_BENCH_SAMPLES ?? fallback);
  if (!Number.isInteger(n) || n < min || n > 10_000) throw new Error(`HUSH_BENCH_SAMPLES must be ${min}–10000`);
  return n;
}

export function summary(ms) {
  const sorted = [...ms].sort((a, b) => a - b);
  const at = p => Number(sorted[Math.ceil(p * sorted.length) - 1].toFixed(3));
  return { samples: sorted.length, p50_ms: at(0.5), p95_ms: at(0.95), p99_ms: at(0.99), max_ms: at(1) };
}

export function checkTarget(t, { name, value, limit, unit = "ms", knownMiss = "" }) {
  const detail = `${name}: ${value} ${unit}, limit ${limit} ${unit}`;
  if (value <= limit) {
    if (knownMiss) t.diagnostic(`known miss now meets its target (${detail}); enforce it when ${knownMiss} lands`);
    return;
  }
  if (knownMiss) t.diagnostic(`performance target missed (known, until ${knownMiss}): ${detail}`);
  else if (reportOnly) t.diagnostic(`performance target missed (report-only): ${detail}`);
  else assert.fail(`performance target missed: ${detail}`);
}
