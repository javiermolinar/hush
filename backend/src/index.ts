import { randomUUID } from "node:crypto";
import { appendFileSync, closeSync, fstatSync, realpathSync, writeSync } from "node:fs";
import { chmod } from "node:fs/promises";
import { flushCompileCache } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { BACKGROUND_CONTEXT as context, type Context } from "@earendil-works/chord/context";
import { createModels, InMemoryCredentialStore, type Models } from "@earendil-works/pi-ai";
import {
  createRegistry, Harness, hook, LiveDoc, MemoryStorage, ROOT_CONVERSATION_ID, ToolTask, type AgentChange, type Conversation,
  type Extension, type HarnessSettings,
} from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { CodingTools } from "@earendil-works/pi-durable/tools";
import { createCoreServer, type NativeHost } from "../../server/src/index.js";
import { WireError } from "../../server/src/wire.js";
import { runAuthCommand } from "./auth.js";
import { registerBundledOAuthFlows } from "./auth-providers.js";
import { CredentialWrites, credentialAdvice, credentialConfigurationFailed, hushCredentialStore } from "./credentials.js";
import { nativeModelCatalog, nativeSelectionValidator } from "./model-catalog.js";
import { ModelOverlayError } from "./model-overlays.js";
import { createHushModels } from "./models.js";
import { IntroReports } from "./intro.js";
import { ThreadDirectory } from "./thread-directory.js";
import { threadTitleGenerator } from "./thread-title-model.js";
import { Scheduling } from "./scheduling.js";
import { CheckpointService } from "./checkpoints.js";
import { GitDiffService } from "./git-diff.js";
import { WorkspaceFiles, workspaceFileHelper } from "./workspace-files.js";
import { Plans, fixtureExtensions, isPlanWorker, planChatExecution, recoverPlanGeneration, scopedPlanExecution, scopedRecovery } from "./plan.js";
import { checkPlanAdmission, foregroundPlan, PlanForeground } from "./plan-turn.js";
import { PlanReviews, planReviewExtension } from "./plan-review.js";
import { planAuthor } from "./plan-author.js";
import { PlanDrafts } from "./plan-draft.js";
import { planAcceptance, type PlanAcceptance } from "./plan-acceptance.js";
import { PlanFiles } from "./plan-files.js";
import { LoadedContext } from "./loaded-context.js";
import { skillUnavailable, StandaloneSkills } from "./workspace-skills.js";
import { readStandaloneSettings, StandaloneSettingsHost, standaloneModelDefault, standaloneSettingsView } from "./standalone-settings.js";
import type { workspaceCodemode } from "./codemode.js";
import { ThreadWorktrees } from "./worktrees.js";
import { runWorkspaceTrustCommand, WorkspaceTrust } from "./workspace-trust.js";
import { ImageFitter } from "./images.js";
import { WasmAssets } from "./wasm-assets.js";
import { BackendError, parseConfig, type BackendConfig } from "./config.js";
import { fauxModel, fauxTools, makeFaux } from "./faux.js";
import { checkStoreProfile, hushDefaultModel, initializeStore, privateDirectory, privateFile } from "./profile.js";
import { activeCutover, allowManagedOpen, managedRuntime, managedWriter, runStoreCommand } from "./cutover.js";
import { dataLock } from "./store-lock.js";
import { checkSupervisor, claimSupervisor } from "./supervisor.js";
import { StoreValidationError, verifyLegacyStore } from "./store-snapshot.js";
import { checkSocket } from "./store-socket.js";
import { LocalEnvironment, StandaloneResources, type Warn } from "./resources.js";

const codingOrder = ["read", "bash", "edit", "write"];
const workspaceWriters = new Set(["write", "edit", "bash"]);
const coding: Extension = { ...CodingTools, tools: [...(CodingTools.tools ?? [])]
  .sort((a, b) => codingOrder.indexOf(a.name) - codingOrder.indexOf(b.name))
  .map(tool => tool.name === "edit" ? { ...tool, description: "Replace exact text in one file. Keep replacements small." } : tool),
};

