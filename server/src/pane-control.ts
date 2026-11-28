import { randomUUID } from "node:crypto";
import { durableID, object, WireError } from "./wire.js";

export const paneControlVersion = 1;
export const threadViewVersion = 2;
export const paneMaxReadLines = 200;
export type PaneCapabilities = { left: boolean; above: boolean; tabs: boolean; read: boolean; views?: boolean;
  threadTabs?: boolean; noPanes?: boolean };
export type ThreadWhere = "tab" | "split";
// Opening and switching to a thread is host-initiated (a conversation move),
// never a model-facing thread_open placement.
export type ThreadOpenWhere = ThreadWhere | "focus";
export type PanePlacement = "right" | "below" | "left" | "above" | "tab";
export type PaneLaunch = { mode: "argv"; program: string; args: string[] } | { mode: "shell"; text: string } |
  { mode: "terminal" } | { mode: "hush" };
export type PaneFileIdentity = { device: string; inode: string; birth: string };
export type PaneDirectory = { dir: string; directory: PaneFileIdentity };
export function paneFileIdentity(info: { dev: bigint; ino: bigint; birthtimeNs: bigint }): PaneFileIdentity {
  return { device: String(info.dev), inode: String(info.ino), birth: String(info.birthtimeNs) };
}
export type PaneRequest =
  ({ op: "open"; placement: PanePlacement; launch: PaneLaunch } & PaneDirectory) |
  { op: "list" } | { op: "focus"; pane: string } | { op: "read"; pane: string; lines: number } |
  { op: "thread_open"; threadId: string; where: "tab" | "focus" } |
  ({ op: "thread_open"; threadId: string; where: "split" } & PaneDirectory);
export type PaneReply = { outcome: "rejected" | "unknown" } |
  ({ outcome: "confirmed" } & ({ pane: string } | { panes: string[] } | { text: string; truncated: boolean } |
    { threadId: string; where: ThreadOpenWhere }));

export const threadStartNoticeKind = "hush.thread-start-notice";
export type PaneAdmission = { key: string; until: number; launch?: true };
// Wire grammar, also used by the Go SDK. Do not use platform trim semantics.
export const paneLaunchInput = (text: unknown) => typeof text === "string" && /^\/pane[ \t]/.test(text) &&
  /[^\p{White_Space}\uFEFF]/u.test(text.slice(6));
// Undefined allows an ordinary input on the issuing peer. False rejects a
// foreign attempt if it did not already win admission under the same key.
export type SelectPaneAdmission = () => PaneAdmission | undefined | false;
const rejected: PaneReply = { outcome: "rejected" };
const unknown: PaneReply = { outcome: "unknown" };
const handle = (value: unknown): value is string => typeof value === "string" && /^hp_[A-Z2-7]{26}$/.test(value);
const maxCalls = 4096;
const maxPanes = 512;
const maxReadBytes = 16 * 1024;
const callMs = 10_000;
const leaseMs = 30 * 60_000;

type Push = { type: "pane_call"; id: string; deadlineMs: number; request: PaneRequest } | { type: "pane_cancel"; id: string };

export function parsePaneCapabilities(value: unknown, version = paneControlVersion): Readonly<PaneCapabilities> {
  const threadViews = version === threadViewVersion;
  const keys = ["left", "above", "tabs", "read", "views", ...(threadViews ? ["threadTabs", "noPanes"] : [])];
  if (!object(value) || Object.keys(value).some(k => !keys.includes(k))) throw new WireError("invalid_request");
  const { left, above, tabs, read, views, threadTabs, noPanes } = value;
  if (typeof left !== "boolean" || typeof above !== "boolean" || typeof tabs !== "boolean" || typeof read !== "boolean" ||
      (views !== undefined && typeof views !== "boolean") ||
      (threadViews && (threadTabs !== true || noPanes !== undefined && noPanes !== true || noPanes && (left || above || tabs || read || views))))
    throw new WireError("invalid_request");
  return Object.freeze({ left, above, tabs, read, views: views === true, ...(threadViews && { threadTabs: true }),
    ...(noPanes === true && { noPanes: true }) });
}

export function panePlacements(caps: Partial<PaneCapabilities>): PanePlacement[] {
  if (caps.noPanes) return [];
  const out: PanePlacement[] = ["right", "below"];
  if (caps.left) out.push("left");
  if (caps.above) out.push("above");
  if (caps.tabs) out.push("tab");
  return out;
}
export function paneLaunchModes(caps: Partial<PaneCapabilities>): PaneLaunch["mode"][] {
  if (caps.noPanes) return [];
  return caps.views ? ["argv", "shell", "terminal", "hush"] : ["argv", "shell"];
}
export function threadViewPlacements(caps: Partial<PaneCapabilities>): ThreadWhere[] {
  return !caps.threadTabs ? [] : caps.views && !caps.noPanes ? ["tab", "split"] : ["tab"];
}
export const paneCanRead = (caps: Partial<PaneCapabilities>) => !caps.noPanes && caps.read === true;
const opensView = (request: PaneRequest) => request.op === "open" || request.op === "thread_open";

function allowed(caps: PaneCapabilities, request: PaneRequest) {
  if (request.op === "thread_open") return durableID(request.threadId) &&
    (request.where === "focus" ? caps.threadTabs === true : threadViewPlacements(caps).includes(request.where));
  if (caps.noPanes) return false;
  switch (request.op) {
    case "list": return true;
    case "focus": return handle(request.pane);
    case "read": return paneCanRead(caps) && handle(request.pane) && Number.isInteger(request.lines) && request.lines >= 1 && request.lines <= paneMaxReadLines;
    case "open": return paneLaunchModes(caps).includes(request.launch.mode) && panePlacements(caps).includes(request.placement);
  }
}

