import { object } from "./wire.js";

export const clientEnvironmentVersion = 1;
// Internal Durable entry kind. Native v1 projects it as the usual pi.user.
export const environmentInputKind = "hush.clientInput";

const ids = {
  os: ["macos", "linux"],
  terminal: ["apple-terminal", "iterm2", "ghostty", "kitty", "wezterm", "alacritty", "foot", "xterm", "vscode", "warp"],
  multiplexer: ["rex", "zellij", "cmux", "tmux"],
  shell: ["bash", "zsh", "fish", "sh", "dash", "ash", "ksh", "csh", "tcsh", "nu", "pwsh"],
} as const;

type Fact = keyof typeof ids;
type Environment = { [K in Fact]?: typeof ids[K][number] };
export type ClientEnvironment = Environment & { loginShellHint?: Environment["shell"] };

// Copy only exact enum members. Unknown keys, objects and strings are never
// stored, rendered or reflected in errors. Reuse when reading stored data.
export function clientEnvironment(value: unknown): ClientEnvironment | undefined {
  if (!object(value)) return;
  const fact = <K extends Fact>(key: K, input: unknown = value[key]): Environment[K] =>
    (ids[key] as readonly unknown[]).includes(input) ? input as Environment[K] : undefined;
  const os = fact("os"), terminal = fact("terminal"), multiplexer = fact("multiplexer"), shell = fact("shell");
  const loginShellHint = shell ? undefined : fact("shell", value.loginShellHint ?? null);
  if (!os && !terminal && !multiplexer && !shell && !loginShellHint) return;
  return { ...(os && { os }), ...(terminal && { terminal }), ...(multiplexer && { multiplexer }),
    ...(shell && { shell }), ...(loginShellHint && { loginShellHint }) };
}

export function environmentEntry(hints: ClientEnvironment | undefined) {
  return hints === undefined ? undefined : { kind: environmentInputKind, data: { ...hints } };
}
