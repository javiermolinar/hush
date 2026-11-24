import { randomUUID } from "node:crypto";
import { join } from "node:path";
import type { CredentialStore } from "@earendil-works/pi-ai";
import { ModelRuntime } from "@earendil-works/pi-coding-agent";
import { object } from "../../server/src/wire.js";
import { authProvider, authProviders, defaultAuthProvider, registerBundledOAuthFlows, type AuthProvider } from "./auth-providers.js";
import { hushDirectory } from "./config.js";
import { acquireCredentialLock, credentialAdvice, hushCredentialStore } from "./credentials.js";
import { InMemoryCodingAgentModelsStore, InMemoryCredentialStore } from "./pi-source.js";
import { privateDirectory, privateFile, uuid } from "./profile.js";
import { readStoreJSON, replaceStoreJSON } from "./store-files.js";

type Interaction = Parameters<ModelRuntime["login"]>[2];
type AuthEvent = Parameters<Interaction["notify"]>[0];
type AuthPrompt = Parameters<Interaction["prompt"]>[0];

const screen = (text: string) => process.stderr.write(text + "\n");
const loginTTY = () => process.stdin.isTTY && process.stdout.isTTY && process.stderr.isTTY;

function providerURL(id: string, event: Extract<AuthEvent, { type: "auth_url" | "device_code" }>) {
  const value = event.type === "auth_url" ? event.url : event.verificationUri;
  if (!/^[\x21-\x7e]{1,8192}$/.test(value)) throw new Error("Unexpected login URL");
  const url = new URL(value);
  if (url.protocol !== "https:" || url.username || url.password || url.port) throw new Error("Unexpected login URL");
  const at = (host: string, path: string) => url.hostname === host && url.pathname === path && !url.hash;
  const allowed = event.type === "auth_url" ? (
    id === "openai-codex" ? at("auth.openai.com", "/oauth/authorize") :
    id === "openai" ? at("auth.openai.com", "/api/accounts/authorize") :
    id === "anthropic" ? at("claude.ai", "/oauth/authorize") :
    id === "openrouter" ? at("openrouter.ai", "/auth") :
    // Radius discovers the authorization endpoint from its built-in HTTPS
    // gateway. It may use a different identity provider; never load overlays.
    id === "radius"
  ) : (
    id === "openai-codex" ? at("auth.openai.com", "/codex/device") :
    id === "github-copilot" ? at("github.com", "/login/device") :
    id === "kimi-coding" ? ["auth.kimi.com", "kimi.com", "www.kimi.com"].includes(url.hostname) :
    id === "meta" ? ["auth.meta.com", "meta.com", "www.meta.com", "meta.ai", "www.meta.ai"].includes(url.hostname) :
    id === "xai" ? ["auth.x.ai", "accounts.x.ai", "x.ai"].includes(url.hostname) :
    id === "radius"
  );
  if (!allowed) throw new Error("Unexpected login URL");
  return url.href;
}

// Only Hush text and the provider's actual browser/device instructions go to
// this terminal. Provider notices, descriptions, and errors are not printed.
function notify(provider: AuthProvider, event: AuthEvent) {
  if (event.type !== "auth_url" && event.type !== "device_code") return;
  const url = providerURL(provider.id, event);
  if (event.type === "device_code" && !/^[A-Za-z0-9-]{1,64}$/.test(event.userCode)) {
    throw new Error("Unexpected device code");
  }
  screen(`Open this URL in a browser to sign in to ${provider.label}:`);
  screen(url);
  if (event.type === "device_code") {
    screen("Enter this one-time code in the browser: " + event.userCode);
    screen("Waiting for browser approval. Press Ctrl+C to cancel.");
  } else if (provider.id === "radius") {
    screen("Complete sign-in in the browser on this host. Press Ctrl+C to cancel.");
  }
}

const loginMethods = new Map([
  ["openai-codex", { alternate: "device_code", label: "Device code" }],
  ["anthropic", { alternate: "copy_code", label: "Copy code" }],
  ["radius", { alternate: "device-code", label: "Device code" }],
]);

