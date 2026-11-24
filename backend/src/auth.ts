import { join } from "node:path";
import type { CredentialStore } from "@earendil-works/pi-ai";
import { registerBunOAuthFlows } from "@earendil-works/pi-ai/bun-oauth";
import { ModelRuntime } from "@earendil-works/pi-coding-agent";
import { hushDirectory } from "./config.js";
import { credentialAdvice, hushCredentialStore } from "./credentials.js";
import { InMemoryCodingAgentModelsStore, InMemoryCredentialStore } from "./pi-source.js";
import { privateDirectory, privateFile } from "./profile.js";

type Interaction = Parameters<ModelRuntime["login"]>[2];
type AuthEvent = Parameters<Interaction["notify"]>[0];
type AuthPrompt = Parameters<Interaction["prompt"]>[0];

const provider = "openai-codex";
const screen = (text: string) => process.stderr.write(text + "\n");
const loginTTY = () => process.stdin.isTTY && process.stdout.isTTY && process.stderr.isTTY;

function providerURL(value: string, path: string) {
  const url = new URL(value);
  if (url.origin !== "https://auth.openai.com" || url.pathname !== path || url.username || url.password || url.hash) {
    throw new Error("Unexpected login URL");
  }
  return url.href;
}

// Only Hush text and the provider's actual browser/device instructions go to
// this terminal. Provider notices, descriptions, and errors are not printed.
function notify(event: AuthEvent) {
  if (event.type === "auth_url") {
    screen("Open this URL in a browser to sign in to OpenAI Codex:");
    screen(providerURL(event.url, "/oauth/authorize"));
  } else if (event.type === "device_code") {
    if (!/^[A-Za-z0-9-]{1,64}$/.test(event.userCode)) throw new Error("Unexpected device code");
    screen("Open this URL in a browser to sign in to OpenAI Codex:");
    screen(providerURL(event.verificationUri, "/codex/device"));
    screen("Enter this one-time code in the browser: " + event.userCode);
    screen("Waiting for browser approval. Press Ctrl+C to cancel.");
  }
}

async function login(runtime: ModelRuntime, abort: AbortController) {
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
        const supported = new Set(step.options.map(option => option.id));
        if (!supported.has("browser") || !supported.has("device_code")) throw new Error("Unexpected login methods");
        while (true) {
          const choice = await line("OpenAI Codex sign-in: [1] Browser (default), [2] Device code. Press Enter to select; Ctrl+C to cancel.", active);
          if (choice === "" || choice === "1") return "browser";
          if (choice === "2") return "device_code";
          screen("Enter 1 or 2.");
        }
      }
      case "manual_code": return line("Waiting for the browser callback. If it cannot connect, paste the redirect URL or code here (hidden), then press Enter.", active);
      case "secret": return line("Enter the requested secret (hidden), then press Enter.", active);
      default: throw new Error("Unexpected login prompt");
    }
  };
  input.setRawMode(true);
  try {
    input.on("data", onData).once("end", end).once("error", end);
    await runtime.login(provider, "oauth", { signal, prompt, notify }, { agentName: "hush" });
  } finally {
    // Also cancel a pending manual prompt if the provider flow rejected.
    terminal.abort();
    input.off("data", onData).off("end", end).off("error", end);
    input.pause();
    input.setRawMode(raw);
  }
}

export async function runAuthCommand(args: string[]) {
  const [action] = args;
  if (args.length !== 1 || action !== "login" && action !== "status" && action !== "logout") {
    screen("Usage: hush auth login|status|logout");
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
  try {
    // No Pi defaults, and never allow an inherited callback binding override.
    process.env.PI_OFFLINE = "1";
    delete process.env.PI_OAUTH_CALLBACK_HOST;
    registerBunOAuthFlows();
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
      const signIn = async (store: CredentialStore) => {
        // An overlay must not change the provider login offer. Test keyring
        // access before beginning a browser flow that may spend a code.
        runtime = await create(store, false);
        if (keyring) await runtime.listCredentials({ signal: abort.signal });
        try { await login(runtime, abort); }
        finally { await runtime.settleCredentialWrites(); }
      };
      if (keyring) await keyring.exclusive(signIn, abort.signal);
      else await signIn(credentials);
      process.stdout.write("openai-codex: signed in with OAuth\n");
      return 0;
    }
    if (action === "logout") {
      runtime = await create(credentials, false);
      await runtime.logout(provider);
    }
    await privateFile(modelsPath);
    runtime = await create(credentials, true);
    const opts = { signal: AbortSignal.timeout(5_000) };
    const managed = (await runtime.listCredentials(opts)).find(entry => entry.providerId === provider)?.type;
    const oauth = managed === "oauth" && (await runtime.checkAuth(provider, opts))?.type === "oauth";
    // A managed credential can hide a configured alternative. Check that
    // same Hush config with an empty in-memory store, never by deleting data.
    const withoutManaged = managed ? await create(new InMemoryCredentialStore(), true) : runtime;
    const alternative = withoutManaged.getProviderAuthStatus(provider).configured ||
      (await withoutManaged.checkAuth(provider, opts))?.type === "api_key";
    const status = managed === "oauth" ? (oauth ? "OAuth configured" : "stored OAuth unavailable")
      : managed === "api_key" ? "stored API key (not OAuth)" : "no Hush-managed credential";
    process.stdout.write("openai-codex: " + status + "\n");
    if (alternative) process.stdout.write("openai-codex: a configured alternative remains (not Hush OAuth)\n");
    return 0;
  } catch (error) {
    if (abort.signal.aborted) {
      screen("OpenAI Codex sign-in cancelled. Run 'hush auth status' to check the stored credential.");
      return 130;
    }
    screen(credentialAdvice(error) ?? "Could not complete OpenAI Codex auth. Check the private Hush profile and run 'hush auth status' to inspect it.");
    return 1;
  } finally {
    await runtime?.settleCredentialWrites().catch(() => {});
    if (action === "login") for (const signal of signals) process.off(signal, stop);
  }
}
