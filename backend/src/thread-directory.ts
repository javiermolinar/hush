import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { basename, dirname, isAbsolute, resolve } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { setImmediate as yieldTurn, setTimeout as delay } from "node:timers/promises";
import { promisify } from "node:util";
import { BACKGROUND_CONTEXT, withAbortSignal, type Context } from "@earendil-works/chord/context";
import type { Message } from "@earendil-works/pi-ai";
import { AgentDoc, LiveDoc, type ConversationId, type ConversationRecord, type Cursor, type EntryId, type EntryRecord, type Harness } from "@earendil-works/pi-durable";
import { Cursors } from "../../server/src/cursor.js";
import type { NativeHost } from "../../server/src/durable.js";
import { threadWorkspaceSearchVersion, type ManagedThreadRow, type ThreadArchive, type ThreadCreation, type ThreadDirectoryHost,
  type ThreadDirectoryPage, type ThreadDirectoryQuery, type ThreadDirectoryReport, type ThreadDirectoryRow, type ThreadDirectoryVersion,
  type ThreadLookup, type ThreadStatus, type ThreadWorkspaceVersion, type WorkspaceMatch } from "../../server/src/thread-directory.js";
import { WireError } from "../../server/src/wire.js";
import { actionableFailure, displayTitle, markCheckoutReady, threadActivity, ThreadPresentationDoc, threadLabel, topicTitle } from "./thread-presentation.js";
import type { GenerateThreadTitle } from "./thread-title-model.js";
import { existingDirectory, type ManagedCheckout, type ThreadWorktrees } from "./worktrees.js";

const historyBatch = 16;
const inventoryBatch = 32;
const topicExcerpt = 768;
const providerExcerpt = 72;
const git = promisify(execFile);

// Only committed, visible chat text counts; tool results, thinking and
// administrative records must not move a conversation up the list.
function chatRole(message: Message) {
  return message.role === "user" || message.role === "assistant";
}

function* chatBlocks(message: Message) {
  if (!chatRole(message)) return;
  const display = (text: string) => {
    if (message.role !== "user" || !text.startsWith("<skill name=")) return text;
    const skill = /^<skill name="([^"\r\n]{1,96})" location=/.exec(text)?.[1];
    return skill ? threadLabel("/skill:" + skill, 72) : "Skill invocation";
  };
  if (typeof message.content === "string") yield display(message.content);
  else for (const part of message.content) {
    if (part.type === "text") yield display(part.text);
  }
}

function chatText(message: Message, max = providerExcerpt) {
  for (const block of chatBlocks(message)) {
    const text = threadLabel(block, max);
    if (text) return text;
  }
  return "";
}

function searchText(text: string) {
  return text.toWellFormed().normalize("NFC").toLowerCase().replace(/[\p{Cc}\s]+/gu, " ").trim();
}

function workspaceName(key: string) {
  const path = key.slice(4);
  const name = key.startsWith("git:") ? basename(path) === ".git" ? basename(dirname(path)) : basename(path, ".git") : basename(path);
  return threadLabel(name || path, 96) || "Workspace";
}

function firstPrompt(entry: EntryRecord) {
  for (const message of entry.model ?? []) {
    if (message.role !== "user") continue;
    const text = chatText(message);
    if (text) return text;
  }
  return "";
}

function lastMessage(entry: EntryRecord) {
  const messages = entry.model ?? [];
  for (let n = messages.length - 1; n >= 0; n--) {
    const msg = messages[n]!;
    if (Number.isSafeInteger(msg.timestamp) && msg.timestamp > 0 && chatText(msg)) return msg.timestamp;
  }
  return 0;
}

function completedAnswer(message: Message) {
  return message.role === "assistant" && (message.stopReason === "stop" || message.stopReason === "length");
}

function lastOutcome(entry: EntryRecord): "idle" | "done" | undefined {
  if (entry.kind === "pi.reset") return "idle";
  for (let n = (entry.model?.length ?? 0) - 1; n >= 0; n--) {
    const msg = entry.model![n]!;
    if (msg.role === "user") return "idle";
    if (completedAnswer(msg)) return "done";
  }
  return undefined;
}