function parseReply(raw: unknown, request: PaneRequest): PaneReply {
  if (!object(raw)) return unknown;
  const size = Object.keys(raw).length;
  if (raw.outcome === "unknown" || raw.outcome === "rejected") return size === 1 ? { outcome: raw.outcome } : unknown;
  if (raw.outcome !== "confirmed") return unknown;
  if (request.op === "open" || request.op === "focus") {
    return size === 2 && handle(raw.pane) && (request.op !== "focus" || request.pane === raw.pane)
      ? { outcome: "confirmed", pane: raw.pane } : unknown;
  }
  if (request.op === "list") {
    const { panes } = raw;
    return size === 2 && Array.isArray(panes) && panes.length <= maxPanes && panes.every(handle) && new Set(panes).size === panes.length
      ? { outcome: "confirmed", panes } : unknown;
  }
  if (request.op === "thread_open") {
    const { threadId, where } = request;
    return size === 3 && raw.threadId === threadId && raw.where === where ? { outcome: "confirmed", threadId, where } : unknown;
  }
  const { text, truncated } = raw;
  if (size !== 3 || typeof text !== "string" || !text.isWellFormed() || Buffer.byteLength(text) > maxReadBytes ||
    /[\x00-\x08\x0b-\x1f\x7f-\x9f]/.test(text) || typeof truncated !== "boolean") return unknown;
  const lines = text === "" ? 0 : text.replace(/\n$/, "").split("\n").length;
  return lines <= request.lines ? { outcome: "confirmed", text, truncated } : unknown;
}

// One route belongs to one verified client connection. Tokens in input entry
// data are inert admission labels: they cannot open or transfer the connection.
export class PaneRoute {
  readonly key = randomUUID();
  readonly #seen = new Set<string>();
  readonly #launches = new Set<string>();
  #pending?: { id: string; deadlineMs: number; request: PaneRequest; finish: (reply: PaneReply) => void };
  #closed = false;

  constructor(readonly capabilities: Readonly<PaneCapabilities>, private readonly send: (call: Push) => boolean,
      private readonly signal: AbortSignal, private readonly remove: () => void) {}

  get live() { return !this.#closed && !this.signal.aborted; }

  admission(text?: unknown): PaneAdmission {
    return { key: this.key, until: Date.now() + leaseMs, ...(paneLaunchInput(text) && { launch: true }) };
  }

  canLaunch(input: string | undefined) { return input !== undefined && !this.#launches.has(input); }

  close(cancelClient = false) {
    if (this.#closed) return;
    this.#closed = true;
    const pending = this.#pending;
    pending?.finish(unknown);
    if (pending && cancelClient && this.capabilities.threadTabs && !this.signal.aborted) {
      // Best effort: any local effect already dispatched remains unknown.
      try { this.send({ type: "pane_cancel", id: pending.id }); } catch { /* route already closed */ }
    }
    this.#seen.clear();
    this.#launches.clear();
    this.remove();
  }

  reply(id: string, result: unknown) {
    const pending = this.#pending;
    if (this.live && pending?.id === id) {
      if (pending.deadlineMs <= Date.now()) this.close(true);
      else pending.finish(parseReply(result, pending.request));
    }
  }

  dispatch(key: string, until: number, request: PaneRequest, signal?: AbortSignal, launchInput?: string): Promise<PaneReply> {
    // Record every dispatched key for the live route. Never evict an ID and
    // make room for a replay, including after an indeterminate result.
    if (this.#seen.has(key)) return Promise.resolve(unknown);
    if (!this.live || until <= Date.now() || signal?.aborted || this.#pending || this.#seen.size >= maxCalls ||
        !allowed(this.capabilities, request) || opensView(request) && !this.canLaunch(launchInput)) return Promise.resolve(rejected);
    const id = randomUUID();
    const deadlineMs = Math.min(until, Date.now() + callMs);
    this.#seen.add(key);
    if (opensView(request) && launchInput !== undefined) this.#launches.add(launchInput);
    return new Promise(resolve => {
      const finish = (reply: PaneReply) => {
        clearTimeout(timer);
        signal?.removeEventListener("abort", onAbort);
        this.#pending = undefined;
        resolve(reply);
      };
      const onAbort = () => {
        // Do not open another operation while the client might still be
        // acting on this one. The origin becomes unavailable until reconnect.
        this.close(true);
      };
      const timer = setTimeout(onAbort, Math.max(0, deadlineMs - Date.now()));
      timer.unref();
      signal?.addEventListener("abort", onAbort, { once: true });
      this.#pending = { id, deadlineMs, request, finish };
      try {
        if (!this.send({ type: "pane_call", id, deadlineMs, request })) finish(rejected);
      } catch {
        this.close();
      }
    });
  }
}

export class PaneRoutes {
  readonly #routes = new Map<string, PaneRoute>();

  bind(capabilities: Readonly<PaneCapabilities>, send: (call: Push) => boolean, signal: AbortSignal) {
    const route: PaneRoute = new PaneRoute(capabilities, send, signal, () => this.#routes.delete(route.key));
    this.#routes.set(route.key, route);
    return route;
  }

  get(admission: PaneAdmission) {
    const route = this.#routes.get(admission.key);
    return route?.live && admission.until > Date.now() ? route : undefined;
  }
}

export function storedPaneAdmission(value: unknown): PaneAdmission | undefined {
  if (!object(value) || !object(value.paneAdmission)) return;
  const { key, until, launch } = value.paneAdmission;
  if (typeof key === "string" && typeof until === "number" && Number.isSafeInteger(until))
    return { key, until, ...(launch === true && { launch: true }) };
}
