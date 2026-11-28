import { defineExtension, defineTask, type PhaseHandler } from "@earendil-works/pi-durable";

// Hush no longer creates these task kinds, but existing stores can still hold
// them. They were created with abandonOnRestart, so Durable aborts them at
// boot and needs a definition to drain them and their owned conversations.
const ended = { status: "terminal", outcome: { status: "completed", result: null } } as const;
const aborted = { status: "terminal", outcome: { status: "aborted" } } as const;

// Every stored phase ends the task; an older version keeps its input.
function retired<P extends string>(name: string, initial: P, phases: readonly P[], version = 1) {
  const end: PhaseHandler<unknown, { phase: P }, { phase: P }, null, object> = (_task, runtime, ctx) => runtime.commit(() => ended, ctx);
  return defineTask<unknown, { phase: P }, null>({
    name, version, initial: () => ({ phase: initial }),
    phases: Object.fromEntries(phases.map(phase => [phase, end])) as Record<P, typeof end>,
    abort: (_task, runtime, ctx) => runtime.commit(() => aborted, ctx),
    ...(version > 1 && { migrate: (input: unknown) => ({ input, checkpoint: { phase: initial } }) }),
  });
}

export const retiredTasks = defineExtension({ name: "hush-retired-tasks", tasks: [
  // Independent plan review (`planReview:1`), removed in favor of delegates.
  retired("hush.plan-review", "open", ["open", "submit", "settle", "timeout"]),
  // Review delegation (`delegation:1`), removed in favor of read-only subagents.
  retired("hush.review-worker", "gate", ["gate", "open", "submit", "settle", "timeout"]),
  // Scoped plan runs and the faux plan fixture, removed with `plan:1`.
  retired("hush.plan-scoped-run", "next", ["next", "submit", "settle", "settleMain", "publishMain", "interrupted"], 2),
  retired("hush.plan-fixture-run", "next", ["next", "submit", "settle", "settleMain", "publishMain", "interrupted"]),
] });
