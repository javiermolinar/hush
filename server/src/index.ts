import { randomUUID } from "node:crypto";
import { createServer, type Socket } from "node:net";
import { diffRevisions } from "@earendil-works/chord/delta";
import type { Harness } from "@earendil-works/pi-durable";
import { DurableAdapter, nativeStopVersion, parseStop, projectView, requestError, visibleEntry, type NativeHost, type StructuralView } from "./durable.js";
export type { NativeHost } from "./durable.js";
import { clientEnvironment, clientEnvironmentVersion, type ClientEnvironment } from "./client-environment.js";
import { checkCatalog, modelCatalogDraftVersion, modelCatalogSearchVersion, modelCatalogVersion,
  parseCatalogCreation, parseCatalogQuery, type ModelCatalogHost, type ModelCatalogVersion } from "./model-catalog.js";
import { storeIdentityVersion, validStoreIdentity, type StoreIdentity } from "./store-identity.js";
import { parsePlanAt, parsePlanAttach, parsePlanClosure, parsePlanDraftTarget, parsePlanHistory, parsePlanRun, parsePlanWait, parsePlanWorkspace, parsePlanWorktreeSource, parsePlanWorktreeStart,
  planWorktreeVersion, type PlanWorktreeHost, planVersion, planCatalogVersion, planChatVersion, planChatActivityVersion, planDraftVersion, planWorkspaceVersion, type PlanChatHost, type PlanDraftHost, type PlanHost } from "./plan.js";
import { parsePlanReview, planReviewVersion, type PlanReviewHost } from "./plan-review.js";
import { delegationVersion, parseReviewKey, parseReviewLaunch, parseWorkerActivity, parseWorkerInspection, parseWorkerKey,
  reviewEvidenceVersion, workerActivityVersion, type DelegationHost } from "./delegation.js";
import { runtimeVersion, type RuntimeHost } from "./runtime.js";
import { checkSettings, parseSettingsChange, settingsVersion, type SettingsHost } from "./settings.js";
import { parseSchedule, schedulesVersion, type SchedulesHost } from "./scheduling.js";
import { checkpointsVersion, parseCheckpoint, type CheckpointsHost } from "./checkpoints.js";
import { gitDiffVersion, gitDiffWatchVersion, parseGitDiff, parseGitDiffWatch, type GitDiffHost } from "./git-diff.js";
import { parseLoadedContext, loadedContextAgentsVersion, loadedContextVersion, type LoadedContextHost, type LoadedContextVersion } from "./loaded-context.js";
import { parseWorkspaceFiles, parseWorkspaceFileSearch, parseWorkspaceScopeWatch, workspaceFilesVersion, workspaceFileSearchVersion,
  workspaceFileProjectPathsVersion, workspaceScopeWatchVersion, type WorkspaceFilesHost } from "./workspace-files.js";
import { parseWorkspaceSkillList, parseWorkspaceSkillSubmit, workspaceSkillDraftVersion, workspaceSkillsVersion,
  type WorkspaceSkillsHost, type WorkspaceSkillsVersion } from "./workspace-skills.js";
import { imageReply, imagesVersion, parseImageRef, parseImageSubmit, type ImagesHost } from "./images.js";
export type { ImagesHost } from "./images.js";
import { checkIntroReply, parseIntroAction, parseIntroQuery, type IntroHost, type IntroVersion } from "./intro.js";
export type { IntroAction, IntroAttention, IntroHost, IntroQuery, IntroReport, IntroRunIssue, IntroThread, IntroVersion } from "./intro.js";
import { checkThreadDirectoryReply, parseThreadArchive, parseThreadCreation, parseThreadDirectoryQuery, parseThreadLookup,
  threadDirectoryVersion, threadManagementVersion, threadWorkspaceSearchVersion, threadWorkspaceVersion,
  type ThreadDirectoryHost, type ThreadDirectoryVersion, type ThreadWorkspaceVersion } from "./thread-directory.js";
import {
  MAX_RECORD, encode, cleanJSON, identity, isCoreMethod, object, parseRecord, patchShape,
  successShape, token, validRequest, WireError, type Envelope, type Params,
} from "./wire.js";

const MAX_CONNECTION_REQUESTS = 262_144;
const MAX_PENDING_WAITS = 64;
const MAX_BACKLOG = MAX_RECORD * 2 + 1024;

// No optional capability is inferred from the Harness. Each installed adapter
// needs a versioned, additive schema and an explicit negotiation branch.
export type HostCapabilities = Readonly<{
  intro?: IntroHost; threadDirectory?: ThreadDirectoryHost; images?: ImagesHost;
  hushProfile?: 1; nativeStop?: 1; modelCatalog?: ModelCatalogHost; storeIdentity?: StoreIdentity; clientEnvironment?: 1;
  runtime?: RuntimeHost;
  plan?: PlanHost;
  planChat?: PlanChatHost;
  planWorktree?: PlanWorktreeHost;
  planDraft?: PlanDraftHost;
  planReview?: PlanReviewHost;
  delegation?: DelegationHost;
  settings?: SettingsHost;
  workspaceWorkTrust?: 1;
  schedules?: SchedulesHost;
  checkpoints?: CheckpointsHost;
  gitDiff?: GitDiffHost;
  loadedContext?: LoadedContextHost;
  workspaceFiles?: WorkspaceFilesHost;
  workspaceSkills?: WorkspaceSkillsHost;
}>;

export interface ServerOptions {
  host?: NativeHost;
  capabilities?: HostCapabilities;
  /** Public, host-owned diagnostic for an attempted mutation on a read-only Harness. */
  readOnlyReason?: string;
  maxConnections?: number;
  pendingObservations?: number;
  onConnections?: (count: number) => void;
}