async function openAIDeviceId(dir: string, signal: AbortSignal) {
  // Only ChatGPT needs an installation UUID. Serialize first use so two
  // terminals cannot register different IDs for the same Hush profile.
  const unlock = await acquireCredentialLock(dir, signal);
  try {
    const path = join(dir, "openai-device.json");
    const saved = await readStoreJSON(path);
    if (saved !== undefined) {
      if (!object(saved) || !uuid(saved.deviceId)) throw new Error("Invalid OpenAI device ID");
      return saved.deviceId;
    }
    const deviceId = randomUUID();
    await replaceStoreJSON(path, { deviceId });
    return deviceId;
  } finally { unlock(); }
}

async function login(runtime: ModelRuntime, provider: AuthProvider, abort: AbortController, deviceId?: string) {
  const input = process.stdin;
  const { signal } = abort;
  const terminal = new AbortController();
  const lifetime = AbortSignal.any([signal, terminal.signal]);
  signal.throwIfAborted();
  const raw = input.isRaw;
  let accept: ((byte: number) => void) | undefined;
  const onData = (chunk: Buffer) => {
    // Stay in no-echo mode between prompts too: pasted callback URLs must
    // not echo even if the callback wins the race or the paste arrives early.
    if (chunk.includes(3) || chunk.includes(4)) { abort.abort(); return; }
    for (const byte of chunk) accept?.(byte);
  };
  const end = () => abort.abort();
  function line(message: string, stepSignal: AbortSignal) {
    stepSignal.throwIfAborted();
    if (accept) throw new Error("Overlapping login prompts");
    return new Promise<string>((resolve, reject) => {
      const buffer = Buffer.alloc(8192);
      let length = 0;
      const finish = (value?: string) => {
        accept = undefined;
        buffer.fill(0);
        stepSignal.removeEventListener("abort", cancel);
        // The prompt is line-oriented, even though the stdin tty is raw.
        if (value !== undefined) resolve(value);
        else reject(new Error("Login input stopped"));
      };
      const cancel = () => finish();
      stepSignal.addEventListener("abort", cancel, { once: true });
      accept = byte => {
        if (byte === 13 || byte === 10) finish(buffer.toString("ascii", 0, length).trim());
        else if (byte === 8 || byte === 127) { if (length > 0) buffer[--length] = 0; }
        else if (byte >= 32 && byte <= 126) {
          if (length === buffer.length) { abort.abort(); return; }
          buffer[length++] = byte;
        } else if (byte === 27) abort.abort(); // Escape cancels; no terminal control sequences are accepted.
      };
      screen(message);
    });
  }
  const prompt = async (step: AuthPrompt) => {
    const active = step.signal ? AbortSignal.any([step.signal, lifetime]) : lifetime;
    switch (step.type) {
      case "select": {
        const method = loginMethods.get(provider.id);
        const supported = new Set(step.options.map(option => option.id));
        if (!method || !supported.has("browser") || !supported.has(method.alternate)) throw new Error("Unexpected login methods");
        while (true) {
          const choice = await line(`${provider.label} sign-in: [1] Browser (default), [2] ${method.label}. Press Enter to select; Ctrl+C to cancel.`, active);
          if (choice === "" || choice === "1") return "browser";
          if (choice === "2") return method.alternate;
          screen("Enter 1 or 2.");
        }
      }
      case "manual_code": return line(provider.id === "openai"
        ? "If the callback cannot connect, paste the full redirect URL here (hidden), then press Enter."
        : "If the browser shows a code or the callback cannot connect, paste the code or redirect URL here (hidden), then press Enter.", active);
      case "text":
        // Use GitHub.com, not a user-supplied enterprise endpoint.
        if (provider.id === "github-copilot") return "";
        throw new Error("Unexpected login prompt");
      default: throw new Error("Unexpected login prompt");
    }
  };
  input.setRawMode(true);
  try {
    input.on("data", onData).once("end", end).once("error", end);
    await runtime.login(provider.id, "oauth", {
      signal: lifetime, prompt,
      notify: event => {
        if (lifetime.aborted) return;
        // Abort through Pi's cleanup path rather than throwing out of a
        // notification: a flow may already have opened a callback server.
        try { notify(provider, event); }
        catch (error) { terminal.abort(error); }
      },
    }, { agentName: "hush", getDeviceId: deviceId ? () => deviceId : undefined });
    lifetime.throwIfAborted();
  } finally {
    // Also cancel a pending manual prompt if the provider flow rejected.
    terminal.abort();
    input.off("data", onData).off("end", end).off("error", end);
    input.pause();
    input.setRawMode(raw);
  }
}

