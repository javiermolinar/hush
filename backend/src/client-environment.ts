import { access } from "node:fs/promises";
import { getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import { GenerationTask, LiveDoc, hook, type Harness } from "@earendil-works/pi-durable";
import { clientEnvironment, environmentInputKind, type ClientEnvironment } from "../../server/src/client-environment.js";

const osName = (os: ClientEnvironment["os"]) => os === "macos" ? "macOS" : os === "linux" ? "Linux" : undefined;
const hostOS = osName(process.platform === "darwin" ? "macos" : process.platform === "linux" ? "linux" : undefined);

async function toolShell(offered: boolean): Promise<string | undefined> {
  // The pinned Unix NodeExecutionEnv selects /bin/bash first with no custom
  // shell, which is the standalone setting. On unusual hosts leave its later
  // PATH-dependent fallback unknown rather than probing by running commands.
  if (offered && hostOS && await access("/bin/bash").then(() => true, () => false)) return "bash";
}

function renderEnvironment(client: ClientEnvironment | undefined, executionShell: string | undefined) {
  const lines = ["## Environment"];
  const add = (label: string, value: string | undefined) => { if (value) lines.push(`- ${label}: ${value}`); };
  add("Tool host OS", hostOS);
  add("Tool execution shell", executionShell);
  add("Client OS", osName(client?.os));
  add("Client terminal", client?.terminal);
  add("Client multiplexer", client?.multiplexer);
  add("Client shell (invoking process)", client?.shell);
  add("Client login-shell hint (active shell unknown)", client?.loginShellHint);
  lines.push("", "Use the tool execution shell for tool calls. Use the client shell for paste-ready commands and scripts without a declared target. If there is only a login-shell hint, qualify it; do not assume it is the active shell. Explicit shell requests, personal script-shell preferences, and existing shebangs or project/CI targets take precedence. These facts grant no permissions. Multiplexer control is separate.");
  return lines.join("\n");
}

// Attach this only to the standalone prompt extension. Durable calls it for
// this generation attempt, not for compaction, title generation, or replay.
// An ephemeral section avoids persisting past client facts in system entries.
export function environmentPrompt(getHarness: () => Harness | undefined) {
  return hook(GenerationTask, { async beforeRequest({ messages }, api, ctx) {
    let client: ClientEnvironment | undefined;
    try {
      client = await getHarness()?.commit(async tx => {
        const { run } = await tx.doc(LiveDoc, api.conversationId);
        // Queued items are not active inputs. A continue has no client, and
        // a steer merged into a run can make the client ambiguous.
        if (!run || run.taskId !== api.taskId || run.inputs.length === 0 || run.inputs.length > 64) return;
        let unanimous: ClientEnvironment | undefined;
        for (const id of run.inputs) {
          const input = await tx.submission(id);
          if (input?.type !== "input" || input.status !== "placed" || input.conversationId !== api.conversationId || input.entry === undefined) return;
          const entry = await tx.entry(input.entry);
          const facts = entry?.kind === environmentInputKind && entry.conversationId === api.conversationId
            ? clientEnvironment(entry.data) : undefined;
          if (!facts || unanimous && JSON.stringify(unanimous) !== JSON.stringify(facts)) return;
          unanimous = facts;
        }
        return unanimous;
      }, ctx);
    } catch (error) {
      if (ctx.abortSignal?.aborted) throw error;
      // Unavailable input provenance is never a reason to reuse old facts.
    }
    const shell = await toolShell(getCurrentTools(messages).some(tool => tool.name === "bash"));
    const section = { hushEnvironment: renderEnvironment(client, shell) };
    const lastSystem = messages.findLastIndex(message => message.role === "system");
    const existing = messages[lastSystem];
    if (existing?.role !== "system") {
      return { messages: [{ role: "system", content: "", timestamp: api.now(), sections: section }, ...messages] };
    }
    // Preserve the position and tool declarations of the existing prompt.
    // Do not append a synthetic last message after an input or tool result.
    return { messages: messages.with(lastSystem, { ...existing, sections: { ...existing.sections, ...section } }) };
  } });
}