// Freeze the standalone profile's existing policy. The Durable agent loop
// still owns retry, context trimming and compaction, not the model adapter.
const hushSettings: HarnessSettings = {
  compaction: { enabled: true, reserveTokens: 16384, keepRecentTokens: 20000 },
  retry: { enabled: true, maxRetries: 3, baseDelayMs: 2000, maxAgentDelayMs: 60000 },
  stream: { maxRetryDelayMs: 60000, transport: "auto" },
  steeringMode: "one-at-a-time", followUpMode: "one-at-a-time",
};

export async function loadModels(config: BackendConfig) {
  await registerBundledOAuthFlows();
  await privateDirectory(config.agentDir, !config.ephemeral, config.ephemeral);
  const writes = new CredentialWrites();
  const credentials = config.faux ? new InMemoryCredentialStore() : (await hushCredentialStore(config.agentDir, {
    createFile: !config.ephemeral, readOnly: config.ephemeral, writes,
  })).credentials;
  const runtime = await createHushModels(credentials, join(config.agentDir, "models.json"));
  if (config.faux) runtime.setProvider(makeFaux(config.ephemeral));
  return { runtime, writes };
}

export interface RunningBackend {
  harness: Harness;
  root: Conversation;
  server: ReturnType<typeof createCoreServer>;
  runtime: Models;
  readOnlyReason?: string;
  closed: Promise<void>;
  close(): Promise<void>;
}

