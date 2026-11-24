package headless

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/javiermolinar/hush/sdk"
	"github.com/spf13/pflag"
)

const usage = `Usage:
  hush [--backend BUNDLE | --socket PATH] [--thread ID] [--local] [--fills auto|on|off] [--images auto|kitty|blocks|none] [--motion=false]
  hush [--print | --mode text] [--socket PATH] [--thread ID] [--request-id KEY] [PROMPT...]
  hush --no-wait [--json] [--thread ID] [--request-id KEY] [PROMPT...]
  hush --no-session [--backend BUNDLE] [PROMPT...]
  hush status [--thread ID] [--json]
  hush stop [--socket PATH] [--json | -q]
  hush auth login|status|logout [--backend BUNDLE]
  hush trust status|grant|revoke [--cwd PATH] [--json]
  hush store inspect|activate|rollback [--backend BUNDLE]
  hush store prepare --from PINNED-HUSH [--backend BUNDLE]
  hush wait --thread ID [--json]
  hush thread list [--all] [--limit N] [--cursor CURSOR] [--json]
  hush thread new [--cwd PATH] [--title TITLE] [--worktree] [--json]
  hush thread fork --thread ID --at-entry ID [--json]
  hush thread archive|unarchive --thread ID [--json]
  hush submission get|wait --thread ID (--request-id KEY | --submission-id ID) [--json]
  hush schedule add --thread ID --target ID --at RFC3339 --text TEXT [--after THREAD/SUBMISSION] [--allow-late] [--json]
  hush schedule list --thread ID [--json]
  hush schedule run|cancel OCCURRENCE --thread ID [--json]
  hush schedule reschedule OCCURRENCE --thread ID --at RFC3339 [--json]
  hush checkpoint list|bookmark|files --thread ID [--at-entry ID] [--title LABEL] [--json]
  hush checkpoint effect --thread ID --text TEXT [--json]
  hush rewind preview --thread ID (--checkpoint ID | --at-entry ID) --restore files|transcript [--json]
  hush rewind confirm OPERATION --thread ID --restore files|transcript [--json]
  hush rewind status OPERATION --thread ID [--json]
  hush --mode protocol [--socket PATH]
  hush --mode json [--no-session] [--model MODEL] [MESSAGE...]
  hush --mode rpc [--no-session] [--model MODEL]
  hush --list-models [PATTERN]

With terminal stdin and stdout, and no prompt or headless mode, hush opens
the TUI. Use --print (or -p) to force one-shot text. Persistent commands
reuse or start the default local backend (use --backend or
HUSH_BACKEND to select a bundle). --socket and HUSH_SOCKET are attach-only;
--mode protocol just relays bytes. --no-session starts a private backend.
Use --profile pi (or HUSH_PROFILE=pi) to select the legacy Pi profile.
Pi modes and options select it explicitly and require a backend with piEvents.
`

type command uint8

const (
	printAnswer command = iota
	backendStatus
	waitIdle
	getSubmission
	waitSubmission
	rawProtocol
	piJSON
	piRPC
	piModels
	listThreads
	newThread
	forkThread
	archiveThread
	unarchiveThread
	localAuth
	stopBackend
	localTrust
	localStore
	scheduleCommand
	checkpointCommand
)

// InteractiveOptions are passed to the terminal frontend only after routing.
type InteractiveOptions struct {
	Socket, Bundle, Fills, Images string
	Profile                       string
	Thread                        sdk.ID
	Local                         *bool // nil: infer from socket selection
	Motion                        bool
}

type options struct {
	command                 command
	prompt                  []string
	socket, bundle, key     string
	thread, submission      sdk.ID
	forcePrint, help, json  bool
	quiet                   bool
	noWait, ephemeral       bool
	pi                      sdk.PiOptions
	profile, textMode       bool
	appendPrompt            []string
	fileArgs                []string
	modelFilter             string
	authAction, trustAction string
	storeAction, storeFrom  string
	schedule                scheduleOptions
	checkpoint              checkpointOptions
	directory               sdk.ThreadDirectoryOptions
	create                  sdk.ThreadCreation
	atEntry                 sdk.ID
	ui                      InteractiveOptions
	uiFlags                 bool
}

