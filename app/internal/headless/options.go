package headless

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/javiermolinar/hush/app/internal/authproviders"
	"github.com/javiermolinar/hush/sdk"
	"github.com/spf13/pflag"
)

const usage = `Usage:
  hush [--backend BUNDLE | --socket PATH] [--thread ID | -t ID] [--local] [--theme NAME] [--fills auto|on|off] [--images auto|kitty|blocks|none] [--motion=false]
  hush (--print | --mode text) [--socket PATH] [--thread ID] [--request-id KEY] [PROMPT...]
  hush [--socket PATH] [--thread ID] [--request-id KEY] -- PROMPT...
  hush --no-wait [--json] [--thread ID] [--request-id KEY] [PROMPT...]
  hush --no-session [--backend BUNDLE] [PROMPT...]
  hush status [--thread ID] [--json]
  hush service install [--backend BUNDLE]
  hush service status [--json]
  hush service pause|resume|restart|uninstall
  hush stop [--socket PATH] [--json | -q]
  hush restart [--backend BUNDLE] [--open [--thread ID] [--theme NAME]]
  hush auth login|status|logout [PROVIDER] [--backend BUNDLE]
  hush trust status|grant|revoke [--cwd PATH | --scope home] [--json]
  hush store inspect|activate|rollback [--backend BUNDLE]
  hush store prepare --from PINNED-HUSH [--backend BUNDLE]
  hush wait --thread ID [--json]
  hush thread list [--all] [--limit N] [--cursor CURSOR] [--json]
  hush thread new [--cwd PATH] [--title TITLE] [--worktree] [--json]
  hush thread fork --thread ID --at-entry ID [--json]
  hush thread archive|unarchive --thread ID [--json]
  hush submission get|wait --thread ID (--request-id KEY | --submission-id ID) [--json]
  hush review start --thread ID --request-id KEY [--tracked PATH] [--untracked PATH] (--brief TEXT | --reviewer NAME=BRIEF) [--parallel]
      [--check ID=DESCRIPTION] [--evidence ID:LABEL=TEXT] [--prompt TEXT]
      [--model PROVIDER/ID --thinking LEVEL] [--worker-timeout-ms MS] [--json]
  hush review status --thread ID --request-id KEY [--json]
  hush review stop HANDLE --thread ID [--json]
  hush review transcript HANDLE --thread ID [--json]
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

With terminal stdin and stdout, plain hush opens the TUI. Use --print (or -p)
or -- before a prompt, or pipe it to hush on stdin. Persistent commands
reuse or start the default local backend (use --backend or HUSH_BACKEND to
select a bundle). --socket and HUSH_SOCKET are attach-only; --mode protocol
just relays bytes. --no-session starts a private backend.
Auth commands default to openai-codex; use /login to see the local OAuth providers.
`

type command uint8

const (
	printAnswer command = iota
	backendStatus
	waitIdle
	getSubmission
	waitSubmission
	rawProtocol
	listThreads
	newThread
	forkThread
	archiveThread
	unarchiveThread
	localAuth
	stopBackend
	restartBackend
	localTrust
	localStore
	scheduleCommand
	checkpointCommand
	localService
	reviewCommand
)

// InteractiveOptions are passed to the terminal frontend only after routing.
type InteractiveOptions struct {
	Socket, Bundle, Fills, Images, Theme string
	Thread                               sdk.ID
	PaneEpoch                            string // private attach-only pane launch
	Local                                *bool  // nil: infer from socket selection
	Motion                               bool
}

type options struct {
	command                 command
	prompt                  []string
	socket, bundle, key     string
	paneEpoch               string
	thread, submission      sdk.ID
	forcePrint, help, json  bool
	implicitPrompt          bool
	quiet, restartOpen      bool
	noWait, ephemeral       bool
	textMode                bool
	authAction, trustAction string
	authProvider            string
	trustScope              string
	storeAction, storeFrom  string
	serviceAction           string
	schedule                scheduleOptions
	checkpoint              checkpointOptions
	review                  reviewOptions
	directory               sdk.ThreadDirectoryOptions
	create                  sdk.ThreadCreation
	atEntry                 sdk.ID
	ui                      InteractiveOptions
	uiFlags                 bool
}

func (o options) interactive(inTTY, outTTY bool) bool {
	return inTTY && outTTY && o.command == printAnswer && !o.help && !o.forcePrint && !o.textMode &&
		!o.noWait && !o.ephemeral && o.key == "" && len(o.prompt) == 0
}