export async function runAuthCommand(args: string[]) {
  const [action, id = defaultAuthProvider] = args;
  if (args.length > 2 || action !== "login" && action !== "status" && action !== "logout") {
    screen("Usage: hush auth login|status|logout [PROVIDER]");
    return 1;
  }
  const provider = authProvider(id);
  if (!provider) {
    screen("Supported auth providers: " + authProviders.map(p => p.id).join(", "));
    return 1;
  }
  if (action === "login" && !loginTTY()) {
    screen("hush auth login requires an interactive terminal on the backend host.");
    return 1;
  }
  const abort = new AbortController();
  const stop = () => abort.abort();
  const signals = ["SIGINT", "SIGTERM", "SIGHUP"] as const;
  if (action === "login") for (const signal of signals) process.on(signal, stop);
  let runtime: ModelRuntime | undefined;
  const statusCommand = `hush auth status${id === defaultAuthProvider ? "" : " " + id}`;
  try {
    // No Pi defaults, and never allow an inherited callback binding override.
    process.env.PI_OFFLINE = "1";
    delete process.env.PI_OAUTH_CALLBACK_HOST;
    delete process.env.KIMI_CODE_OAUTH_HOST;
    delete process.env.KIMI_OAUTH_HOST;
    await registerBundledOAuthFlows();
    const dir = hushDirectory();
    await privateDirectory(dir, action === "login", action !== "login");
    const modelsPath = join(dir, "models.json");
    const { credentials, keyring } = await hushCredentialStore(dir, { createFile: action === "login" });
    const create = (store: CredentialStore, overlays: boolean) => ModelRuntime.create({
      credentials: store, modelsPath: overlays ? modelsPath : null,
      modelsStore: new InMemoryCodingAgentModelsStore(),
      allowModelNetwork: false, refreshOnCreate: false,
    });
    if (action === "login") {
      const deviceId = id === "openai" ? await openAIDeviceId(dir, abort.signal) : undefined;
      const signIn = async (store: CredentialStore) => {
        // An overlay must not change the provider login offer. Test keyring
        // access before beginning a browser flow that may spend a code.
        runtime = await create(store, false);
        if (keyring) await store.read(id, { signal: abort.signal });
        try { await login(runtime, provider, abort, deviceId); }
        finally { await runtime.settleCredentialWrites(); }
      };
      if (keyring) await keyring.exclusive(signIn, abort.signal);
      else await signIn(credentials);
      process.stdout.write(`${id}: signed in with OAuth\n`);
      return 0;
    }
    if (action === "logout") {
      runtime = await create(credentials, false);
      await runtime.logout(id);
    }
    await privateFile(modelsPath);
    runtime = await create(credentials, true);
    const opts = { signal: AbortSignal.timeout(5_000) };
    const managed = (await credentials.read(id, opts))?.type;
    const oauth = managed === "oauth" && (await runtime.checkAuth(id, opts))?.type === "oauth";
    // A managed credential can hide a configured alternative. Check that
    // same Hush config with an empty in-memory store, never by deleting data.
    const withoutManaged = managed ? await create(new InMemoryCredentialStore(), true) : runtime;
    const alternative = withoutManaged.getProviderAuthStatus(id).configured ||
      (await withoutManaged.checkAuth(id, opts))?.type === "api_key";
    const status = managed === "oauth" ? (oauth ? "OAuth configured" : "stored OAuth unavailable")
      : managed === "api_key" ? "stored API key (not OAuth)" : "no Hush-managed credential";
    process.stdout.write(`${id}: ${status}\n`);
    if (alternative) process.stdout.write(`${id}: a configured alternative remains (not Hush OAuth)\n`);
    return 0;
  } catch (error) {
    if (abort.signal.aborted) {
      screen(`${provider.label} sign-in cancelled. Run '${statusCommand}' to check the stored credential.`);
      return 130;
    }
    screen(credentialAdvice(error) ?? `Could not complete ${provider.label} auth. Check the private Hush profile and run '${statusCommand}' to inspect it.`);
    return 1;
  } finally {
    await runtime?.settleCredentialWrites().catch(() => {});
    if (action === "login") for (const signal of signals) process.off(signal, stop);
  }
}
