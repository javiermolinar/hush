import { randomUUID } from "node:crypto";
import { appendFileSync, closeSync, fstatSync, realpathSync, writeSync } from "node:fs";
import { chmod, lstat, open, unlink } from "node:fs/promises";
import { createConnection } from "node:net";
import { dirname, join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { fileURLToPath } from "node:url";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import type { CredentialStore } from "@earendil-works/pi-ai";
import { registerBunOAuthFlows as registerBundledOAuthFlows } from "@earendil-works/pi-ai/bun-oauth";
import { ModelRegistry, ModelRuntime, type SettingsManager } from "@earendil-works/pi-coding-agent";
import {
  AgentDoc, createRegistry, Harness, MemoryStorage, type AgentChange, type Conversation, type Cursor, type Extension, type HarnessSettings,
} from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { CodingTools } from "@earendil-works/pi-durable/tools";
import { createCoreServer, type NativeHost } from "../../server/src/index.js";
import { WireError } from "../../server/src/wire.js";
import { runAuthCommand } from "./auth.js";
import { credentialAdvice, credentialConfigurationFailed, hushCredentialStore } from "./credentials.js";
import { nativeModelCatalog, nativeSelectionValidator } from "./model-catalog.js";
import { Rejected } from "./pi/commands.js";
import { IntroReports } from "./intro.js";
import { ThreadDirectory } from "./thread-directory.js";
import { threadTitleGenerator } from "./thread-title-model.js";
import { Scheduling } from "./scheduling.js";
import { CheckpointService } from "./checkpoints.js";
import { readStandaloneSettings, standaloneModelDefault } from "./standalone-settings.js";
import { workspaceCodemode } from "./codemode.js";
import { ThreadWorktrees } from "./worktrees.js";
import { runWorkspaceTrustCommand, WorkspaceTrust } from "./workspace-trust.js";
import { ImageFitter } from "./images.js";
import { BackendError, parseConfig, type BackendConfig } from "./config.js";
import { fauxModel, fauxTools, makeFaux } from "./faux.js";
import { checkStoreProfile, hushDefaultModel, initializeStore, privateDirectory, privateFile } from "./profile.js";
import { findInitialModel, InMemoryCodingAgentModelsStore, InMemoryCredentialStore, resolveModelScopeFromModels } from "./pi-source.js";
import { LocalEnvironment, ProjectResources, type Warn } from "./resources.js";
import { PiHost } from "./pi/host.js";
import { ProfileDoc } from "./pi/projection.js";
import { PiSources } from "./pi/source.js";

const codingOrder = ["read", "bash", "edit", "write"];
const coding: Extension = { ...CodingTools, tools: [...(CodingTools.tools ?? [])].sort(
  (a, b) => codingOrder.indexOf(a.name) - codingOrder.indexOf(b.name)) };

function shellSettings(settings: SettingsManager): HarnessSettings {
  const compaction = {
    ...settings.getCompactionSettings(), modelOverrides: settings.getSettings().compaction?.modelOverrides,
  };
  return {
    compaction, retry: settings.getRetrySettings(),
    stream: { ...settings.getProviderRetrySettings(), transport: settings.getTransport() },
    steeringMode: settings.getSteeringMode(), followUpMode: settings.getFollowUpMode(),
  };
}

export async function loadModels(config: BackendConfig) {
  // Pi's normal OAuth loader uses variable imports, which esbuild cannot fold
  // into a single file. The standalone registration uses ordinary Node APIs.
  registerBundledOAuthFlows();
  const standalone = config.profile === "hush";
  const authPath = join(config.agentDir, "auth.json"), modelsPath = join(config.agentDir, "models.json");
  let credentials: CredentialStore | undefined;
  if (standalone) {
    await privateDirectory(config.agentDir, !config.ephemeral, config.ephemeral);
    await privateFile(modelsPath);
    if (!config.faux) credentials = (await hushCredentialStore(config.agentDir, {
      createFile: !config.ephemeral, readOnly: config.ephemeral,
    })).credentials;
  }
  if (config.faux) credentials = new InMemoryCredentialStore();
  const runtime = await ModelRuntime.create({
    authPath, modelsPath,
    ...(standalone || config.ephemeral || config.faux ? { modelsStore: new InMemoryCodingAgentModelsStore() } : {}),
    ...(credentials ? { credentials } : {}),
    allowModelNetwork: false, refreshOnCreate: !config.faux && !standalone,
  });
  if (config.faux) runtime.registerNativeProvider(makeFaux(config.ephemeral));
  return { runtime, models: new ModelRegistry(runtime) };
}

async function dataLock(dir: string) {
  // An exclusive SQLite lock on a dedicated file is kernel-owned on macOS/Linux:
  // it survives stopped processes and is released on SIGKILL without PID probing.
  const path = join(dir, "owner.sqlite");
  const file = await open(path, "wx", 0o600).catch((err: NodeJS.ErrnoException) => {
    if (err.code !== "EEXIST") throw err;
  });
  await file?.close();
  if (!await privateFile(path)) throw new BackendError("data_locked");
  for (const suffix of ["-wal", "-shm", "-journal"]) await privateFile(path + suffix);
  const db = new DatabaseSync(path);
  try {
    db.exec("PRAGMA busy_timeout = 0; BEGIN EXCLUSIVE");
    return () => db.close();
  } catch {
    db.close();
    throw new BackendError("data_locked");
  }
}

async function checkSocket(path: string, mayClean: boolean) {
  const info = await lstat(path).catch((error: NodeJS.ErrnoException) => {
    if (error.code !== "ENOENT") throw error;
  });
  if (!info) return;
  if (!mayClean || !info.isSocket() || info.uid !== process.getuid?.()) throw new BackendError("socket_unavailable");
  const stale = await new Promise<boolean>(resolve => {
    const peer = createConnection(path);
    peer.setTimeout(250);
    const done = (ok: boolean) => { peer.destroy(); resolve(ok); };
    peer.once("connect", () => done(false));
    peer.once("timeout", () => done(false));
    peer.once("error", (err: NodeJS.ErrnoException) => done(err.code === "ECONNREFUSED" || err.code === "ENOENT"));
  });
  if (!stale) throw new BackendError("socket_unavailable");
  await unlink(path);
}

interface RunningBackend {
  harness: Harness;
  root: Conversation;
  server: ReturnType<typeof createCoreServer>;
  modelRegistry: ModelRegistry;
  runtime: ModelRuntime;
  closed: Promise<void>;
  close(): Promise<void>;
}

export async function startBackend(config: BackendConfig): Promise<RunningBackend> {
  if (config.ephemeral ? config.dataDir !== undefined : config.socket !== join(config.dataDir ?? "", "hush.sock")) {
    throw new BackendError("configuration");
  }
  const standalone = config.profile === "hush";
  if (standalone && config.piEvents) throw new BackendError("configuration");
  // Keep the optional Pi loader offline. Standalone never invokes it.
  process.env.PI_OFFLINE = "1";
  if (standalone) await privateDirectory(config.agentDir, !config.ephemeral, config.ephemeral);
  const directory = dirname(config.socket);
  await privateDirectory(directory, !config.ephemeral);
  const unlock = config.dataDir ? await dataLock(config.dataDir) : () => {};
  let compatibleStore = false;
  const warn: Warn = code => {
    if (config.dataDir && compatibleStore) {
      try { appendFileSync(join(config.dataDir, "hush.log"), new Date().toISOString() + " " + code + "\n", { mode: 0o600 }); }
      catch { /* do not leak diagnostics or make logging a condition of scheduler progress */ }
    }
  };
  const registry = createRegistry();
  const startedAtMs = Date.now();
  let intro: IntroReports | undefined;
  let threadDirectory: ThreadDirectory | undefined;
  let images: ImageFitter | undefined;
  let codemode: Awaited<ReturnType<typeof workspaceCodemode>> | undefined;
  let h: Harness | undefined, resources: ProjectResources | undefined, server: ReturnType<typeof createCoreServer> | undefined;
  let models: Awaited<ReturnType<typeof loadModels>> | undefined;
  let timer: NodeJS.Timeout | undefined;
  let closing: Promise<void> | undefined;
  const stopped = Promise.withResolvers<void>();
  void stopped.promise.catch(() => {});
  const close = () => closing ??= (async () => {
    clearInterval(timer);
    warn("closing");
    const tasks = [
      () => intro?.close(),
      () => server?.close(),
      () => images?.close(),
      () => threadDirectory?.close(),
      () => h?.close(context),
      () => codemode?.sandbox.close(),
      () => resources?.close(),
      () => models?.runtime.settleCredentialWrites(),
      unlock,
    ];
    let failed = false;
    for (const stop of tasks) {
      try { await stop(); } catch { failed = true; }
    }
    if (failed) {
      const error = new BackendError("shutdown_failed");
      stopped.reject(error);
      throw error;
    }
    stopped.resolve();
  })();
  try {
    let storeIdentity = config.dataDir ? await checkStoreProfile(config.dataDir, standalone) : undefined;
    await checkSocket(config.socket, !config.ephemeral);
    if (standalone && !storeIdentity) storeIdentity = config.dataDir ? await initializeStore(config.dataDir) :
      { storeId: randomUUID(), generation: randomUUID() };
    compatibleStore = true;
    const { models: modelRegistry, runtime } = models = await loadModels(config);
    const sources = config.piEvents ? new PiSources(runtime) : undefined;
    const dirs = resources = new ProjectResources(config.cwd, config.agentDir, registry, modelRegistry, warn, config.ephemeral, sources?.tapTool, standalone);
    const prompt = dirs.extension(() => h);
    const tap = (ext: Extension): Extension => sources ? { ...ext, tools: ext.tools?.map(sources.tapTool) } : ext;
    const builtin = tap(coding), fauxExtension = tap(fauxTools);
    const localSettings = standalone && (config.dataDir || !config.faux && !config.model)
      ? await readStandaloneSettings(config.agentDir) : {};
    if (standalone && config.dataDir && localSettings.codemode === true) codemode = await workspaceCodemode();
    const base = config.ephemeral ? [prompt] : [prompt, builtin,
      ...(codemode ? [codemode.extension] : []), ...(config.faux ? [fauxExtension] : [])];
    const ordered = sources?.extensions(() => h!) ?? [];
    const trust = standalone && config.dataDir ? new WorkspaceTrust(config.agentDir) : undefined;
    const schedules = standalone && config.dataDir ? new Scheduling(() => h!, config.scheduleLateMs) : undefined;
    const checkpoints = trust && config.dataDir ? new CheckpointService(() => h!, config.dataDir, trust) : undefined;
    for (const extension of [...base, ...ordered]) registry.install(extension);
    if (schedules) registry.install(schedules.extension);
    if (checkpoints) registry.install(checkpoints.extension);
    const rootResources = await dirs.load(config.cwd);
    const selection = (project: typeof rootResources): AgentChange => {
      const requested = project.settings.getDefaultTools();
      const tools = requested?.flatMap(name => builtin.tools?.filter(t => t.name === name) ?? []);
      return {
        extensions: project.mcp ? [...base, project.mcp] : base,
        tools: config.ephemeral ? [] : tools
          ? [...tools, ...(project.mcp?.tools ?? []), ...(config.faux ? fauxExtension.tools ?? [] : [])]
          : null,
      };
    };
    const preparePi = async (cwd: string): Promise<AgentChange> => {
      const project = await dirs.load(cwd);
      return { ...selection(project), extensions: [...base, ...ordered, ...(project.mcp ? [project.mcp] : [])] };
    };
    const nativeSelection = standalone && !config.faux && !config.model
      ? standaloneModelDefault(localSettings, runtime) : undefined;
    const nativeDefault = config.faux ? fauxModel : config.model ?? nativeSelection?.model ?? hushDefaultModel;
    const defaultModel = async (settings: SettingsManager) => {
      if (standalone) return { model: nativeDefault, thinkingLevel: config.model ? "off" as const : nativeSelection?.thinkingLevel ?? "off" as const };
      const initial = await findInitialModel({
        scopedModels: resolveModelScopeFromModels(settings.getSettings().enabledModels ?? [], modelRegistry.getAvailable()).scopedModels,
        isContinuing: false,
        defaultProvider: settings.getDefaultProvider(), defaultModelId: settings.getDefaultModel(),
        defaultThinkingLevel: settings.getDefaultThinkingLevel(), modelThinkingLevels: settings.getSettings().modelThinkingLevels,
        modelRuntime: runtime,
      });
      const model = config.faux ? fauxModel : config.model ?? (initial.model &&
        { provider: initial.model.provider, modelId: initial.model.id });
      return { model, thinkingLevel: config.faux ? settings.getDefaultThinkingLevel() ?? "off" : initial.thinkingLevel };
    };
    const defaults: NonNullable<NativeHost["defaults"]> = async (cwd) => {
      const directory = dirs.directory(cwd);
      const project = await dirs.load(directory);
      return {
        ...selection(project), cwd: directory, ...await defaultModel(project.settings),
        compaction: config.faux ? { enabled: false } : shellSettings(project.settings).compaction,
      };
    };
    const requireModel: NativeHost["requireModel"] = standalone && !config.faux ? async model => {
      // A disk-backed check, not ModelRegistry's startup availability snapshot.
      // ModelRuntime separately reads and resolves auth at each provider request.
      let installed = false, diagnostic: string | undefined;
      try {
        installed = !!(model && runtime.getModel(model.provider, model.modelId));
        if (installed && model && await runtime.checkAuth(model.provider, { signal: AbortSignal.timeout(5_000) })) return;
      } catch (error) {
        // Only Hush's fixed diagnostic strings may cross the socket.
        diagnostic = credentialAdvice(error);
      }
      const error = new WireError("invalid_request");
      error.message = diagnostic ?? (!installed ? "Selected model unavailable. Restore the model on the backend host or choose another with /model."
        : model?.provider === "openai-codex" ?
          "Selected model unavailable. Run 'hush auth status' or sign in with 'hush auth login' on the backend host, or choose another with /model." +
          (config.ephemeral ? " Private runs require a current credential and do not refresh OAuth." : "")
        : "Selected model unavailable. Configure provider credentials on the backend host or choose another with /model.");
      throw error;
    } : undefined;
    const host: NativeHost = {
      defaults, requireModel,
      validateSelection: standalone ? nativeSelectionValidator(runtime, requireModel) : undefined,
      async mayMutate(conv, ctx) { return !(await h!.snapshot(ProfileDoc, conv.id, ctx))?.id; },
      async prepare({ cwd, text }) {
        const project = await dirs.load(cwd);
        try {
          const expanded = !standalone && !config.ephemeral && text?.startsWith("/skill:") ? await dirs.expandSkill(project, text, true) : text;
          return { text: dirs.expand(project, expanded), agent: selection(project) };
        } catch (error) {
          if (error instanceof Rejected) throw new WireError("invalid_request");
          throw error;
        }
      },
    };
    const store = config.dataDir ? await openNodeSqliteStorage(join(config.dataDir, "history.sqlite")) : new MemoryStorage();
    const settings = { ...shellSettings(rootResources.settings), extensions: base,
      ...(config.faux ? { retry: { enabled: false }, compaction: { enabled: false }, progress: { outputIntervalMs: 10 } } : {}),
    };
    const harness = h = await Harness.open(store, {
      models: sources?.modelAPI ?? runtime, registry, settings,
      env: async ({ cwd }) => {
        const path = dirs.directory(cwd);
        const { settings } = await dirs.load(path);
        return new LocalEnvironment(path, settings.getShellPath());
      },
      onReport: report => {
        warn("harness_report");
        // Generation/provider stop reasons have their own evidence. A failed
        // observing hook or faulted task may have lost a required boundary.
        if (sources && (report.kind === "extension_error" || report.kind === "task_faulted" || report.kind === "task_interrupted")) {
          const thread = report.task?.conversationId ?? (report.kind === "extension_error" ? report.conversationId : undefined);
          if (thread !== undefined) sources.threads.get(thread)?.trace.fail("harness source faulted");
        }
      },
    }, context);
    const pi = sources && new PiHost(h, sources, runtime, dirs, defaults, preparePi, settings.retry, config.ephemeral, config.faux);
    const root = await h.root(context, { agent: await defaults(config.cwd, context) });
    // Reinstall previously selected per-project MCP tool definitions before runnable work resumes.
    if (!standalone && !config.ephemeral) {
      const loaded = new Set([config.cwd]);
      let cursor: Cursor | undefined;
      do {
        const page = await h.commit(tx => tx.scanConversations({}, 100, cursor), context);
        for (const row of page.items) {
          const doc = await h.snapshot(AgentDoc, row.id, context);
          const cwd = dirs.directory(doc?.cwd);
          if (loaded.has(cwd) || !Array.isArray(doc?.extensions) ||
              !doc.extensions.some(name => name.startsWith("hush-mcp-"))) continue;
          loaded.add(cwd);
          await dirs.load(cwd);
        }
        cursor = page.next;
      } while (cursor);
    }
    let connected = false;
    let quiescentSince: number | undefined;
    const check = () => {
      if (closing || !server) return;
      connected ||= server.connections > 0;
      if (config.ephemeral && connected && server.connections === 0) {
        void close().catch(() => {});
        return;
      }
      const wake = harness.wake();
      if (server.connections !== 0 || wake.active || wake.at !== undefined || pi?.active) {
        quiescentSince = undefined;
        return;
      }
      quiescentSince ??= performance.now();
      const grace = config.ephemeral ? 10_000 : config.idleMs;
      if (performance.now() - quiescentSince >= grace) void close().catch(() => {});
    };
    if (!config.ephemeral) intro = new IntroReports(harness, root, dirs, runtime, startedAtMs, async project => {
      const selected = selection(project);
      const tools = Array.isArray(selected.tools) ? selected.tools : (Array.isArray(selected.extensions) ? selected.extensions : base)
        .flatMap(ext => ext.tools ?? []);
      return { ...await defaultModel(project.settings),
        tools: tools.filter(tool => !tool.deferred && (tool.callers === undefined || tool.callers.includes("model"))) };
    }, host.prepare!, host.requireModel);
    if (config.dataDir) {
      const title = config.threadTitleModel && threadTitleGenerator(runtime, config.threadTitleModel);
      threadDirectory = new ThreadDirectory(harness, config.cwd, defaults,
        new ThreadWorktrees(config.dataDir, root => dirs.trusted(root), trust), title);
      images = new ImageFitter(runtime);
    }
    const modelCatalog = standalone ? nativeModelCatalog(harness, runtime, modelRegistry, nativeDefault) : undefined;
    server = createCoreServer(harness, { host, onConnections: check, capabilities: { piEvents: pi, intro, threadDirectory, images, schedules, checkpoints,
      hushProfile: standalone ? 1 : undefined, nativeStop: standalone ? 1 : undefined,
      clientEnvironment: standalone ? 1 : undefined, modelCatalog, storeIdentity,
      runtime: { stop: () => { void close().catch(() => {}); } } } });
    await server.listen(config.socket);
    await chmod(config.socket, 0o600);
    warn("listening");
    harness.resume();
    void harness.closed.then(() => { if (!closing) void close().catch(() => {}); });
    check();
    timer = setInterval(check, Math.min(250, Math.max(15, Math.ceil(config.idleMs / 4))));
    return { harness, root, server, modelRegistry, runtime, closed: stopped.promise, close };
  } catch (err) {
    await close().catch(() => {});
    throw err;
  }
}

const errorCode = (error: unknown) => error instanceof BackendError ? error.code
  : credentialConfigurationFailed(error) ? "credential_configuration" : "startup_failed";

async function main() {
  process.umask(0o077);
  const argv = process.argv.slice(2);
  if (argv[0] === "auth") {
    process.exitCode = await runAuthCommand(argv.slice(1));
    return;
  }
  if (argv[0] === "trust") {
    process.exitCode = await runWorkspaceTrustCommand(argv.slice(1));
    return;
  }
  const config = await parseConfig(argv).catch(() => { throw new BackendError("configuration"); });
  let startupFd = config.startupFd;
  if (startupFd !== undefined) {
    try { if (!fstatSync(startupFd).isFIFO()) throw new Error("not a pipe"); }
    catch { throw new BackendError("configuration"); }
  }
  const notify = (record: { ready: true; socket: string } | { error: string }) => {
    if (startupFd === undefined) return;
    const fd = startupFd;
    startupFd = undefined;
    try { writeSync(fd, JSON.stringify(record) + "\n"); }
    catch { /* A detached backend can finish starting after its launcher exits. */ }
    finally { try { closeSync(fd); } catch { /* already closed */ } }
  };
  let app: RunningBackend | undefined;
  const terminate = () => { void app?.close().catch(() => {}); };
  const parentGone = () => {
    // Only the tool-free MemoryStorage host can be bound to this lifetime.
    // Losing the parent is final, including before the first connection and
    // if a provider or credential cleanup doesn't stop promptly.
    if (!app) process.exit(0);
    setTimeout(() => process.exit(0), 1_000).unref();
    terminate();
  };
  if (config.parentStdin) {
    process.stdin.once("end", parentGone);
    process.stdin.once("error", parentGone);
    process.stdin.resume();
  }
  try {
    app = await startBackend(config);
    process.once("SIGTERM", terminate);
    process.once("SIGINT", terminate);
    const ready = { ready: true as const, socket: config.socket };
    if (config.startupFd !== undefined) notify(ready);
    else process.stdout.write(JSON.stringify(ready) + "\n");
    await app.closed;
  } catch (error) {
    notify({ error: errorCode(error) });
    throw error;
  } finally {
    process.off("SIGTERM", terminate);
    process.off("SIGINT", terminate);
    if (config.parentStdin) {
      process.stdin.off("end", parentGone);
      process.stdin.off("error", parentGone);
      process.stdin.pause();
    }
  }
}

function invokedDirectly() {
  try { return process.argv[1] && realpathSync(process.argv[1]) === fileURLToPath(import.meta.url); }
  catch { return false; }
}

if (invokedDirectly()) {
  await main().catch(error => {
    process.stderr.write("hush-backend: " + (credentialAdvice(error) ?? errorCode(error)) + "\n");
    process.exitCode = 1;
  });
}