type DirectoryRow = { id: ConversationId; parent: number | null; title: string; last: number;
  archived: 0 | 1; status: ThreadStatus; cwd: string; branch: string; workspace: string };
type Fence = { last: number; id: number };
const ownerless = { kind: "ownerless" } as const;
const liveFields = new Set(["run", "runFailure", "compactions", "compactionFailure"]);

// The ordering index is disposable. A cooperative worker saves a title from
// the first completed exchange in Hush's presentation document, including for
// older threads. Queries read bounded pages without opening conversation watches.
export class ThreadDirectory implements ThreadDirectoryHost {
  readonly workspace = threadWorkspaceSearchVersion;
  readonly #life = new AbortController();
  readonly #db = new DatabaseSync(":memory:");
  readonly #pending = new Set<ConversationId>();
  readonly #cursors = new Cursors();
  readonly #detach: () => void;
  readonly #ctx: Context;
  readonly #waiters = new Set<() => void>();
  readonly #mutating = new Set<Promise<unknown>>();
  readonly #titleCandidates = new Set<ConversationId>();
  readonly #chatThrough = new Map<ConversationId, EntryId>();
  #titlesGenerating = 0;
  readonly #graph: Promise<void>;
  #activity: ReadonlyMap<ConversationId, ThreadStatus> = new Map();
  #task?: Promise<void>;
  #revision = 1;
  #indexing = true;
  #failed = false;

