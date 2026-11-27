import { arr, int, lit, obj, oneOf, str, union } from "@earendil-works/pi-schema";
import { paneCanRead, paneLaunchModes, paneMaxReadLines, panePlacements, threadViewPlacements,
  type PaneCapabilities, type ThreadWhere } from "../../server/src/pane-control.js";
const noNUL = "^[^\\u0000]*$";
const text = str({ pattern: noNUL, minLength: 1 });
const modes = {
  argv: obj({ mode: lit("argv"), program: text, args: arr(str({ pattern: noNUL })) }),
  shell: obj({ mode: lit("shell"), text }),
  terminal: obj({ mode: lit("terminal") }),
  hush: obj({ mode: lit("hush") }),
  review: obj({ mode: lit("review"), handle: str({ pattern: "^[0-9a-f]{32}$", minLength: 32, maxLength: 32 }) }),
} as const;

// Build schemas from live capabilities. A client without a route is not
// eligible for any of these tools.
export function paneToolParameters(caps: Partial<PaneCapabilities>, review = false) {
  const placements = threadViewPlacements(caps), launches = paneLaunchModes(caps).filter(mode => mode !== "review" || review);
  return {
    ...(!caps.noPanes ? {
      pane_open: obj({ placement: { type: "string", ...oneOf(panePlacements(caps)) }, launch: union(launches.map(mode => modes[mode])) }),
      pane_list: obj({}),
      pane_focus: obj({ pane: text }),
      ...(paneCanRead(caps) ? { pane_read: obj({ pane: text, lines: int({ minimum: 1, maximum: paneMaxReadLines }) }) } : {}),
    } : {}),
    ...(placements.length ? { thread_open: obj({ threadId: str({ pattern: "^[1-9][0-9]{0,15}$" }),
      where: { type: "string", ...oneOf(placements) } }) } : {}),
  };
}

export type PaneToolName = keyof ReturnType<typeof paneToolParameters>;
export const isPaneTool = (name: string): name is PaneToolName =>
  name === "pane_open" || name === "pane_list" || name === "pane_focus" || name === "pane_read" || name === "thread_open";

type PaneToolResult<T> = ({ outcome: "confirmed" } & T) | {
  outcome: "rejected" | "unknown";
  reason?: string;
};

// Pane strings in tool results are Hush-issued opaque handles, not native IDs.
export type PaneToolResults = {
  pane_open: PaneToolResult<{ pane: string }>;
  pane_list: PaneToolResult<{ panes: string[] }>;
  pane_focus: PaneToolResult<{ pane: string }>;
  pane_read: PaneToolResult<{ text: string; truncated: boolean }>;
  thread_open: PaneToolResult<{ threadId: string; where: ThreadWhere }>;
};