func (o options) interactive(inTTY, outTTY bool) bool {
	return inTTY && outTTY && o.command == printAnswer && !o.help && !o.forcePrint && !o.textMode &&
		!o.noWait && !o.ephemeral && !o.profile && o.key == "" && len(o.prompt) == 0
}

func parse(args []string) (options, error) {
	var o options
	var mode, thread, submission, fork, sessionDir, systemPrompt, atEntry string
	var resume, local bool
	f := pflag.NewFlagSet("hush", pflag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&mode, "mode", "", "")
	f.BoolVarP(&o.forcePrint, "print", "p", false, "")
	f.BoolVarP(&o.help, "help", "h", false, "")
	f.StringVar(&o.socket, "socket", "", "")
	f.StringVar(&o.bundle, "backend", "", "")
	f.StringVar(&o.storeFrom, "from", "", "")
	f.StringVar(&o.ui.Profile, "profile", "", "")
	f.StringVar(&thread, "thread", "", "")
	f.StringVar(&o.create.CWD, "cwd", "", "")
	f.StringVar(&o.create.Title, "title", "", "")
	f.BoolVar(&o.create.Worktree, "worktree", false, "")
	f.IntVar(&o.directory.Limit, "limit", 50, "")
	f.StringVar(&o.directory.Cursor, "cursor", "", "")
	f.BoolVar(&o.directory.IncludeArchived, "all", false, "")
	f.StringVar(&atEntry, "at-entry", "", "")
	f.BoolVar(&local, "local", false, "")
	f.StringVar(&o.ui.Fills, "fills", "auto", "")
	f.StringVar(&o.ui.Images, "images", "auto", "")
	f.BoolVar(&o.ui.Motion, "motion", true, "")
	f.StringVar(&submission, "submission-id", "", "")
	f.StringVar(&o.key, "request-id", "", "")
	f.BoolVar(&o.json, "json", false, "")
	f.BoolVarP(&o.quiet, "quiet", "q", false, "")
	f.BoolVar(&o.noWait, "no-wait", false, "")
	f.BoolVar(&o.ephemeral, "no-session", false, "")
	f.StringVar(&o.pi.Model, "model", "", "")
	f.StringVar(&o.pi.Provider, "provider", "", "")
	f.StringVar(&o.pi.Thinking, "thinking", "", "")
	f.StringSliceVar(&o.pi.Models, "models", nil, "")
	f.StringVarP(&o.pi.Name, "name", "n", "", "")
	f.BoolVarP(&o.pi.Continue, "continue", "c", false, "")
	f.BoolVarP(&resume, "resume", "r", false, "")
	f.StringVar(&o.pi.Session, "session", "", "")
	f.StringVar(&sessionDir, "session-dir", "", "")
	f.StringVar(&fork, "fork", "", "")
	f.StringVar(&systemPrompt, "system-prompt", "", "")
	f.StringArrayVar(&o.appendPrompt, "append-system-prompt", nil, "")
	f.StringVar(&o.modelFilter, "list-models", "", "")
	f.Lookup("list-models").NoOptDefVal = "*"
	o.schedule.flags(f)
	o.checkpoint.flags(f)
	if err := f.Parse(listModelArgs(args)); err != nil {
		return o, err
	}
	if o.help {
		return o, nil
	}
	rest := f.Args()
	// Reserve unescaped auth syntax even in an explicit generation mode. An
	// incompatible option must not accidentally send a login command as chat.
	if len(rest) != 0 && rest[0] == "auth" && f.ArgsLenAtDash() != 0 {
		if len(rest) != 2 || !slices.Contains([]string{"login", "status", "logout"}, rest[1]) {
			return o, errors.New("use auth login, status, or logout")
		}
		var invalid bool
		f.Visit(func(v *pflag.Flag) {
			invalid = invalid || v.Name != "backend" && v.Name != "profile"
		})
		if invalid || f.Changed("backend") && o.bundle == "" || f.Changed("profile") && o.ui.Profile != "hush" {
			return o, errors.New("auth requires the local Hush profile and accepts only --backend and --profile hush")
		}
		o.command, o.authAction = localAuth, rest[1]
		return o, nil
	}
	if len(rest) != 0 && rest[0] == "trust" && f.ArgsLenAtDash() != 0 {
		if len(rest) != 2 || !slices.Contains([]string{"status", "grant", "revoke"}, rest[1]) {
			return o, errors.New("use trust status, grant, or revoke")
		}
		var invalid bool
		f.Visit(func(v *pflag.Flag) {
			invalid = invalid || !slices.Contains([]string{"backend", "profile", "cwd", "json"}, v.Name)
		})
		if invalid || f.Changed("backend") && o.bundle == "" ||
			f.Changed("profile") && o.ui.Profile != "hush" || f.Changed("cwd") && o.create.CWD == "" {
			return o, errors.New("trust is local to the Hush backend host and accepts only --backend, --profile hush, --cwd and --json")
		}
		o.command, o.trustAction = localTrust, rest[1]
		return o, nil
	}
	if len(rest) != 0 && rest[0] == "store" && f.ArgsLenAtDash() != 0 {
		if len(rest) != 2 || !slices.Contains([]string{"inspect", "prepare", "activate", "rollback"}, rest[1]) {
			return o, errors.New("use store inspect, prepare --from PINNED-HUSH, activate, or rollback")
		}
		var invalid bool
		f.Visit(func(v *pflag.Flag) {
			invalid = invalid || !slices.Contains([]string{"backend", "profile", "from", "json"}, v.Name)
		})
		if invalid || f.Changed("backend") && o.bundle == "" || f.Changed("profile") && o.ui.Profile != "hush" {
			return o, errors.New("store operations are local to the managed Hush backend host and do not accept a socket or Pi profile")
		}
		if f.Changed("from") != (rest[1] == "prepare") || f.Changed("from") && o.storeFrom == "" {
			return o, errors.New("store prepare requires --from PINNED-HUSH; other store operations do not accept it")
		}
		o.command, o.storeAction = localStore, rest[1]
		return o, nil
	}
	if f.Changed("from") {
		return o, errors.New("--from is for store prepare")
	}
	if len(rest) != 0 && (rest[0] == "checkpoint" || rest[0] == "rewind") && f.ArgsLenAtDash() != 0 {
		o.command = checkpointCommand
		if o.bundle != "" && o.socket != "" {
			return o, errors.New("--backend selects a local bundle; it cannot be combined with --socket")
		}
		o.checkpoint.thread, o.checkpoint.at = sdk.ID(thread), sdk.ID(atEntry)
		o.checkpoint.label, o.checkpoint.effect = o.create.Title, o.schedule.text
		err := o.checkpoint.parse(f, rest)
		return o, err
	}
	if f.Changed("restore") || f.Changed("checkpoint") {
		return o, errors.New("--restore and --checkpoint are for rewind")
	}
	if len(rest) != 0 && rest[0] == "schedule" && f.ArgsLenAtDash() != 0 {
		o.command = scheduleCommand
		if o.bundle != "" && o.socket != "" {
			return o, errors.New("--backend selects a local bundle; it cannot be combined with --socket")
		}
		err := o.schedule.parse(f, sdk.ID(thread), rest[1:])
		return o, err
	}
	if o.schedule.haveFlags(f) {
		return o, errors.New("--target, --at, --after, --text and --allow-late are for schedule")
	}
	switch mode {
	case "":
	case "print":
		o.forcePrint = true
	case "text":
		o.textMode = true
	case "protocol":
		o.command = rawProtocol
	case "json":
		o.command = piJSON
	case "rpc":
		o.command = piRPC
	default:
		return o, fmt.Errorf("unknown mode %q", mode)
	}

	for _, name := range []string{"model", "provider", "thinking", "models", "name", "continue", "resume", "session", "session-dir", "fork", "system-prompt", "append-system-prompt"} {
		o.profile = o.profile || f.Changed(name)
	}
	if f.Changed("system-prompt") {
		o.pi.SystemPrompt = &systemPrompt
	}
	if f.Changed("list-models") {
		if o.command != printAnswer || o.profile || o.forcePrint || o.ephemeral || o.textMode || o.noWait || o.json || thread != "" || submission != "" || o.key != "" {
			return o, errors.New("--list-models only accepts a filter and backend selection")
		}
		o.command = piModels
	}
	if fork != "" {
		if o.pi.Session != "" || o.pi.Continue {
			return o, errors.New("--fork cannot be combined with --session or --continue")
		}
		o.pi.Session, o.pi.Fork = fork, true
	}
	escaped := f.ArgsLenAtDash() == 0
	if o.command == printAnswer && !escaped && !o.forcePrint && !o.textMode && !o.profile && len(rest) > 0 {
		switch rest[0] {
		case "status":
			o.command, rest = backendStatus, rest[1:]
		case "stop":
			o.command, rest = stopBackend, rest[1:]
		case "wait":
			o.command, rest = waitIdle, rest[1:]
		case "thread":
			commands := map[string]command{"list": listThreads, "new": newThread, "fork": forkThread, "archive": archiveThread, "unarchive": unarchiveThread}
			var ok bool
			if len(rest) >= 2 {
				o.command, ok = commands[rest[1]]
			}
			if !ok {
				return o, errors.New("use thread list, new, fork, archive, or unarchive")
			}
			rest = rest[2:]
		case "submission":
			if len(rest) >= 2 && rest[1] == "get" {
				o.command = getSubmission
			} else if len(rest) >= 2 && rest[1] == "wait" {
				o.command = waitSubmission
			} else {
				return o, errors.New("use submission get or submission wait")
			}
			rest = rest[2:]
		}
	}
	if o.command != printAnswer && o.noWait {
		return o, errors.New("--no-wait only applies to submitting a prompt")
	}
	for _, group := range []struct {
		command command
		name    string
		flags   []string
	}{
		{newThread, "thread new", []string{"cwd", "title", "worktree"}},
		{listThreads, "thread list", []string{"limit", "cursor", "all"}},
		{forkThread, "thread fork", []string{"at-entry"}},
	} {
		for _, name := range group.flags {
			if o.command != group.command && f.Changed(name) {
				return o, fmt.Errorf("--%s is for %s", name, group.name)
			}
		}
	}

	var emptyFlag string
	f.Visit(func(v *pflag.Flag) {
		if emptyFlag == "" && v.Value.String() == "" && v.Name != "system-prompt" && v.Name != "list-models" {
			emptyFlag = v.Name
		}
	})
	if emptyFlag != "" {
		return o, fmt.Errorf("--%s must not be empty", emptyFlag)
	}
	o.thread, o.submission, o.atEntry = sdk.ID(thread), sdk.ID(submission), sdk.ID(atEntry)
	o.ui.Socket, o.ui.Bundle, o.ui.Thread = o.socket, o.bundle, o.thread
	o.uiFlags = f.Changed("local") || f.Changed("fills") || f.Changed("images") || f.Changed("motion")
	if f.Changed("local") {
		o.ui.Local = &local
	}
	if o.thread != "" && !o.thread.Valid() {
		return o, errors.New("--thread must be a valid durable ID")
	}
	if o.submission != "" && !o.submission.Valid() {
		return o, errors.New("--submission-id must be a valid durable ID")
	}
	if !utf8.ValidString(o.key) || utf8.RuneCountInString(o.key) > 200 {
		return o, errors.New("--request-id must be valid UTF-8 with at most 200 characters")
	}
	if o.command == printAnswer || o.command == piJSON {
		o.prompt = rest
		var text []string
		for _, arg := range rest {
			if strings.HasPrefix(arg, "@") {
				o.fileArgs = append(o.fileArgs, strings.TrimPrefix(arg, "@"))
				o.profile = true
			} else {
				text = append(text, arg)
			}
		}
		o.prompt = text
	} else if len(rest) != 0 {
		if o.command == piRPC {
			return o, errors.New("startup messages and @file arguments are not supported in RPC mode")
		}
		return o, errors.New("unexpected positional arguments")
	}
	if !o.ephemeral && o.bundle != "" && o.socket != "" {
		return o, errors.New("--backend selects a local bundle; it cannot be combined with --socket")
	}
	if o.ephemeral && ((o.command != printAnswer && o.command != piJSON && o.command != piRPC) || o.noWait || o.thread != "" || o.socket != "" || o.key != "" || o.submission != "" || o.pi.Continue || o.pi.Session != "" || resume || sessionDir != "") {
		return o, errors.New("--no-session requires a new, attached invocation; no durable socket, continuation, IDs, or --no-wait")
	}
	if resume || sessionDir != "" {
		return o, errors.New("--resume and --session-dir require Pi session import; no interactive picker in headless mode")
	}
	if o.pi.Continue && (o.pi.Session != "" || o.thread != "") || o.pi.Session != "" && o.thread != "" {
		return o, errors.New("use exactly one of --thread, --session, --fork, or --continue")
	}
	if o.pi.Thinking != "" && !slices.Contains([]string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}, o.pi.Thinking) {
		return o, errors.New("invalid --thinking level")
	}
	if f.Changed("models") {
		var scope []string
		for _, m := range o.pi.Models {
			if m = strings.TrimSpace(m); m != "" {
				scope = append(scope, m)
			}
		}
		if len(scope) == 0 {
			return o, errors.New("--models needs at least one name or pattern")
		}
		o.pi.Models = scope
	}
	if o.profile && (o.noWait || o.key != "" || o.command != printAnswer && o.command != piJSON && o.command != piRPC) {
		return o, errors.New("Pi profile options cannot be used with native durable commands")
	}

	switch o.command {
	case printAnswer:
		if o.submission != "" || o.json && !o.noWait {
			return o, errors.New("--submission-id is for submission commands; --json requires a durable command or --no-wait")
		}
	case piJSON, piRPC:
		if o.submission != "" || o.key != "" || o.json {
			return o, errors.New("Pi JSON/RPC do not accept native admission flags")
		}
	case rawProtocol:
		if o.thread != "" || o.submission != "" || o.key != "" || o.json || o.forcePrint || o.bundle != "" {
			return o, errors.New("--mode protocol only accepts a socket and raw stdin")
		}
	case backendStatus, waitIdle:
		if o.submission != "" || o.key != "" {
			return o, errors.New("submission IDs and keys are for submission commands")
		}
		if o.command == waitIdle && o.thread == "" {
			return o, errors.New("wait requires --thread")
		}
	case stopBackend:
		if o.thread != "" || o.submission != "" || o.key != "" {
			return o, errors.New("stop does not accept thread or submission selectors")
		}
		if o.bundle != "" {
			return o, errors.New("stop is attach-only and does not accept --backend")
		}
		if o.quiet && o.json {
			return o, errors.New("stop --quiet cannot be combined with --json")
		}
	case getSubmission, waitSubmission:
		if o.thread == "" || (o.key == "") == (o.submission == "") {
			return o, errors.New("submission commands require --thread and exactly one of --request-id or --submission-id")
		}
	case listThreads, newThread, forkThread, archiveThread, unarchiveThread:
		if o.submission != "" || o.key != "" {
			return o, errors.New("submission IDs and keys are for submission commands")
		}
		if o.command == listThreads || o.command == newThread {
			if o.thread != "" {
				return o, errors.New("--thread is for thread fork, archive, or unarchive")
			}
			if o.directory.Limit < 1 || o.directory.Limit > 100 {
				return o, errors.New("--limit must be between 1 and 100")
			}
		} else if !o.thread.Valid() || o.command == forkThread && !o.atEntry.Valid() {
			return o, errors.New("thread fork requires --thread and --at-entry; archive/unarchive requires --thread")
		}
	}
	if o.quiet && o.command != stopBackend {
		return o, errors.New("--quiet is for stop")
	}
	return o, nil
}

func listModelArgs(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--list-models" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			out := append([]string(nil), args[:i]...)
			out = append(out, arg+"="+args[i+1])
			return append(out, args[i+2:]...)
		}
	}
	return args
}

func readPrompt(args []string, in io.Reader, inputTTY bool) (string, error) {
	text := strings.Join(args, " ")
	if len(args) == 0 && !inputTTY {
		data, err := io.ReadAll(io.LimitReader(in, sdk.MaxRecord+1))
		if err != nil {
			return "", fmt.Errorf("read prompt: %w", err)
		}
		text = string(data)
	}
	switch {
	case len(text) > sdk.MaxRecord:
		return "", errors.New("prompt exceeds the native record limit")
	case !utf8.ValidString(text):
		return "", errors.New("prompt is not valid UTF-8")
	case strings.TrimSpace(text) == "":
		return "", errors.New("provide a prompt or redirect text to stdin")
	}
	return text, nil
}
