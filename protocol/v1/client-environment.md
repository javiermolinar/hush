# Client environment (`clientEnvironment:1`)

The standalone Hush host may select `clientEnvironment:1` in `hello`. This extension is not offered by the opt-in Pi profile. It does not change the core `submit`, `continue`, state, history or image frames.

With the capability selected, send `ext.clientEnvironment.submit` with exactly `{input, environment}`. `input` is the unchanged core `submit` parameters, or the unchanged `ext.images.submit` parameters if `images:1` is also selected. A successful reply returns the same `{submissionId}` as `submit`. The submission key, busy policy, and validation/admission rules are unchanged. The first admitted input owns its environment as well as its user content. SDK fallback uses the original method before the first write if the extension is not selected. A retry after a lost reply does not change the method or payload, even if a subsequent hello lacks this capability.

`environment` is a hint object with optional fields. Only these exact IDs are copied; unknown keys, unknown values and non-object hints are ignored:

| Field | Values |
| --- | --- |
| `os` | `macos`, `linux` |
| `terminal` | `apple-terminal`, `iterm2`, `ghostty`, `kitty`, `wezterm`, `alacritty`, `foot`, `xterm`, `vscode`, `warp` |
| `multiplexer` | `rex`, `zellij`, `cmux`, `tmux` |
| `shell`, `loginShellHint` | `bash`, `zsh`, `fish`, `sh`, `dash`, `ash`, `ksh`, `csh`, `tcsh`, `nu`, `pwsh` |

The TUI collects facts once per invocation. A recognized invoking shell takes precedence over `$SHELL`, whose basename is at most a login-shell hint. No raw environment value, process or host identifier, or path is transmitted. The client host may differ from the tool host. The backend reports its own OS and the known tool shell independently (the pinned Unix default selects `/bin/bash` first; an unverified fallback is omitted).

Facts travel with the submitted input through queueing and placement. They are not thread state, session properties, chat text or wire history. A generation request gets client facts only if all its active inputs have the same known hints. Old, mixed, unadorned or unproven inputs do not inherit facts. Headless, extension-driven input without an explicit client, and unnegotiated clients use the ordinary submission path. Summary and compaction requests do not borrow client facts. Rendering does not change tool execution, permissions, or multiplexer controls.