export async function startBackend(config: BackendConfig): Promise<RunningBackend> {
  if (config.ephemeral ? config.dataDir !== undefined : config.socket !== join(config.dataDir ?? "", "hush.sock")) {
    throw new BackendError("configuration");
  }
  await privateDirectory(config.agentDir, !config.ephemeral, config.ephemeral);
  const directory = dirname(config.socket);
  let unlockRuntime: (() => void) | undefined, unlockManaged: (() => void) | undefined;
  let compatibleStore = false;
  const log = (message: string) => {
    if (config.dataDir && compatibleStore) {
      try { appendFileSync(join(config.dataDir, "hush.log"), new Date().toISOString() + " " + message + "\n", { mode: 0o600 }); }
      catch { /* logging is not a condition of scheduler progress */ }
    }
  };
  const warn: Warn = log;
  const registry = createRegistry();
  const startedAtMs = Date.now();
  let intro: IntroReports | undefined;
  let threadDirectory: ThreadDirectory | undefined;
  let images: ImageFitter | undefined;
  let wasm: WasmAssets | undefined;
  let workspaceFiles: WorkspaceFiles | undefined;
  let gitDiff: GitDiffService | undefined;
  let stopGrantWatch: (() => void) | undefined;
  let acceptance: PlanAcceptance | undefined;
  let codemode: Awaited<ReturnType<typeof workspaceCodemode>> | undefined;
  let h: Harness | undefined, server: ReturnType<typeof createCoreServer> | undefined;
  let hushModels: Awaited<ReturnType<typeof loadModels>> | undefined;
  let timer: NodeJS.Timeout | undefined;
  let closing: Promise<void> | undefined;
  const stopped = Promise.withResolvers<void>();
  void stopped.promise.catch(() => {});
  const close = () => closing ??= (async () => {
    clearInterval(timer);
    warn("closing");
    const tasks = [
      () => intro?.close(),
      // Publish this boot's cache before the socket disappears so a CLI
      // restart can use it immediately. No-op when Node caching is disabled.
      () => flushCompileCache(),
      () => server?.close(),
      () => images?.close(),
      () => stopGrantWatch?.(),
      () => gitDiff?.close(),
      () => workspaceFiles?.close(),
      () => threadDirectory?.close(),
      () => h?.close(context),
      () => codemode?.sandbox.close(),
      () => acceptance?.close(),
      () => wasm?.close(),
      () => hushModels?.writes.settle(),
      () => unlockRuntime?.(),
      () => unlockManaged?.(),
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
    const managed = config.dataDir ? await managedRuntime(config.agentDir, config.dataDir) : undefined;
    if (managed) unlockManaged = await managedWriter(managed);
    await checkSupervisor(managed?.agent, config.supervisor);
    const gate = managed && await activeCutover(managed);
    await privateDirectory(directory, !config.ephemeral && !gate);
    if (config.dataDir) unlockRuntime = await dataLock(config.dataDir);
    let localSettings = (config.dataDir || !config.faux && !config.model)
      ? await readStandaloneSettings(config.agentDir) : {};
    const allowLegacy = managed && !gate && localSettings.allowLegacyStore === true;
    const manifest = config.dataDir ? await checkStoreProfile(config.dataDir, allowLegacy ? "either" : undefined) : undefined;
    await allowManagedOpen(managed, gate, manifest, fileURLToPath(import.meta.url));
    if (config.dataDir) wasm = await WasmAssets.open(import.meta.url);
    if (manifest?.format !== 1) await checkSocket(config.socket, config.ephemeral ? "refuse" : "clean");
    compatibleStore = manifest?.format !== 1;
    let modelIssue: ModelOverlayError | undefined;
    const runtime = await loadModels(config).then(models => (hushModels = models).runtime).catch(error => {
      if (!(error instanceof ModelOverlayError)) throw error;
      if (!manifest) {
        log(error.message); // bounded and redacted by the overlay loader
        throw error;
      }
      modelIssue = error;
      // A broken proxy override must never send work to a built-in endpoint,
      // including a submission or tool that Durable would otherwise recover.
      return createModels({ credentials: new InMemoryCredentialStore() });
    });
    const storeIdentity = manifest ? { storeId: manifest.storeId, generation: manifest.generation } :
      config.dataDir ? await initializeStore(config.dataDir) : { storeId: randomUUID(), generation: randomUUID() };
    const readOnlyReason = modelIssue?.message;
    const refuseWork = readOnlyReason ? () => {
      const error = new WireError("invalid_request");
      error.message = readOnlyReason;
      throw error;
    } : undefined;
    const trust = !config.ephemeral ? new WorkspaceTrust(config.agentDir) : undefined;
    const workspaceSkills = trust && config.dataDir && ["darwin", "linux"].includes(process.platform)
      ? new StandaloneSkills(() => h!, config.agentDir, trust) : undefined;
    const resources = new StandaloneResources(config.cwd, config.agentDir, workspaceSkills);
    const authorizeWork = trust && ((cwd: string | undefined, ctx: Context) =>
      trust.require(resources.directory(cwd), ctx.abortSignal));
    const prompt = resources.extension(() => h, !!trust);
    // Use the existing prompt extension name so persisted threads get the
    // hook too. Observe every write-capable result, including failed tools
    // that may have changed files before reporting the failure.
    const withWorkspaceChanges: Extension = trust ? { ...prompt, hooks: [...prompt.hooks ?? [], hook(ToolTask, {
      afterTool: call => {
        if (!workspaceWriters.has(call.name)) return;
        // Tool results don't identify all destinations. Shell commands and
        // writes through aliases can change a different checkout.
        gitDiff?.invalidate();
        workspaceFiles?.invalidateSearch();
      },
    })] } : prompt;
    if (config.dataDir && localSettings.codemode === true) {
      codemode = (await import("./codemode.js")).workspaceCodemode(wasm);
    }
    const planEnabled = !!config.dataDir && !config.ephemeral && manifest?.format !== 1;
    const fauxPlan = planEnabled && config.faux;
    const browser = trust && config.dataDir ? await workspaceFileHelper() : undefined;
    const planFiles = planEnabled && !config.faux && !modelIssue && browser && trust && config.dataDir
      ? await PlanFiles.open(browser, trust, [config.agentDir, config.dataDir], () => {
        gitDiff?.invalidate();
        workspaceFiles?.invalidateSearch();
      }) : undefined;
    const author = planEnabled ? planAuthor(() => h!, cwd => resources.directory(cwd), fauxPlan, planFiles) : undefined;
    const base = config.ephemeral ? [prompt] : [withWorkspaceChanges, coding,
      ...(codemode ? [codemode.extension] : []), ...(config.faux ? [fauxTools] : []), ...(author ? [author] : [])];
    const planAdmission = planEnabled && !config.faux ? checkPlanAdmission : undefined;
    const schedules = config.dataDir ? new Scheduling(() => h!, config.scheduleLateMs, authorizeWork,
      planAdmission && (async (tx, id) => !!await foregroundPlan(tx, id))) : undefined;
    const checkpoints = trust && config.dataDir ? new CheckpointService(() => h!, config.dataDir, trust) : undefined;
    for (const extension of base) registry.install(extension);
    if (fauxPlan) for (const extension of fixtureExtensions) registry.install(extension);
    if (planEnabled) registry.install(planReviewExtension);
    if (schedules) registry.install(schedules.extension);
    if (checkpoints) registry.install(checkpoints.extension);
    if (manifest?.format === 1) {
      if (!managed || !allowLegacy) throw new BackendError("store_profile");
      try { await verifyLegacyStore(managed.runtime, managed.control, registry.snapshot()); }
      catch { throw new BackendError("store_profile"); }
      await checkSocket(config.socket, "clean");
      compatibleStore = true;
    }
    if (readOnlyReason) log(readOnlyReason);
    const selection: AgentChange = { extensions: base, tools: config.ephemeral ? [] : null };
    const useStoredDefaults = !modelIssue && !config.faux && !config.model;
    const nativeSelection = () => useStoredDefaults ? standaloneModelDefault(localSettings, runtime) : undefined;
    const nativeDefault = () => config.faux ? fauxModel : config.model ?? nativeSelection()?.model ?? hushDefaultModel;
    if (useStoredDefaults) standaloneSettingsView(localSettings, runtime);
    const defaultModel = () => ({ model: nativeDefault(), thinkingLevel: nativeSelection()?.thinkingLevel ?? "off" as const });
    const defaults: NonNullable<NativeHost["defaults"]> = async cwd => ({
      ...selection, cwd: resources.directory(cwd), ...defaultModel(),
      compaction: config.faux ? { enabled: false } : hushSettings.compaction,
    });
    const requireModel: NativeHost["requireModel"] = refuseWork ? async () => refuseWork() : !config.faux ? async model => {
      // Pi AI reads and resolves auth at each provider request.
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
    const scopedPlans = planFiles && scopedPlanExecution(planFiles, acceptance = planAcceptance(wasm), async (agent, ctx) => {
      const model = agent.model;
      await requireModel?.(model);
      ctx.abortSignal?.throwIfAborted();
      const available = model && runtime.getModel(model.provider, model.modelId);
      if (!model || !available) throw new WireError("invalid_request");
      return { model: { provider: model.provider, modelId: model.modelId }, thinking: agent.thinkingLevel ?? "off" };
    });
    if (scopedPlans) registry.install(scopedPlans.extension);
    else if (planEnabled) registry.install(scopedRecovery);
    const host: NativeHost = {
      defaults, requireModel, checkAdmission: planAdmission,
      validateSelection: nativeSelectionValidator(runtime, requireModel),
      async checkMutation(conv, ctx) {
        if (planEnabled && await isPlanWorker(h!, conv.id, ctx)) throw new WireError("not_found");
      },
      async prepare({ threadId, cwd, text, skillToken }, ctx) {
        // Native text, images, Continue and Retry share this admission path.
        refuseWork?.();
        await authorizeWork?.(cwd, ctx);
        if (!config.ephemeral && text?.startsWith("/skill:")) {
          if (!workspaceSkills || !threadId) throw skillUnavailable();
          const skill = await workspaceSkills.expand(threadId, cwd, text, skillToken, ctx.abortSignal);
          return { ...skill, agent: selection };
        }
        if (skillToken) throw skillUnavailable();
        return { text, agent: selection };
      },
    };
    const chatPlans = planEnabled && !config.faux && !modelIssue && authorizeWork
      ? planChatExecution(() => h!, authorizeWork, requireModel, selection) : undefined;
    if (chatPlans) registry.install(chatPlans.extension);
    const store = config.dataDir ? await openNodeSqliteStorage(join(config.dataDir, "history.sqlite")) : new MemoryStorage();
    const settings = { ...hushSettings, extensions: base,
      ...(config.faux ? { retry: { enabled: false }, compaction: { enabled: false }, progress: { outputIntervalMs: 10 } } : {}),
    };
    const harness = h = await Harness.open(store, {
      models: runtime, registry, settings, readOnly: !!modelIssue,
      env: async ({ cwd, conversationId, read }, ctx) => {
        // Scoped tools use the file helper, not the normal work environment.
        // A terminal reservation left by a fault must not restrict later chat.
        const run = planAdmission && (await read.snapshot(PlanForeground, conversationId, ctx))?.run;
        if (run) {
          const task = await h!.getTask(run.id, ctx);
          const live = run.turn && (await read.snapshot(LiveDoc, conversationId, ctx))?.run;
          if (task && task.state.status !== "terminal" || run.turn && live && live.first === run.turn.entry) return undefined;
        }
        const path = resources.directory(cwd);
        // Durable builds the env for every tool attempt, also after a wake or
        // recovery that skips beforeTool. Do not hand a tool an unchecked cwd.
        const authorized = authorizeWork ? await authorizeWork(cwd, ctx) : path;
        return new LocalEnvironment(authorized, path);
      },
      onReport: () => warn("harness_report"),
    }, context);
    if (planAdmission && !modelIssue) await recoverPlanGeneration(harness, context);
    const root = modelIssue ? await h.conversation(ROOT_CONVERSATION_ID, context)
      : await h.root(context, { agent: await defaults(config.cwd, context) });
    if (!root) throw modelIssue; // A store without a root has no history to serve.
    let connected = false;
    let quiescentSince: number | undefined;
    const check = () => {
      if (closing || !server) return;
      connected ||= server.connections > 0;
      if (config.ephemeral && connected && server.connections === 0) {
        void close().catch(() => {});
        return;
      }
      if (config.supervisor) return; // Only its login supervisor owns this lifetime.
      const wake = harness.wake();
      if (server.connections !== 0 || wake.active || wake.at !== undefined) {
        quiescentSince = undefined;
        return;
      }
      quiescentSince ??= performance.now();
      const grace = config.ephemeral ? 10_000 : config.idleMs;
      if (performance.now() - quiescentSince >= grace) void close().catch(() => {});
    };
    if (!config.ephemeral) {
      const tools = base.flatMap(ext => ext.tools ?? [])
        .filter(tool => !tool.deferred && (tool.callers === undefined || tool.callers.includes("model")));
      intro = new IntroReports(harness, root, resources, runtime, startedAtMs, () => ({ ...defaultModel(), tools }),
        host.prepare!, host.requireModel, planAdmission);
    }
    if (config.dataDir && trust) {
      const title = !modelIssue && config.threadTitleModel ? threadTitleGenerator(runtime, config.threadTitleModel) : undefined;
      threadDirectory = new ThreadDirectory(harness, config.cwd, defaults, new ThreadWorktrees(config.dataDir, trust), title, readOnlyReason);
      images = new ImageFitter(runtime, wasm);
    }
    const modelCatalog = modelIssue ? undefined : nativeModelCatalog(harness, runtime, nativeDefault);
    gitDiff = trust && checkpoints ? new GitDiffService(harness, trust, checkpoints) : undefined;
    workspaceFiles = trust && browser ? new WorkspaceFiles(harness, trust, browser) : undefined;
    if (gitDiff || workspaceFiles) stopGrantWatch = trust?.watchGrants(() => {
      gitDiff?.invalidate();
      workspaceFiles?.grantsChanged();
    });
    const loadedContext = trust && config.dataDir ? new LoadedContext(harness, resources, trust) : undefined;
    const settingsHost = config.dataDir && useStoredDefaults
      ? new StandaloneSettingsHost(config.agentDir, runtime, localSettings, settings => { localSettings = settings; }) : undefined;
    const plans = planEnabled && !modelIssue ? new Plans(harness, store, cwd => resources.directory(cwd), fauxPlan, scopedPlans, chatPlans) : undefined;
    const planDrafts = plans && authorizeWork ? new PlanDrafts(harness, cwd => resources.directory(cwd), fauxPlan, authorizeWork) : undefined;
    const planReviews = plans && !fauxPlan ? new PlanReviews(harness, plans, async (cwd, model, ctx) => {
      await authorizeWork?.(cwd, ctx);
      await requireModel?.(model);
    }) : undefined;
    server = createCoreServer(harness, { host, onConnections: check, readOnlyReason, capabilities: { intro, threadDirectory, images,
      schedules: modelIssue ? undefined : schedules, checkpoints: modelIssue ? undefined : checkpoints,
      plan: plans, planChat: chatPlans && plans, planDraft: planDrafts, planReview: planReviews,
      gitDiff, workspaceFiles, workspaceSkills, loadedContext, hushProfile: 1, nativeStop: modelIssue ? undefined : 1,
      workspaceWorkTrust: trust ? 1 : undefined, settings: settingsHost, clientEnvironment: 1, modelCatalog, storeIdentity,
      runtime: { stop: () => { void close().catch(() => {}); } } } });
    await server.listen(config.socket);
    await chmod(config.socket, 0o600);
    warn("listening");
    if (!modelIssue) harness.resume();
    if (config.supervisor && !config.faux && !modelIssue) {
      const { provider } = nativeDefault();
      // A once-per-boot diagnostic must not stop the scheduler. Never print provider data.
      void runtime.checkAuth(provider, { signal: AbortSignal.timeout(5_000) }).then(auth => {
        if (!auth) process.stderr.write("Hush service: provider sign-in needed. Run 'hush auth status' or choose another model.\n");
      }, error => process.stderr.write("Hush service: " + (credentialAdvice(error) ?? "Provider credentials unavailable; run 'hush auth status'.") + "\n"));
    }
    void harness.closed.then(() => { if (!closing) void close().catch(() => {}); });
    check();
    if (!config.supervisor) timer = setInterval(check, Math.min(250, Math.max(15, Math.ceil(config.idleMs / 4))));
    return { harness, root, server, runtime, readOnlyReason, closed: stopped.promise, close };
  } catch (err) {
    await close().catch(() => {});
    throw err;
  }
}

const errorCode = (error: unknown) => error instanceof ModelOverlayError ? "models_configuration" : error instanceof BackendError ? error.code
  : credentialConfigurationFailed(error) ? "credential_configuration" : "startup_failed";

async function main(entryUrl: string) {
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
  if (argv[0] === "--hush-internal-service-claim") {
    if (argv.length !== 2) throw new BackendError("configuration");
    await claimSupervisor(argv[1]);
    return;
  }
  if (argv[0] === "store") {
    try { await runStoreCommand(argv.slice(1), fileURLToPath(entryUrl)); }
    catch (error) {
      const reason = error instanceof StoreValidationError ? error.message :
        error instanceof BackendError && (error.code === "data_locked" || error.code === "socket_unavailable") ?
          "The backend is not quiescent. Stop it and retry only after the writer exits." :
          "Store operation refused. Inspect the store; preserve its files and recover forward if the state is unknown.";
      process.stderr.write(`hush store: ${reason}\n`);
      process.exitCode = 1;
    }
    return;
  }
  const config = await parseConfig(argv).catch((error: unknown) => {
    throw error instanceof BackendError ? error : new BackendError("configuration");
  });
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
    // Keep the handlers installed while closing. The legacy file-lock
    // library also listens for signals and re-raises an unhandled signal.
    process.on("SIGTERM", terminate);
    process.on("SIGINT", terminate);
    const ready = { ready: true as const, socket: config.socket };
    if (config.startupFd !== undefined) notify(ready);
    else {
      if (app.readOnlyReason) process.stderr.write(`hush-backend: ${app.readOnlyReason} History is read-only.\n`);
      process.stdout.write(JSON.stringify(ready) + "\n");
    }
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

function invokedDirectly(url: string) {
  try { return process.argv[1] && realpathSync(process.argv[1]) === fileURLToPath(url); }
  catch { return false; }
}

export async function runIfMain(url: string) {
  if (!invokedDirectly(url)) return;
  await main(url).catch(error => {
    const code = errorCode(error);
    const supervised = process.argv.includes("--supervisor");
    const serviceClaim = process.argv[2] === "--hush-internal-service-claim";
    const advice = code === "service_owned" ? "The login supervisor owns this backend. Check 'hush service status' or 'hush service resume'."
      : code === "wasm_pack" ? "Backend .wasm-pack is missing or invalid. Rebuild or reinstall the paired backend files."
      : supervised && code === "store_cutover" ? "The store is fenced. Run 'hush store inspect' before restarting the service."
      : (supervised || serviceClaim) && code === "data_locked" ? "Another backend or store operation owns this profile. Wait until it stops."
      : error instanceof ModelOverlayError ? error.message : credentialAdvice(error);
    process.stderr.write("hush-backend: " + (advice ?? code) + "\n");
    // The launcher maps known startup/hold failures to a non-restarting exit.
    process.exitCode = supervised &&
      ["service_owned", "data_locked", "store_profile", "store_cutover", "configuration", "socket_directory", "socket_unavailable",
        "credential_configuration", "models_configuration", "wasm_pack"].includes(code) ? 78 : 1;
  });
}