type Context = ReturnType<DurableAdapter["context"]>;
type Projected = ReturnType<typeof projectView>;
type EndReason = "overflow" | "source_failed";
type Write = (record: string, flushed?: () => void) => boolean;

class Feed {
  readonly identity: { epoch: string; subscriptionId: string };
  #pending: StructuralView | undefined;
  #pendingCount = 0;
  #detach?: () => void;
  #state?: Projected;
  #rev = 0;
  #scheduled?: NodeJS.Immediate;
  #flushing = false;
  #project = projectView as (view: StructuralView) => Projected | Promise<Projected>;
  #armed = false;
  #closed = false;
  #reason?: EndReason;

  constructor(epoch: string, private readonly limit: number, private readonly write: Write,
      private readonly backpressured: () => boolean) {
    this.identity = { epoch, subscriptionId: randomUUID() };
  }

  async attach(adapter: DurableAdapter, params: Params["subscribe"], ctx: Context, delegation: boolean) {
    const min = params.window?.min ?? 50;
    let older: { before: NonNullable<StructuralView["before"]>; limit: number; entries: StructuralView["entries"] } | undefined;
    let rawEntries: StructuralView["entries"] | undefined, visible: StructuralView["entries"];
    this.#project = view => {
      if (rawEntries !== view.entries) {
        rawEntries = view.entries;
        visible = view.entries.filter(entry => visibleEntry(entry, view.conversation.id, delegation));
      }
      const entries = visible;
      if (!adapter.filtersWorkerNotices && entries.length === view.entries.length) return projectView(view);
      if (view.before === undefined) return projectView({ ...view, entries });
      const before = view.before, limit = Math.max(0, min - entries.length) + 1;
      const assemble = (previous: StructuralView["entries"]) => projectView({ ...view,
        before: previous[limit - 1]?.id, entries: [...previous.slice(0, limit - 1).reverse(), ...entries] });
      if (older?.before === before && older.limit >= limit) return assemble(older.entries);
      return adapter.windowPredecessors(view.conversation.id, before, limit, delegation, ctx).then(previous => {
        older = { before, limit, entries: previous };
        return assemble(previous);
      });
    };
    const observation = await adapter.observe(params, {
      // Called on the Session line. Retain only the newest immutable view. No projection or socket I/O.
      advance: (call) => {
        if (this.#closed || this.#reason) return;
        if (call.kind !== "value") this.#fail("source_failed");
        else if (++this.#pendingCount > this.limit) this.#fail("overflow");
        else { this.#pending = call.value; this.#schedule(); }
      },
      end: () => this.#fail("source_failed"),
    }, ctx);
    this.#detach = () => observation.detach();
    const state = this.#state = await this.#project(observation.value);
    return { ...this.identity, rev: 0, state };
  }

  arm() { this.#armed = true; this.#schedule(); }
  drain() { this.#schedule(); }
  close() {
    this.#closed = true;
    if (this.#scheduled) clearImmediate(this.#scheduled);
    this.#scheduled = undefined;
    this.#detach?.();
    this.#detach = undefined;
    this.#state = this.#pending = undefined;
  }

  #fail(reason: EndReason) {
    if (this.#closed) return;
    this.#reason ??= reason;
    this.#pending = undefined;
    this.#schedule();
  }
  #schedule() {
    if (!this.#armed || this.#closed || this.#scheduled || this.#flushing ||
        (!this.#reason && (!this.#pending || this.backpressured()))) return;
    this.#scheduled = setImmediate(() => {
      this.#scheduled = undefined;
      void this.#flush();
    });
  }
  async #flush() {
    if (this.#reason) {
      this.write(encode({ type: "state_end", ...this.identity, reason: this.#reason }));
      this.close();
      return;
    }
    const view = this.#pending;
    if (!view || this.backpressured()) return;
    this.#pending = undefined;
    this.#pendingCount = 0;
    this.#flushing = true;
    try {
      const candidate = this.#project(view);
      const next = candidate instanceof Promise ? await candidate : candidate;
      if (this.#closed || this.#reason) return;
      if (this.backpressured()) {
        this.#pending ??= view;
        return;
      }
      const ops = diffRevisions(this.#state!, next);
      if (ops.length === 0) return;
      const base = { type: "patch", ...this.identity, baseRev: this.#rev, nextRev: this.#rev + 1 };
      const delta = { ...base, ops };
      // A Chord delta may have a nested path too deep for v1, or a split UTF-16 suffix.
      const frame = cleanJSON(ops) && patchShape(delta) ? delta : { ...base, ops: [["r", next]] };
      if (this.write(encode(frame))) {
        this.#rev++;
        this.#state = next;
      }
    } catch (err) {
      this.#fail(err instanceof WireError && err.code === "limit" ? "overflow" : "source_failed");
    } finally {
      this.#flushing = false;
      this.#schedule();
    }
  }
}

class Peer {
  readonly #abort = new AbortController();
  readonly #context: Context;
  readonly #seen = new Set<string>();
  #version?: number;
  #intro?: { host: IntroHost; version: IntroVersion };
  #threadDirectory?: { host: ThreadDirectoryHost; version: ThreadDirectoryVersion; workspace?: ThreadWorkspaceVersion };
  #directoryPending = false;
  #images?: ImagesHost;
  #schedules?: SchedulesHost;
  #checkpoints?: CheckpointsHost;
  #gitDiff?: GitDiffHost;
  #gitDiffPending = false;
  #gitDiffWatch = false;
  #plan?: PlanHost;
  #planChat?: PlanChatHost;
  #planChatActivity = false;
  #planWorktree?: PlanWorktreeHost;
  #planDraft?: PlanDraftHost;
  #planWorkspace = false;
  #planCatalog = false;
  #planReview?: PlanReviewHost;
  #delegation?: DelegationHost;
  #reviewEvidence = false;
  #workerActivity = false;
  #loadedContext?: { host: LoadedContextHost; version: LoadedContextVersion };
  #workspaceFiles?: { host: WorkspaceFilesHost; version: number };
  #workspaceScopeWatch = false;
  #workspaceSkills?: { host: WorkspaceSkillsHost; version: WorkspaceSkillsVersion };
  readonly #pendingMetadata = new Set<string>();
  #modelCatalog?: { host: ModelCatalogHost; version: ModelCatalogVersion };
  #storeIdentity?: StoreIdentity;
  #runtime?: RuntimeHost;
  #settings?: SettingsHost;
  #nativeStop = false;
  #clientEnvironment = false;
  #introPending = false;
  #feed?: Feed;
  #waiting = 0;

  constructor(private readonly socket: Socket, private readonly adapter: DurableAdapter, private readonly epoch: string,
      private readonly pendingObservations: number, private readonly capabilities: HostCapabilities | undefined,
      private readonly readOnlyReason?: string) {
    this.#context = adapter.context(this.#abort.signal);
    socket.on("error", () => socket.destroy());
    socket.on("close", () => {
      this.#abort.abort();
      this.#feed?.close();
      this.#seen.clear();
    });
    socket.on("drain", () => this.#feed?.drain());
  }

  async run() {
    let pending: Buffer | undefined;
    let length = 0;
    try {
      for await (const chunk of this.socket as AsyncIterable<Buffer>) {
        for (let offset = 0; offset < chunk.length;) {
          if (this.socket.destroyed) return;
          const newline = chunk.indexOf(10, offset);
          const part = chunk.subarray(offset, newline < 0 ? chunk.length : newline);
          const size = length + part.length;
          if (size > MAX_RECORD + 1) throw new WireError("limit");
          let line = part;
          if (length > 0 || newline < 0) {
            if (!pending || size > pending.length) {
              // Bound memory even when a record arrives one byte at a time.
              const buf = Buffer.allocUnsafe(Math.min(MAX_RECORD + 1, Math.max(size, (pending?.length ?? 2048) * 2)));
              pending?.copy(buf, 0, 0, length);
              pending = buf;
            }
            part.copy(pending, length);
            length = size;
            if (newline < 0) break;
            line = pending.subarray(0, size);
            pending = undefined;
            length = 0;
          }
          if (line.at(-1) === 13) line = line.subarray(0, -1);
          if (line.length > MAX_RECORD) throw new WireError("limit");
          offset = newline + 1;
          await this.#request(parseRecord(line));
        }
      }
      // EOF is a connection boundary: cancel passive waits and free the observation.
      this.socket.end();
    } catch {
      this.socket.destroy();
    }
  }

  async #submitInput(req: Envelope, input: Record<string, unknown>, hints?: ClientEnvironment, skillToken?: string) {
    if (Object.hasOwn(input, "images")) {
      if (!this.#images) throw new WireError("unsupported_capability", "images");
      return this.adapter.submitImages(parseImageSubmit(input), this.#images, this.#abort.signal, this.#context, hints, skillToken);
    }
    const call = { ...req, method: "submit", params: input };
    if (!validRequest(call) || call.method !== "submit") throw new WireError("invalid_request");
    const admission = await this.adapter.admit(call, this.#context, !!this.#modelCatalog, hints, skillToken);
    if (!("result" in admission)) throw new WireError("internal");
    return admission.result;
  }

  #metadata(req: Envelope, capability: string, call: () => Promise<object>) {
    if (this.#pendingMetadata.has(capability)) throw new WireError("limit");
    this.#pendingMetadata.add(capability);
    // Keep parsing so EOF cancels the background metadata read.
    void Promise.resolve().then(call).then(result => {
      if (!this.socket.destroyed) this.#write(encode({ ...identity(req), ok: true, result }));
    }).catch(error => {
      if (!this.socket.destroyed) this.#error(req, error);
    }).finally(() => this.#pendingMetadata.delete(capability));
  }

  async #request(raw: unknown) {
    if (!object(raw) || raw.type !== "request" || !token(raw.id) || typeof raw.method !== "string" ||
        !raw.method.length || raw.method.length > 320 || [...raw.method].length > 160 ||
        this.#seen.has(raw.id) || this.#seen.size >= MAX_CONNECTION_REQUESTS) throw new WireError("invalid_request");
    const req = raw as Envelope;
    this.#seen.add(req.id);
    try {
      if (req.method !== "hello" && !this.#version) throw new WireError("not_negotiated");
      if (Object.keys(req).length !== 4 || !object(req.params)) throw new WireError("invalid_request");
      if (!isCoreMethod(req.method)) {
        const parts = /^ext\.([A-Za-z][A-Za-z0-9_-]{0,63})\.([A-Za-z][A-Za-z0-9_-]{0,63})$/.exec(req.method);
        if (!parts || parts[0] !== req.method) throw new WireError("unknown_method");
        if (parts[1] === "runtime" && this.#runtime) {
          if (parts[2] !== "stop") throw new WireError("unknown_method");
          if (Object.keys(req.params).length !== 0) throw new WireError("invalid_request");
          const runtime = this.#runtime;
          this.#write(encode({ ...identity(req), ok: true, result: {} }), () => runtime.stop());
          return;
        }
        if (parts[1] === "settings" && this.#settings) {
          if (parts[2] !== "get" && parts[2] !== "set") throw new WireError("unknown_method");
          if (parts[2] === "get" && Object.keys(req.params).length !== 0) throw new WireError("invalid_request");
          const result = parts[2] === "get" ? await this.#settings.get() : await this.#settings.set(parseSettingsChange(req.params));
          checkSettings(result);
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "storeIdentity" && this.#storeIdentity) {
          if (parts[2] !== "get") throw new WireError("unknown_method");
          if (Object.keys(req.params).length !== 0) throw new WireError("invalid_request");
          this.#write(encode({ ...identity(req), ok: true, result: this.#storeIdentity }));
          return;
        }
        if (parts[1] === "checkpoints" && this.#checkpoints) {
          const result = await this.#checkpoints.call(parseCheckpoint(parts[2]!, req.params), this.#abort.signal);
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "plan" && this.#plan) {
          const host = this.#plan, signal = this.#abort.signal;
          if (parts[2] === "wait") {
            const { threadId, after } = parsePlanWait(req.params);
            const workspace = this.#planWorkspace;
            this.#metadata(req, parts[1], async () => {
              const snapshot = await host.wait(threadId, after, signal, undefined, !!this.#planChat);
              if (workspace || snapshot.run?.state !== "closed") return snapshot;
              // Preserve the original plan:1 state vocabulary for older clients.
              return { ...snapshot, run: { ...snapshot.run, state: "failed", failure: "The plan closed before the remaining tasks started." } };
            });
            return;
          }
          const result = parts[2] === "run" ? await host.run(parsePlanRun(req.params), signal)
            : parts[2] === "attach" ? await host.attach(parsePlanAttach(req.params), signal) : undefined;
          if (!result) throw new WireError("unknown_method");
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "planWorktree" && this.#planWorktree) {
          const host = this.#planWorktree, signal = this.#abort.signal;
          // Checkout/model work can take seconds; keep the peer's request
          // admission available without allowing concurrent prepares on it.
          if (parts[2] === "prepare") {
            const query = parsePlanWorktreeSource(req.params);
            this.#metadata(req, parts[1], () => host.prepare(query, signal));
            return;
          }
          if (parts[2] !== "start") throw new WireError("unknown_method");
          const result = await host.start(parsePlanWorktreeStart(req.params), signal);
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "planChatActivity" && this.#planChatActivity && this.#planChat?.activity) {
          if (parts[2] !== "wait") throw new WireError("unknown_method");
          const { threadId, after } = parsePlanWait(req.params);
          this.#metadata(req, parts[1], () => this.#planChat!.activity!(threadId, after, this.#abort.signal));
          return;
        }
        if (parts[1] === "planChat" && this.#planChat) {
          const host = this.#planChat, signal = this.#abort.signal;
          if (parts[2] === "workspace") {
            const query = parsePlanAt(req.params, false);
            this.#metadata(req, parts[1], () => host.workspaceAt(query, signal, this.#planCatalog));
            return;
          }
          if (parts[2] === "inspect") {
            const query = parsePlanAt(req.params, true);
            this.#metadata(req, parts[1], () => host.inspectAt(query, signal, this.#planCatalog));
            return;
          }
          if (parts[2] !== "run") throw new WireError("unknown_method");
          const result = await host.runInChat(parsePlanRun(req.params), signal);
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "planDraft" && this.#planDraft) {
          if (parts[2] !== "save") throw new WireError("unknown_method");
          const result = await this.#planDraft.save(parsePlanDraftTarget(req.params), this.#abort.signal);
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "planCatalog" && this.#planCatalog && this.#plan?.catalog) {
          if (parts[2] !== "wait") throw new WireError("unknown_method");
          const query = parsePlanWorkspace(req.params), host = this.#plan;
          this.#metadata(req, parts[1], () => host.catalog!(query, this.#abort.signal, !!this.#planChat));
          return;
        }
        if (parts[1] === "planWorkspace" && this.#plan && this.#planWorkspace) {
          const host = this.#plan, signal = this.#abort.signal;
          if (parts[2] === "wait") {
            const query = parsePlanWorkspace(req.params);
            this.#metadata(req, parts[1], () => host.workspace(query, signal, !!this.#planChat));
            return;
          }
          if (parts[2] === "inspect") {
            const { threadId, after, planId } = parsePlanWait(req.params, true);
            this.#metadata(req, parts[1], () => host.wait(threadId, after, signal, planId, !!this.#planChat, this.#planCatalog));
            return;
          }
          const result = parts[2] === "setClosed" ? await host.setClosed(parsePlanClosure(req.params), signal)
            : parts[2] === "use" ? await host.attach(parsePlanAttach(req.params), signal, true)
            : parts[2] === "history" ? await host.history(parsePlanHistory(req.params), signal, !!this.#planChat, this.#planCatalog, !!this.#planWorktree) : undefined;
          if (!result) throw new WireError("unknown_method");
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "planReview" && this.#planReview) {
          const host = this.#planReview, signal = this.#abort.signal;
          const call = parsePlanReview(parts[2], req.params);
          if (call.method === "wait") {
            this.#metadata(req, parts[1], () => host.wait(call.query, signal));
            return;
          }
          const result = call.method === "start" ? await host.start(call.query, signal) : await host.cancel(call.query, signal);
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "loadedContext" && this.#loadedContext) {
          if (parts[2] !== "get") throw new WireError("unknown_method");
          const { host, version } = this.#loadedContext, threadId = parseLoadedContext(req.params);
          this.#metadata(req, parts[1], () => host.get(threadId, version, this.#abort.signal));
          return;
        }
        if (parts[1] === "workspaceScopeWatch" && this.#workspaceScopeWatch) {
          if (parts[2] !== "wait") throw new WireError("unknown_method");
          const { host, version } = this.#workspaceFiles!;
          const query = parseWorkspaceScopeWatch(req.params, version);
          this.#metadata(req, parts[1], () => host.waitScope!(query, this.#abort.signal));
          return;
        }
        if (parts[1] === "workspaceFiles" && this.#workspaceFiles) {
          const { host, version } = this.#workspaceFiles;
          if (parts[2] === "search" && version >= workspaceFileSearchVersion && host.search) {
            const search = host.search.bind(host), query = parseWorkspaceFileSearch(req.params, version);
            this.#metadata(req, parts[1], () => search(query, this.#abort.signal));
          } else {
            const query = parseWorkspaceFiles(parts[2]!, req.params, version);
            this.#metadata(req, parts[1], () => host.call(query, this.#abort.signal));
          }
          return;
        }
        if (parts[1] === "gitDiffWatch" && this.#gitDiffWatch) {
          if (parts[2] !== "wait") throw new WireError("unknown_method");
          const query = parseGitDiffWatch(req.params);
          this.#metadata(req, parts[1], () => this.#gitDiff!.wait!(query, this.#abort.signal));
          return;
        }
        if (parts[1] === "gitDiff" && this.#gitDiff) {
          const query = parseGitDiff(parts[2]!, req.params);
          if (this.#gitDiffPending) throw new WireError("limit");
          this.#gitDiffPending = true;
          // Disk scans are passive reads. A slow or cancelled review must not
          // block EOF, input admission, or abort on the same connection.
          void this.#gitDiff.call(query, this.#abort.signal).then(result => {
            if (!this.socket.destroyed) this.#write(encode({ ...identity(req), ok: true, result }));
          }).catch(error => {
            if (!this.socket.destroyed) this.#error(req, error);
          }).finally(() => { this.#gitDiffPending = false; });
          return;
        }
        if (parts[1] === "schedules" && this.#schedules) {
          const result = await this.#schedules.call(parseSchedule(parts[2]!, req.params), this.#abort.signal);
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "nativeStop" && this.#nativeStop) {
          if (parts[2] !== "stop") throw new WireError("unknown_method");
          const result = await this.adapter.stop(parseStop(req.params), this.#context);
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "modelCatalog" && this.#modelCatalog) {
          const { host, version } = this.#modelCatalog;
          if (parts[2] === "new" && version === modelCatalogDraftVersion) {
            const result = await this.adapter.createCatalogThread(parseCatalogCreation(req.params), this.#context);
            this.#write(encode({ ...identity(req), ok: true, result }));
            return;
          }
          if (parts[2] !== "list") throw new WireError("unknown_method");
          const { threadId, query } = parseCatalogQuery(req.params, version);
          const signal = this.#abort.signal;
          const result = threadId === undefined ? await host.draft!(query, signal) :
            query ? await host.search!(threadId, query, signal) : await host.list(threadId, signal);
          checkCatalog(result, !!query);
          if (threadId === undefined && result.current !== null) throw new WireError("internal");
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "intro" && this.#intro) {
          const { host, version } = this.#intro;
          if (parts[2] !== "get") {
            if (version !== 2 || !host.act) throw new WireError("unknown_method");
            const result = await host.act(parseIntroAction(parts[2]!, req.params), this.#abort.signal);
            this.#write(encode(checkIntroReply(version, { ...identity(req), ok: true, result })));
            return;
          }
          const query = parseIntroQuery(req.params);
          if (this.#introPending) throw new WireError("limit");
          this.#introPending = true;
          // A passive wait must not stop the peer from observing EOF.
          void host.get(query, this.#abort.signal, version).then(result => {
            this.#introPending = false;
            if (!this.socket.destroyed) this.#write(encode(checkIntroReply(version, { ...identity(req), ok: true, result })));
          }).catch(error => {
            this.#introPending = false;
            if (!this.socket.destroyed) this.#error(req, error);
          });
          return;
        }
        if (parts[1] === "threadDirectory" && this.#threadDirectory) {
          const { host, version, workspace } = this.#threadDirectory;
          const signal = this.#abort.signal;
          const reply = (result: object, rows: Parameters<typeof checkThreadDirectoryReply>[2], shape?: ThreadWorkspaceVersion) => {
            if (!this.socket.destroyed) this.#write(encode(checkThreadDirectoryReply(version, { ...identity(req), ok: true, result }, rows, shape)));
          };
          if (parts[2] === "list") {
            const query = parseThreadDirectoryQuery(req.params, version, workspace);
            const search = workspace === threadWorkspaceSearchVersion;
            const list = async () => {
              const result = await host.list(query, signal, version, workspace);
              const scoped = query.workspace !== undefined, matches = result.workspaceMatches;
              if (result.threads.length > query.limit || (scoped ? matches?.length !== result.threads.length ||
                  !query.browseAll && matches?.some(match => match !== "inside") : matches !== undefined) ||
                  search && result.workspaces?.length !== result.threads.length) throw new WireError("internal");
              reply(result, result.threads, search || scoped ? workspace : undefined);
            };
            if (query.query === undefined) await list();
            else {
              if (this.#directoryPending) throw new WireError("limit");
              this.#directoryPending = true;
              // Keep observing EOF so a superseded picker search can be cancelled.
              void list().catch(error => {
                if (!this.socket.destroyed) this.#error(req, error);
              }).finally(() => { this.#directoryPending = false; });
            }
            return;
          }
          const actions = version === 2 ? host.management : undefined;
          if (!actions) throw new WireError("unknown_method");
          switch (parts[2]) {
            case "get": {
              const query = parseThreadLookup(req.params);
              if (this.#directoryPending) throw new WireError("limit");
              this.#directoryPending = true;
              void Promise.resolve().then(() => actions.get(query, signal)).then(result => {
                if (result.threads.some(row => !query.threadIds.includes(row.id))) throw new WireError("internal");
                reply(result, result.threads);
              }).catch(error => {
                if (!this.socket.destroyed) this.#error(req, error);
              }).finally(() => { this.#directoryPending = false; });
              return;
            }
            case "new": {
              const result = await actions.create(parseThreadCreation(req.params), signal);
              reply(result, [result]);
              return;
            }
            case "archive":
              await actions.archive(parseThreadArchive(req.params), signal);
              reply({}, []);
              return;
            default: throw new WireError("unknown_method");
          }
        }
        if (parts[1] === "clientEnvironment" && this.#clientEnvironment) {
          if (parts[2] !== "submit") throw new WireError("unknown_method");
          const { input, environment } = req.params;
          if (Object.keys(req.params).length !== 2 || !Object.hasOwn(req.params, "environment") || !object(input)) {
            throw new WireError("invalid_request");
          }
          const result = await this.#submitInput(req, input, clientEnvironment(environment));
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "workspaceSkills" && this.#workspaceSkills) {
          if (parts[2] === "list") {
            const { host, version } = this.#workspaceSkills;
            const scope = parseWorkspaceSkillList(req.params, version);
            this.#metadata(req, parts[1], () => host.list(scope, this.#abort.signal));
            return;
          }
          if (parts[2] !== "submit") throw new WireError("unknown_method");
          const { input, token, environment } = parseWorkspaceSkillSubmit(req.params);
          if (environment !== undefined && !this.#clientEnvironment) throw new WireError("unsupported_capability", "clientEnvironment");
          const result = await this.#submitInput(req, input, environment === undefined ? undefined : clientEnvironment(environment), token);
          this.#write(encode({ ...identity(req), ok: true, result }));
          return;
        }
        if (parts[1] === "images" && this.#images) {
          const result = parts[2] === "get" ? await this.adapter.getImage(parseImageRef(req.params), this.#context)
            : parts[2] === "submit" ? await this.adapter.submitImages(parseImageSubmit(req.params), this.#images, this.#abort.signal, this.#context)
            : undefined;
          if (!result) throw new WireError("unknown_method");
          this.#write(encode(imageReply({ ...identity(req), ok: true, result })));
          return;
        }
        if (parts[1] === "workerActivity" && this.#workerActivity) {
          const host = this.#delegation!, signal = this.#abort.signal;
          if (parts[2] === "wait") {
            const query = parseWorkerActivity(req.params);
            this.#metadata(req, "workerActivity.wait", () => host.waitActivity!(query, signal));
          } else if (parts[2] === "inspect") {
            const query = parseWorkerInspection(req.params);
            this.#metadata(req, "workerActivity.inspect", () => host.inspect!(query, signal));
          } else throw new WireError("unknown_method");
          return;
        }
        if (parts[1] === "delegation" && this.#delegation) {
          const host = this.#delegation, signal = this.#abort.signal;
          if (parts[2] === "get") {
            const key = parseReviewKey(req.params);
            this.#metadata(req, "delegation.get", () => host.get(key, signal));
          } else {
            if (parts[2] === "start" && Object.hasOwn(req.params, "assessment") && !this.#reviewEvidence)
              throw new WireError("unsupported_capability", "reviewEvidence");
            const result = parts[2] === "start" ? await host.start(parseReviewLaunch(req.params), signal)
              : parts[2] === "stop" ? await host.stop(parseWorkerKey(req.params), signal) : undefined;
            if (!result) throw new WireError("unknown_method");
            this.#write(encode({ ...identity(req), ok: true, result }));
          }
          return;
        }
        throw new WireError("unsupported_capability", parts[1]);
      }
      if (!validRequest(req)) throw new WireError("invalid_request");
      if (req.method === "hello") {
        if (this.#version) throw new WireError("already_negotiated");
        if (!req.params.versions.includes(1)) throw new WireError("unsupported_version");
        const capabilities: Record<string, number> = {};
        if (this.capabilities?.delegation && req.params.capabilities?.delegation?.includes(delegationVersion)) {
          capabilities.delegation = delegationVersion;
          this.#delegation = this.capabilities.delegation;
          if (this.#delegation.evidence && req.params.capabilities?.reviewEvidence?.includes(reviewEvidenceVersion)) {
            capabilities.reviewEvidence = reviewEvidenceVersion;
            this.#reviewEvidence = true;
          }
          if (this.#delegation.waitActivity && this.#delegation.inspect && req.params.capabilities?.workerActivity?.includes(workerActivityVersion)) {
            capabilities.workerActivity = workerActivityVersion;
            this.#workerActivity = true;
          }
        }
        if (this.capabilities?.hushProfile === 1 && req.params.capabilities?.hushProfile?.includes(1)) {
          capabilities.hushProfile = 1;
        }
        if (this.capabilities?.workspaceWorkTrust === 1 && req.params.capabilities?.workspaceWorkTrust?.includes(1)) {
          capabilities.workspaceWorkTrust = 1;
        }
        if (this.capabilities?.storeIdentity && req.params.capabilities?.storeIdentity?.includes(storeIdentityVersion)) {
          capabilities.storeIdentity = storeIdentityVersion;
          this.#storeIdentity = this.capabilities.storeIdentity;
        }
        if (this.capabilities?.runtime && req.params.capabilities?.runtime?.includes(runtimeVersion)) {
          capabilities.runtime = runtimeVersion;
          this.#runtime = this.capabilities.runtime;
        }
        if (this.capabilities?.settings && req.params.capabilities?.settings?.includes(settingsVersion)) {
          capabilities.settings = settingsVersion;
          this.#settings = this.capabilities.settings;
        }
        if (this.capabilities?.clientEnvironment === clientEnvironmentVersion &&
            req.params.capabilities?.clientEnvironment?.includes(clientEnvironmentVersion)) {
          capabilities.clientEnvironment = clientEnvironmentVersion;
          this.#clientEnvironment = true;
        }
        if (this.capabilities?.nativeStop === nativeStopVersion && req.params.capabilities?.nativeStop?.includes(nativeStopVersion)) {
          capabilities.nativeStop = nativeStopVersion;
          this.#nativeStop = true;
        }
        if (this.capabilities?.checkpoints && req.params.capabilities?.checkpoints?.includes(checkpointsVersion)) {
          capabilities.checkpoints = checkpointsVersion;
          this.#checkpoints = this.capabilities.checkpoints;
        }
        if (this.capabilities?.plan && req.params.capabilities?.plan?.includes(planVersion)) {
          capabilities.plan = planVersion;
          this.#plan = this.capabilities.plan;
          if (this.capabilities.planDraft && req.params.capabilities?.planDraft?.includes(planDraftVersion)) {
            capabilities.planDraft = planDraftVersion;
            this.#planDraft = this.capabilities.planDraft;
          }
          if (req.params.capabilities?.planWorkspace?.includes(planWorkspaceVersion)) {
            capabilities.planWorkspace = planWorkspaceVersion;
            this.#planWorkspace = true;
            if (this.capabilities.planChat && req.params.capabilities?.planChat?.includes(planChatVersion)) {
              capabilities.planChat = planChatVersion;
              this.#planChat = this.capabilities.planChat;
              if (this.#planChat.activity && req.params.capabilities?.planChatActivity?.includes(planChatActivityVersion)) {
                capabilities.planChatActivity = planChatActivityVersion;
                this.#planChatActivity = true;
              }
              if (this.capabilities.planWorktree && req.params.capabilities?.planWorktree?.includes(planWorktreeVersion)) {
                this.#planWorktree = this.capabilities.planWorktree;
                capabilities.planWorktree = planWorktreeVersion;
              }
            }
            if (this.#plan.catalog && req.params.capabilities?.planCatalog?.includes(planCatalogVersion)) {
              capabilities.planCatalog = planCatalogVersion;
              this.#planCatalog = true;
            }
            if (this.capabilities.planReview && req.params.capabilities?.planReview?.includes(planReviewVersion)) {
              capabilities.planReview = planReviewVersion;
              this.#planReview = this.capabilities.planReview;
            }
          }
        }
        const loadedContext = this.capabilities?.loadedContext, offeredContext = req.params.capabilities?.loadedContext;
        const contextVersion = offeredContext?.includes(loadedContextAgentsVersion) ? loadedContextAgentsVersion
          : offeredContext?.includes(loadedContextVersion) ? loadedContextVersion : undefined;
        if (loadedContext && contextVersion !== undefined) {
          capabilities.loadedContext = contextVersion;
          this.#loadedContext = { host: loadedContext, version: contextVersion };
        }
        const skills = this.capabilities?.workspaceSkills, offeredSkills = req.params.capabilities?.workspaceSkills;
        const skillsVersion = offeredSkills?.includes(workspaceSkillDraftVersion) ? workspaceSkillDraftVersion
          : offeredSkills?.includes(workspaceSkillsVersion) ? workspaceSkillsVersion : undefined;
        if (skills && skillsVersion !== undefined) {
          capabilities.workspaceSkills = skillsVersion;
          this.#workspaceSkills = { host: skills, version: skillsVersion };
        }
        const files = this.capabilities?.workspaceFiles, offeredFiles = req.params.capabilities?.workspaceFiles;
        if (files) {
          const version = files.search && files.projectPaths && offeredFiles?.includes(workspaceFileProjectPathsVersion) ? workspaceFileProjectPathsVersion :
            files.search && offeredFiles?.includes(workspaceFileSearchVersion) ? workspaceFileSearchVersion :
            offeredFiles?.includes(workspaceFilesVersion) ? workspaceFilesVersion : undefined;
          if (version !== undefined) {
            capabilities.workspaceFiles = version;
            this.#workspaceFiles = { host: files, version };
            if (files.waitScope && req.params.capabilities?.workspaceScopeWatch?.includes(workspaceScopeWatchVersion)) {
              capabilities.workspaceScopeWatch = workspaceScopeWatchVersion;
              this.#workspaceScopeWatch = true;
            }
          }
        }
        if (this.capabilities?.gitDiff && req.params.capabilities?.gitDiff?.includes(gitDiffVersion)) {
          capabilities.gitDiff = gitDiffVersion;
          this.#gitDiff = this.capabilities.gitDiff;
          if (this.#gitDiff.wait && req.params.capabilities?.gitDiffWatch?.includes(gitDiffWatchVersion)) {
            capabilities.gitDiffWatch = gitDiffWatchVersion;
            this.#gitDiffWatch = true;
          }
        }
        if (this.capabilities?.schedules && req.params.capabilities?.schedules?.includes(schedulesVersion)) {
          capabilities.schedules = schedulesVersion;
          this.#schedules = this.capabilities.schedules;
        }
        const modelCatalog = this.capabilities?.modelCatalog, offeredCatalog = req.params.capabilities?.modelCatalog;
        if (modelCatalog) {
          const version = modelCatalog.search && modelCatalog.draft && this.adapter.supportsCatalogCreation &&
            offeredCatalog?.includes(modelCatalogDraftVersion) ? modelCatalogDraftVersion :
            modelCatalog.search && offeredCatalog?.includes(modelCatalogSearchVersion) ? modelCatalogSearchVersion :
              offeredCatalog?.includes(modelCatalogVersion) ? modelCatalogVersion : undefined;
          if (version !== undefined) {
            capabilities.modelCatalog = version;
            this.#modelCatalog = { host: modelCatalog, version };
          }
        }
        const intro = this.capabilities?.intro, offeredIntro = req.params.capabilities?.intro;
        if (intro) {
          const version = intro.act && offeredIntro?.includes(2) ? 2 : offeredIntro?.includes(1) ? 1 : undefined;
          if (version !== undefined) {
            capabilities.intro = version;
            this.#intro = { host: intro, version };
          }
        }
        const directory = this.capabilities?.threadDirectory, offeredDirectory = req.params.capabilities?.threadDirectory;
        if (directory) {
          const v2 = directory.management;
          const version = v2 && offeredDirectory?.includes(threadManagementVersion) ? threadManagementVersion
            : offeredDirectory?.includes(threadDirectoryVersion) ? threadDirectoryVersion : undefined;
          if (version !== undefined) {
            const offeredWorkspace = req.params.capabilities?.threadWorkspace;
            const workspace = version !== threadManagementVersion || !directory.workspace ? undefined
              : directory.workspace === threadWorkspaceSearchVersion && offeredWorkspace?.includes(threadWorkspaceSearchVersion) ? threadWorkspaceSearchVersion
              : offeredWorkspace?.includes(threadWorkspaceVersion) ? threadWorkspaceVersion : undefined;
            if (workspace) capabilities.threadWorkspace = workspace;
            capabilities.threadDirectory = version;
            this.#threadDirectory = { host: directory, version, workspace };
          }
        }
        if (typeof this.capabilities?.images?.fit === "function" && req.params.capabilities?.images?.includes(imagesVersion)) {
          capabilities.images = imagesVersion;
          this.#images = this.capabilities.images;
        }
        this.#version = 1;
        this.#reply(req, { version: 1, epoch: this.epoch, capabilities });
        return;
      }
      if (req.method === "subscribe") {
        if (req.params.window && req.params.window.min > req.params.window.max) throw new WireError("invalid_request");
        const feed = new Feed(this.epoch, this.pendingObservations, this.#write, () => this.socket.writableNeedDrain);
        let adopted = false;
        try {
          const ready = await feed.attach(this.adapter, req.params, this.#context, !!this.#delegation);
          const encoded = this.#success(req, ready);
          if (this.#write(encoded)) {
            this.#feed?.close();
            this.#feed = feed;
            adopted = true;
            feed.arm();
          }
        } finally {
          if (!adopted) feed.close();
        }
        return;
      }

      if ((req.method === "thread.wait" || req.method === "submission.wait") && this.#waiting >= MAX_PENDING_WAITS) {
        throw new WireError("limit");
      }
      const action = await this.adapter.admit(req, this.#context, !!this.#modelCatalog, undefined, undefined, !!this.#delegation);
      if ("wait" in action) {
        this.#waiting++;
        void action.wait.then(
          (value) => { this.#waiting--; this.#reply(req, value); },
          (error) => { this.#waiting--; this.#error(req, error); },
        );
      } else this.#reply(req, action.result);
    } catch (error) {
      this.#error(req, error);
    }
  }

  #success(req: Envelope, result: object) {
    const frame = { ...identity(req), ok: true, result };
    if (!successShape(frame)) throw new WireError("internal");
    return encode(frame);
  }
  #reply(req: Envelope, result: object) {
    if (this.socket.destroyed) return;
    try { this.#write(this.#success(req, result)); }
    catch (error) { this.#error(req, error); }
  }
  #error(req: Envelope, error: unknown) {
    const { code, message, capability } = requestError(error, this.readOnlyReason);
    this.#write(encode({ ...identity(req), ok: false, error: capability ? { code, message, capability } : { code, message } }));
  }
  #write: Write = (record, flushed) => {
    if (!this.socket.writable || this.socket.destroyed) return false;
    if (this.socket.writableLength + Buffer.byteLength(record) > MAX_BACKLOG) {
      this.socket.destroy();
      return false;
    }
    this.socket.write(record, flushed);
    return true; // Accepted by Node; writableNeedDrain indicates backpressure.
  };
}

/** A server epoch over an already-open Harness. The host owns storage, socket permissions, and scheduler lifecycle. */
export function createCoreServer(harness: Harness, options: ServerOptions = {}) {
  const epoch = randomUUID();
  const identity = options.capabilities?.storeIdentity;
  if (identity !== undefined && !validStoreIdentity(identity)) throw new TypeError("Invalid store identity");
  const capabilities = identity === undefined ? options.capabilities :
    { ...options.capabilities, storeIdentity: Object.freeze({ ...identity }) };
  const adapter = new DurableAdapter(harness, options.host);
  const peers = new Set<Socket>();
  const maxConnections = options.maxConnections ?? 512;
  const pendingObservations = options.pendingObservations ?? 64;
  for (const bound of [maxConnections, pendingObservations]) {
    if (!Number.isSafeInteger(bound) || bound < 1) throw new RangeError("Invalid server limit");
  }
  let closing: Promise<void> | undefined;
  const changed = () => options.onConnections?.(peers.size);
  const listener = createServer(socket => {
    if (closing || peers.size >= maxConnections) { socket.destroy(); return; }
    peers.add(socket);
    socket.once("close", () => { peers.delete(socket); changed(); });
    const peer = new Peer(socket, adapter, epoch, pendingObservations, capabilities, options.readOnlyReason);
    changed();
    void peer.run();
  });
  return {
    epoch,
    get connections() { return peers.size; },
    listen(path: string): Promise<void> {
      return new Promise((resolve, reject) => {
        if (closing || listener.listening) { reject(new Error("Server cannot listen again")); return; }
        const onError = (error: Error) => { listener.off("listening", onListening); reject(error); };
        const onListening = () => { listener.off("error", onError); resolve(); };
        listener.once("error", onError);
        listener.once("listening", onListening);
        listener.listen(path);
      });
    },
    close(): Promise<void> {
      return closing ??= new Promise((resolve, reject) => {
        for (const socket of peers) socket.destroy();
        listener.close(error => {
          if (error && (error as NodeJS.ErrnoException).code !== "ERR_SERVER_NOT_RUNNING") reject(error);
          else resolve();
        });
      });
    },
  };
}