func parse(args []string) (options, error) {
	var o options
	var mode, thread, submission, profile, atEntry string
	var local bool
	f := pflag.NewFlagSet("hush", pflag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&mode, "mode", "", "")
	f.BoolVarP(&o.forcePrint, "print", "p", false, "")
	f.BoolVarP(&o.help, "help", "h", false, "")
	f.StringVar(&o.socket, "socket", "", "")
	f.StringVar(&o.paneEpoch, "pane-epoch", "", "")
	_ = f.MarkHidden("pane-epoch")
	f.StringVar(&o.bundle, "backend", "", "")
	f.StringVar(&o.storeFrom, "from", "", "")
	// Retain the old standalone spelling, not a profile selector.
	f.StringVar(&profile, "profile", "", "")
	f.StringVarP(&thread, "thread", "t", "", "")
	f.StringVar(&o.create.CWD, "cwd", "", "")
	f.StringVar(&o.trustScope, "scope", "", "")
	f.StringVar(&o.create.Title, "title", "", "")
	f.BoolVar(&o.create.Worktree, "worktree", false, "")
	f.IntVar(&o.directory.Limit, "limit", 50, "")
	f.StringVar(&o.directory.Cursor, "cursor", "", "")
	f.BoolVar(&o.directory.IncludeArchived, "all", false, "")
	f.StringVar(&atEntry, "at-entry", "", "")
	f.BoolVar(&local, "local", false, "")
	f.StringVar(&o.ui.Theme, "theme", "", "")
	f.StringVar(&o.ui.Fills, "fills", "", "") // unset uses the choice saved in /theme
	f.StringVar(&o.ui.Images, "images", "auto", "")
	f.BoolVar(&o.ui.Motion, "motion", true, "")
	f.StringVar(&submission, "submission-id", "", "")
	f.StringVar(&o.key, "request-id", "", "")
	f.BoolVar(&o.json, "json", false, "")
	f.BoolVarP(&o.quiet, "quiet", "q", false, "")
	f.BoolVar(&o.restartOpen, "open", false, "")
	f.BoolVar(&o.noWait, "no-wait", false, "")
	f.BoolVar(&o.ephemeral, "no-session", false, "")
	o.schedule.flags(f)
	o.checkpoint.flags(f)
	o.review.flags(f)
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if o.help {
		return o, nil
	}
	if !f.Changed("profile") {
		profile = os.Getenv("HUSH_PROFILE")
	}
	if profile != "" && profile != "hush" || f.Changed("profile") && profile == "" {
		return o, errors.New("only the Hush backend is supported")
	}
	rest := f.Args()
	if f.Changed("pane-epoch") {
		review := len(rest) == 3 && rest[0] == "review" && rest[1] == "transcript" && f.ArgsLenAtDash() != 0
		view := len(rest) == 0 && (!f.Changed("thread") || sdk.ID(thread).Valid()) && !o.noWait && !o.ephemeral && !o.forcePrint && mode == "" && o.key == ""
		if (!review && !view) || o.socket == "" || f.Changed("backend") || len(o.paneEpoch) == 0 || len(o.paneEpoch) > 160 ||
			strings.Trim(o.paneEpoch, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
			return o, errors.New("pane attachment requires an explicit socket and a valid backend epoch")
		}
	}
	if f.Changed("theme") {
		if o.ui.Theme == "" {
			return o, errors.New("--theme must not be empty")
		}
		if f.Changed("mode") || f.Changed("print") || f.Changed("no-session") || f.Changed("no-wait") ||
			len(rest) != 0 && !(len(rest) == 1 && rest[0] == "restart" && o.restartOpen && f.ArgsLenAtDash() != 0) {
			return o, errors.New("--theme requires the interactive TUI")
		}
	}
	// Reserve unescaped auth syntax even in an explicit generation mode. An
	// incompatible option must not accidentally send a login command as chat.
	if len(rest) != 0 && rest[0] == "auth" && f.ArgsLenAtDash() != 0 {
		if len(rest) < 2 || len(rest) > 3 || !slices.Contains([]string{"login", "status", "logout"}, rest[1]) {
			return o, errors.New("use auth login, status, or logout with an optional provider ID")
		}
		if len(rest) == 3 {
			if _, ok := authproviders.Lookup(rest[2]); !ok {
				return o, errors.New("unsupported auth provider; use /login to see the OAuth providers")
			}
			o.authProvider = rest[2]
		}
		var invalid bool
		f.Visit(func(v *pflag.Flag) {
			invalid = invalid || v.Name != "backend" && v.Name != "profile"
		})
		if invalid || f.Changed("backend") && o.bundle == "" {
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
			invalid = invalid || !slices.Contains([]string{"backend", "profile", "cwd", "json", "scope"}, v.Name)
		})
		if invalid || f.Changed("backend") && o.bundle == "" ||
			f.Changed("cwd") && o.create.CWD == "" {
			return o, errors.New("trust is local to the Hush backend host and accepts only --backend, --profile hush, --cwd, --json and --scope")
		}
		// Keep --scope work as an alias; never broaden a command asking for
		// the old Git-only or browse-only permission.
		if f.Changed("scope") && o.trustScope != "home" && o.trustScope != "work" ||
			o.trustScope == "home" && f.Changed("cwd") {
			return o, errors.New("use --cwd PATH for workspace trust or --scope home for home browsing (not both)")
		}
		o.command, o.trustAction = localTrust, rest[1]
		return o, nil
	}
	if f.Changed("scope") {
		return o, errors.New("--scope is for trust")
	}
	if len(rest) != 0 && rest[0] == "service" && f.ArgsLenAtDash() != 0 {
		if len(rest) != 2 || !slices.Contains([]string{"install", "status", "pause", "resume", "restart", "uninstall"}, rest[1]) {
			return o, errors.New("use service install, status, pause, resume, restart, or uninstall")
		}
		var invalid bool
		f.Visit(func(v *pflag.Flag) {
			invalid = invalid || v.Name != "profile" && !(v.Name == "backend" && rest[1] == "install") &&
				!(v.Name == "json" && rest[1] == "status")
		})
		if invalid || f.Changed("backend") && o.bundle == "" {
			return o, errors.New("service requires the local Hush profile; only install accepts --backend and only status accepts --json")
		}
		o.command, o.serviceAction = localService, rest[1]
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
		if invalid || f.Changed("backend") && o.bundle == "" {
			return o, errors.New("store operations are local to the managed Hush backend host and do not accept a socket")
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
	if len(rest) != 0 && rest[0] == "review" && f.ArgsLenAtDash() != 0 {
		o.command = reviewCommand
		if o.bundle != "" && o.socket != "" {
			return o, errors.New("--backend selects a local bundle; it cannot be combined with --socket")
		}
		err := o.review.parse(f, sdk.ID(thread), o.key, rest[1:])
		return o, err
	}
	for _, flag := range reviewFlags {
		if f.Changed(flag) {
			return o, fmt.Errorf("--%s is for review start", flag)
		}
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
	default:
		return o, fmt.Errorf("unknown mode %q; use --json on native commands for JSON output", mode)
	}

	escaped := f.ArgsLenAtDash() == 0
	if o.command == printAnswer && !escaped && !o.forcePrint && !o.textMode && len(rest) > 0 {
		switch rest[0] {
		case "status":
			o.command, rest = backendStatus, rest[1:]
		case "stop":
			o.command, rest = stopBackend, rest[1:]
		case "restart":
			o.command, rest = restartBackend, rest[1:]
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
		default:
			o.implicitPrompt = !o.noWait && !o.ephemeral
		}
	}
	if o.command != printAnswer && o.noWait {
		return o, errors.New("--no-wait only applies to submitting a prompt")
	}
	if o.command != restartBackend && f.Changed("open") {
		return o, errors.New("--open is for restart")
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
		if emptyFlag == "" && v.Value.String() == "" {
			emptyFlag = v.Name
		}
	})
	if emptyFlag != "" {
		return o, fmt.Errorf("--%s must not be empty", emptyFlag)
	}
	o.thread, o.submission, o.atEntry = sdk.ID(thread), sdk.ID(submission), sdk.ID(atEntry)
	o.ui.Socket, o.ui.Bundle, o.ui.Thread, o.ui.PaneEpoch = o.socket, o.bundle, o.thread, o.paneEpoch
	o.uiFlags = o.paneEpoch != "" || f.Changed("local") || f.Changed("fills") || f.Changed("images") || f.Changed("motion") || f.Changed("theme")
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
	if o.command == printAnswer {
		o.prompt = rest
	} else if len(rest) != 0 {
		return o, errors.New("unexpected positional arguments")
	}
	if !o.ephemeral && o.bundle != "" && o.socket != "" {
		return o, errors.New("--backend selects a local bundle; it cannot be combined with --socket")
	}
	if o.ephemeral && (o.command != printAnswer || o.noWait || o.thread != "" || o.socket != "" || o.key != "" || o.submission != "") {
		return o, errors.New("--no-session requires a new, attached invocation; no durable socket, IDs, or --no-wait")
	}

	switch o.command {
	case printAnswer:
		if o.submission != "" || o.json && !o.noWait {
			return o, errors.New("--submission-id is for submission commands; --json requires a durable command or --no-wait")
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
	case restartBackend:
		if o.socket != "" {
			return o, errors.New("restart requires a managed local backend; --socket is attach-only")
		}
		if o.thread != "" && !o.restartOpen {
			return o, errors.New("restart only accepts --thread with --open")
		}
		var invalid string
		f.Visit(func(v *pflag.Flag) {
			if invalid == "" && !slices.Contains([]string{"backend", "profile", "open", "thread"}, v.Name) &&
				!(v.Name == "theme" && o.restartOpen) {
				invalid = v.Name
			}
		})
		if invalid != "" {
			return o, fmt.Errorf("--%s is not valid for restart", invalid)
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