  constructor(private readonly h: Harness, private readonly cwd: string, private readonly defaults: NonNullable<NativeHost["defaults"]>,
      private readonly worktrees: ThreadWorktrees, private readonly generateTitle?: GenerateThreadTitle,
      private readonly readOnlyReason?: string) {
    this.#ctx = withAbortSignal(this.#life.signal, BACKGROUND_CONTEXT);
    this.#db.exec(`
      CREATE TABLE directory (id INTEGER PRIMARY KEY, parent INTEGER, title TEXT NOT NULL, last INTEGER NOT NULL,
        archived INTEGER NOT NULL, status TEXT NOT NULL, cwd TEXT NOT NULL, branch TEXT NOT NULL, workspace TEXT NOT NULL,
        search_title TEXT NOT NULL, search_workspace TEXT NOT NULL);
      CREATE VIRTUAL TABLE directory_chat USING fts5(body, content = '', tokenize = 'trigram');
      CREATE TABLE directory_chat_threads (entry INTEGER NOT NULL, thread INTEGER NOT NULL, PRIMARY KEY (entry, thread)) WITHOUT ROWID;
      CREATE INDEX directory_recent ON directory(last DESC, id DESC);
      CREATE INDEX directory_visible ON directory(last DESC, id DESC) WHERE archived = 0;
      CREATE INDEX directory_checkout ON directory(cwd);
      CREATE INDEX directory_workspace ON directory(workspace, last DESC, id DESC);
      CREATE INDEX directory_workspace_visible ON directory(workspace, last DESC, id DESC) WHERE archived = 0;
    `);
    this.#detach = h.observeCommits({
      advance: ({ changes }) => {
        // The observer only coalesces owners and schedules work; no SQL,
        // history projection or filesystem I/O on the commit line.
        for (const c of changes) {
          if (c.type === "conversation") this.#pending.add(c.value.id);
          else if (c.type === "entry" && (c.value.kind === "pi.reset" || c.value.model?.some(chatRole))) {
            const id = c.value.conversationId;
            this.#pending.add(id);
            if (this.#titleCandidates.size < 64 && c.value.model?.some(completedAnswer)) {
              this.#titleCandidates.add(id);
            }
          }
          else if ((c.type === "document" || c.type === "document.copy") && c.conversationId !== undefined) {
            const kind = c.record.kind;
            if (kind === ThreadPresentationDoc.definition.kind || kind === AgentDoc.definition.kind ||
                kind === LiveDoc.definition.kind && (c.type !== "document" || c.kind !== "change" ||
                  c.ops.some(op => op[0] === "r" || Array.isArray(op[1]) && liveFields.has(String(op[1][0]))))) {
              this.#pending.add(c.conversationId);
            }
          }
        }
        if (this.#pending.size) this.#schedule();
      },
      end: () => this.#fail(),
    }).detach;
    this.#graph = this.h.watchTaskGraph(this.#ctx).then(watch => {
      this.#updateActivity(threadActivity(watch.value));
      watch.start(async call => { if (call.kind === "value") this.#updateActivity(threadActivity(call.value)); });
      void watch.closed.then(() => this.#fail());
    }).catch(() => this.#fail());
    this.#schedule();
  }

  readonly management = {
    get: (query: ThreadLookup, peer: AbortSignal) => this.#get(query, peer),
    create: (query: ThreadCreation, peer: AbortSignal) => this.#track(this.#create(query, this.#signal(peer))),
    archive: (query: ThreadArchive, peer: AbortSignal) => this.#track(this.#archive(query, this.#signal(peer))),
  };

  #signal(peer: AbortSignal) { return AbortSignal.any([peer, this.#life.signal]); }

  #track<T>(op: Promise<T>): Promise<T> {
    this.#mutating.add(op);
    return op.finally(() => { this.#mutating.delete(op); });
  }

  #advance() { this.#revision++; for (const wake of this.#waiters) wake(); }

  #fail() { if (!this.#life.signal.aborted) { this.#failed = true; this.#advance(); } }

  #updateActivity(next: ReadonlyMap<ConversationId, ThreadStatus>) {
    const changed = next.size !== this.#activity.size || [...next].some(([id, status]) => this.#activity.get(id) !== status);
    this.#activity = next;
    if (changed) this.#advance();
  }

  #schedule() {
    if (this.#task || this.#failed || this.#life.signal.aborted) return;
    const signal = this.#life.signal;
    this.#task = yieldTurn(undefined, { signal }).then(async () => {
      if (this.#indexing) await this.#bootstrap();
      while (this.#pending.size) {
        let count = 0;
        for (const id of this.#pending) {
          if (++count > inventoryBatch) break;
          this.#pending.delete(id);
          const candidate = this.#titleCandidates.delete(id);
          const record = await this.h.commit(tx => tx.conversation(id), this.#ctx);
          if (record && !record.owner) await this.#refresh(record, candidate);
          await yieldTurn(undefined, { signal });
        }
      }
    }).catch(() => this.#fail()).finally(() => {
      this.#task = undefined;
      if (this.#pending.size) this.#schedule();
    });
  }

  async #bootstrap() {
    let cursor: Cursor | undefined;
    do {
      const page = await this.h.commit(tx => tx.scanConversations({ order: "descending" }, inventoryBatch, cursor), this.#ctx);
      for (const record of page.items) {
        if (!record.owner) await this.#refresh(record, false, true);
        await yieldTurn(undefined, { signal: this.#life.signal });
      }
      cursor = page.next;
    } while (cursor);
    this.#indexing = false;
    this.#advance();
  }

  async #find<T>(id: ConversationId, order: "ascending" | "descending", match: (entry: EntryRecord) => T): Promise<T | undefined> {
    let cursor: Cursor | undefined;
    do {
      const page = await this.h.commit(tx => tx.scanEntries({ conversationId: id, order }, historyBatch, cursor), this.#ctx);
      for (const entry of page.items) {
        const value = match(entry);
        if (value) return value;
      }
      cursor = page.next;
      if (cursor) await yieldTurn(undefined, { signal: this.#life.signal });
    } while (cursor);
    return undefined;
  }

  async #firstExchange(id: ConversationId) {
    let user: string | undefined;
    return this.#find(id, "ascending", entry => {
      for (const message of entry.model ?? []) {
        if (message.role === "user") user ??= chatText(message, topicExcerpt);
        else if (user !== undefined && completedAnswer(message)) return { user, answer: chatText(message, topicExcerpt) };
      }
    });
  }

  async #indexChat(id: ConversationId, maxEntryId: EntryId) {
    const through = this.#chatThrough.get(id);
    if (through !== undefined && through >= maxEntryId) return;
    const minEntryId = through === undefined ? undefined : (through + 1) as EntryId;
    const query = { conversationId: id, order: "descending" as const, minEntryId, maxEntryId };
    const known = this.#db.prepare("SELECT 1 FROM directory_chat_threads WHERE entry = ? LIMIT 1");
    const index = this.#db.prepare("INSERT INTO directory_chat(rowid, body) VALUES (?, ?)");
    const link = this.#db.prepare("INSERT INTO directory_chat_threads VALUES (?, ?)");
    let cursor: Cursor | undefined;
    do {
      const page = await this.h.commit(tx => tx.scanEntries(query, historyBatch, cursor), this.#ctx);
      this.#life.signal.throwIfAborted();
      for (const entry of page.items) {
        // A fork can see inherited entries. Index each entry once, then
        // record which threads can match it. Keep no plaintext copy in FTS.
        if (!known.get(entry.id)) {
          const body = (entry.model ?? []).map(message => searchText([...chatBlocks(message)].join(" "))).filter(Boolean).join("\n");
          if (!body) continue;
          index.run(entry.id, body);
        }
        link.run(entry.id, id);
      }
      cursor = page.next;
      if (cursor) await yieldTurn(undefined, { signal: this.#life.signal });
    } while (cursor);
    this.#chatThrough.set(id, maxEntryId);
  }

  async #refresh(record: ConversationRecord, candidate: boolean, bootstrap = false) {
    const { id } = record;
    const [presentation, agent, live] = await Promise.all([
      this.h.snapshot(ThreadPresentationDoc, id, this.#ctx), this.h.snapshot(AgentDoc, id, this.#ctx),
      this.h.snapshot(LiveDoc, id, this.#ctx),
    ]);
    let title = displayTitle(presentation);
    let last = 0;
    let newest: EntryId | undefined, outcome: "idle" | "done" | undefined;
    await this.#find(id, "descending", entry => {
      newest ??= entry.id;
      last ||= lastMessage(entry);
      outcome ??= lastOutcome(entry);
      return last !== 0 && outcome !== undefined;
    });
    const unnamed = !presentation?.title;
    const local = unnamed && !presentation?.topicTitle && (candidate || outcome === "done" || bootstrap && !!presentation?.autoTitle);
    const remote = this.generateTitle && candidate && unnamed && !record.parent &&
      !presentation?.archived && !presentation?.titleAttempted && this.#titlesGenerating < 2;
    const exchange = local || remote ? await this.#firstExchange(id) : undefined;
    const topic = local && exchange ? topicTitle(exchange.user, exchange.answer) : "";
    // An unanswered thread (including a fork cut before the first answer)
    // can still be recognized by its provisional prompt topic.
    const prompt = !title && !topic ? await this.#find(id, "ascending", firstPrompt) : undefined;
    if (this.readOnlyReason) title ||= topic || (prompt && topicTitle(prompt));
    else if (topic || prompt) title = await this.h.commit(async tx => {
      const doc = await tx.doc(ThreadPresentationDoc, id);
      if (!doc.title) {
        if (topic) doc.topicTitle ??= topic;
        else if (prompt) doc.autoTitle ??= prompt;
      }
      return displayTitle(doc);
    }, this.#ctx);
    const failure = live?.runFailure;
    const status = live?.run || live?.compactions?.length ? "running"
      : live?.compactionFailure ? "failed"
      : failure?.taskId !== undefined && failure.taskId === presentation?.dismissedFailure ? "idle"
      : failure?.reason === "aborted" ? "stopped" : failure && actionableFailure(failure.reason) ? "failed" : outcome ?? "idle";
    const cwd = wireCwd(agent?.cwd);
    const branch = cwd && cwd === presentation?.checkout?.cwd ? presentation.checkout.branch : "";
    const indexed = this.#db.prepare("SELECT workspace FROM directory WHERE id = ? AND cwd = ?").get(id, cwd) as Pick<DirectoryRow, "workspace"> | undefined;
    // A stable stored cwd keeps its identity for this index lifetime. A
    // missing/unresolvable cwd never falls into another workspace's scope.
    const workspace = indexed?.workspace || (cwd ? await workspaceIdentity(resolve(this.cwd, cwd), this.#life.signal).catch(() => "") : "");
    this.#life.signal.throwIfAborted();
    if (newest !== undefined) await this.#indexChat(id, newest);
    const update = this.#db.prepare(`INSERT INTO directory VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(id) DO UPDATE SET parent=excluded.parent, title=excluded.title, last=excluded.last, archived=excluded.archived,
        status=excluded.status, cwd=excluded.cwd, branch=excluded.branch, workspace=excluded.workspace,
        search_title=excluded.search_title, search_workspace=excluded.search_workspace
      WHERE (parent, title, last, archived, status, cwd, branch, workspace) IS NOT
        (excluded.parent, excluded.title, excluded.last, excluded.archived, excluded.status, excluded.cwd, excluded.branch, excluded.workspace)`)
      .run(id, record.parent?.conversationId ?? null, title ?? "", last, presentation?.archived ? 1 : 0, status, cwd, branch, workspace,
        searchText(title ?? ""), workspace ? searchText(workspaceName(workspace)) : "");
    if (update.changes) this.#advance();
    if (remote && exchange?.user && exchange.answer) {
      this.#titlesGenerating++;
      void this.#track(this.#generate(id, exchange)).catch(() => {}).finally(() => { this.#titlesGenerating--; });
    }
  }

  async #generate(id: ConversationId, { user, answer }: { user: string; answer: string }) {
    const claimed = await this.h.commit(async tx => {
      const doc = await tx.doc(ThreadPresentationDoc, id);
      if (doc.title || doc.archived || doc.titleAttempted) return false;
      doc.titleAttempted = true;
      return true;
    }, this.#ctx);
    if (!claimed) return;
    const title = await this.generateTitle!(threadLabel(user, providerExcerpt), threadLabel(answer, providerExcerpt), this.#life.signal);
    if (!title) return;
    await this.h.commit(async tx => {
      const doc = await tx.doc(ThreadPresentationDoc, id);
      if (!doc.title && !doc.archived) doc.generatedTitle = title;
    }, this.#ctx);
  }

  #legacy(row: DirectoryRow): ThreadDirectoryRow {
    return { id: String(row.id), ...(row.parent === null ? {} : { parentId: String(row.parent) }),
      ...(row.title ? { title: row.title } : {}), ...(row.last ? { lastMessageMs: row.last } : {}) };
  }

  #view(row: DirectoryRow): ManagedThreadRow {
    const { cwd, branch } = row;
    const shared = !branch || this.#indexing || this.#db.prepare("SELECT 1 FROM directory WHERE cwd = ? AND id != ? LIMIT 1").get(cwd, row.id);
    return { ...this.#legacy(row), status: this.#activity.get(row.id) ?? row.status, archived: row.archived === 1,
      checkout: shared ? { kind: "shared", ...(cwd && { cwd }) } : { kind: "managed", cwd, branch } };
  }

  async #get(query: ThreadLookup, peer: AbortSignal): Promise<ThreadDirectoryReport> {
    const signal = this.#signal(peer);
    await this.#graph;
    signal.throwIfAborted();
    if (query.after !== undefined && query.after > this.#revision) throw new WireError("invalid_request");
    if (!this.#failed && query.after === this.#revision) await this.#wait(signal);
    signal.throwIfAborted();
    if (this.#failed) throw new WireError("internal");
    const read = this.#db.prepare("SELECT * FROM directory WHERE id = ?");
    const threads = query.threadIds.flatMap(id => {
      const row = read.get(Number(id)) as DirectoryRow | undefined;
      return row ? [this.#view(row)] : [];
    });
    return { revision: this.#revision, threads };
  }

  #wait(signal: AbortSignal) {
    signal.throwIfAborted();
    return new Promise<void>((resolve, reject) => {
      const cleanup = () => { this.#waiters.delete(wake); signal.removeEventListener("abort", abort); };
      const wake = () => { cleanup(); resolve(); };
      const abort = () => { cleanup(); reject(signal.reason); };
      this.#waiters.add(wake);
      signal.addEventListener("abort", abort, { once: true });
    });
  }

  async #create(query: ThreadCreation, signal: AbortSignal): Promise<ManagedThreadRow> {
    signal.throwIfAborted();
    // Reject before an optional worktree can move files or refs.
    if (this.readOnlyReason) {
      const error = new WireError("invalid_request");
      error.message = this.readOnlyReason;
      throw error;
    }
    const title = query.title === undefined ? "" : threadLabel(query.title, 96);
    if (query.title !== undefined && !title) throw new WireError("invalid_request");
    const workspace = await existingDirectory(resolve(this.cwd, query.cwd ?? "."));
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const open = async (cwd: string, managed?: ManagedCheckout) => {
      const agent = await this.defaults(cwd, ctx);
      return (await this.h.createConversation({ ownership: ownerless, agent: { ...agent, cwd }, init: async (tx, id) => {
        if (title || managed) {
          const doc = await tx.doc(ThreadPresentationDoc, id);
          if (title) doc.title = title;
          if (managed) doc.checkout = managed;
        }
      } }, ctx)).id;
    };
    const row = (id: ConversationId, cwd: string, branch?: string) => ({ id: String(id), ...(title && { title }), status: "idle" as const,
      archived: false, checkout: branch === undefined ? { kind: "shared" as const, cwd } : { kind: "managed" as const, cwd, branch } });
    if (!query.worktree) return row(await open(workspace), workspace);
    // Reserve the thread before Git writes the checkout, so a failure leaves
    // a thread to find it by rather than an unrecorded checkout.
    let reserved: ConversationId | undefined;
    try {
      const managed = await this.worktrees.create(workspace, signal, { name: async () => ({ fallback: "" }),
        reserve: async ({ cwd, branch, repository }) => { reserved = await open(cwd, { cwd, branch, repository }); },
        created: async ({ cwd }) => { if (reserved !== undefined) await this.h.commit(tx => markCheckoutReady(tx, reserved!, cwd), BACKGROUND_CONTEXT); } });
      return row(reserved!, managed.cwd, managed.branch);
    } catch (error) {
      if (reserved === undefined) throw error;
      const failure = new WireError("invalid_request");
      failure.message = `Could not finish the worktree for thread ${reserved}. It may be incomplete; review it in /threads before using it.`;
      throw failure;
    }
  }

  async #archive(query: ThreadArchive, signal: AbortSignal) {
    const ctx = withAbortSignal(signal, BACKGROUND_CONTEXT);
    const id = Number(query.threadId) as ConversationId;
    await this.h.commit(async tx => {
      const row = await tx.conversation(id);
      if (!row || row.owner) throw new WireError("not_found");
      if (query.archived) {
        const live = await tx.doc(LiveDoc, id);
        if (live.run || live.compactions?.length) throw new WireError("busy");
      }
      const doc = await tx.doc(ThreadPresentationDoc, id);
      if (query.archived) doc.archived = true;
      else delete doc.archived;
    }, ctx);
  }

  async list(query: ThreadDirectoryQuery, peer: AbortSignal, version: ThreadDirectoryVersion, workspaceVersion?: ThreadWorkspaceVersion): Promise<ThreadDirectoryPage> {
    const signal = this.#signal(peer);
    signal.throwIfAborted();
    const term = query.query === undefined ? "" : searchText(query.query);
    if (query.query !== undefined && !term) throw new WireError("invalid_request");
    if (version === 2) await this.#graph;
    const workspace = query.workspace === undefined ? undefined : await workspaceIdentity(resolve(this.cwd, query.workspace), signal);
    const scoped = workspace !== undefined && !query.browseAll;
    const base = `thread-directory:${version}:${query.includeArchived ? "all" : "visible"}`;
    let scope = workspace === undefined ? base :
      `${base}:${query.browseAll ? "browse" : "within"}:${createHash("sha256").update(workspace).digest("base64url")}`;
    if (term) scope += ":search:" + createHash("sha256").update(term).digest("base64url");
    const fence = query.cursor === undefined ? undefined : this.#cursors.decode<Fence>(scope, query.cursor);
    if (this.#task) {
      // Often the initial catalogue or a pending commit is ready immediately.
      // Large inventories get a partial first page instead of blocking the picker.
      const wait = new AbortController();
      try {
        await Promise.race([this.#task, delay(120, undefined, { signal: AbortSignal.any([signal, wait.signal]) })]);
      } finally { wait.abort(); }
    }
    signal.throwIfAborted();
    if (this.#failed) throw new WireError("internal");
    const filters: string[] = [], params: Array<string | number> = [];
    if (!query.includeArchived) filters.push("archived = 0");
    if (scoped) { filters.push("workspace = ?"); params.push(workspace); }
    if (fence) { filters.push("(last,id) < (?,?)"); params.push(fence.last, fence.id); }
    if (term) {
      const matches = ["instr(search_title, ?) > 0", "instr(search_workspace, ?) > 0",
        "instr(replace(replace(search_workspace, '_', ' '), '-', ' '), ?) > 0"];
      params.push(term, term, term);
      if (Array.from(term).length >= 3) {
        // Trigrams require three characters. Treat user input as a literal
        // phrase, not FTS operators, column references, or wildcards.
        matches.push(`id IN (SELECT thread FROM directory_chat JOIN directory_chat_threads ON entry = directory_chat.rowid
          WHERE directory_chat MATCH ?)`);
        params.push('"' + term.replaceAll('"', '""') + '"');
      }
      filters.push(`(${matches.join(" OR ")})`);
    }
    const where = filters.length ? "WHERE " + filters.join(" AND ") : "";
    const rows = this.#db.prepare(`SELECT * FROM directory ${where} ORDER BY last DESC,id DESC LIMIT ?`)
      .all(...params, query.limit + 1) as unknown as DirectoryRow[];
    const indexing = this.#indexing;
    const selected = rows.slice(0, query.limit), tail = selected.at(-1);
    return {
      indexing,
      threads: selected.map(row => version === 2 ? this.#view(row) : this.#legacy(row)),
      ...(workspace !== undefined && { workspaceMatches: selected.map<WorkspaceMatch>(row =>
        !row.workspace ? "unknown" : row.workspace === workspace ? "inside" : "outside") }),
      ...(workspaceVersion === threadWorkspaceSearchVersion && { workspaces: selected.map(({ workspace: key }) => key ?
        { id: createHash("sha256").update(key).digest("hex"), name: workspaceName(key) } : null) }),
      nextCursor: !indexing && tail && rows.length > query.limit ? this.#cursors.encode(scope, { last: tail.last, id: tail.id }) : null,
    };
  }

  async close() {
    this.#detach();
    this.#life.abort();
    await Promise.allSettled([this.#task, this.#graph, ...this.#mutating]);
    this.#db.close();
  }
}

// Resolve launch and indexed directories identically on the backend host.
// Git's common directory groups subdirectories and linked worktrees. A plain
// folder has exact canonical-directory scope. No checkout is read or changed.
async function workspaceIdentity(path: string, signal: AbortSignal): Promise<string> {
  signal.throwIfAborted();
  const cwd = await existingDirectory(path);
  try {
    const { stdout } = await git("git", ["--no-optional-locks", "rev-parse", "--path-format=absolute", "--git-common-dir"], {
      cwd, signal, timeout: 2_000, maxBuffer: 32 * 1024,
      env: { PATH: process.env.PATH ?? "/usr/bin:/bin", HOME: process.env.HOME ?? "", LC_ALL: "C",
        GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null", GIT_TERMINAL_PROMPT: "0", GIT_NO_LAZY_FETCH: "1" },
    });
    const common = stdout.replace(/\r?\n$/, "");
    if (!isAbsolute(common) || /[\0\r\n]/.test(common)) throw new WireError("invalid_request");
    const canonical = await existingDirectory(common);
    signal.throwIfAborted();
    return "git:" + canonical;
  } catch (error) {
    signal.throwIfAborted();
    const failure = error as NodeJS.ErrnoException & { stderr?: string };
    if (failure.code === "ENOENT" || failure.stderr?.includes("not a git repository")) return "dir:" + cwd;
    throw new WireError("internal");
  }
}

function wireCwd(cwd: string | undefined) {
  return cwd && cwd.isWellFormed() && !cwd.includes("\0") && Array.from(cwd).length <= 4096 ? cwd : "";
}
