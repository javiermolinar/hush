package headless

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/javiermolinar/hush/app/internal/backendstart"
	"github.com/javiermolinar/hush/sdk"
	"golang.org/x/term"
)

// Run dispatches to the provided terminal frontend or to the headless CLI.
// Headless commands do not start the UI or change terminal settings.
func Run(ctx context.Context, args []string, in, out, stderr *os.File,
	openUI func(context.Context, InteractiveOptions) error) (code int) {
	report := func(err error) int {
		if err == nil {
			return 0
		}
		if ctx.Err() != nil {
			return 1
		}
		var outcome *piAnswerError
		if errors.As(err, &outcome) {
			fmt.Fprintln(stderr, outcome)
		} else {
			fmt.Fprintln(stderr, "hush:", err)
		}
		return 1
	}
	o, err := parse(args)
	if err != nil {
		return report(err)
	}
	if o.help {
		_, err := io.WriteString(out, usage)
		return report(err)
	}
	if o.command == localAuth || o.command == localTrust {
		var err error
		if o.command == localTrust {
			err = runTrust(ctx, o, out, stderr)
		} else {
			err = runAuth(ctx, o, in, out, stderr)
		}
		var exited *exec.ExitError
		if errors.As(err, &exited) && (exited.ExitCode() == 1 || exited.ExitCode() == 130) {
			return exited.ExitCode() // The local bundle already wrote the redacted diagnostic.
		}
		return report(err)
	}
	needPi := o.profile || o.command == piJSON || o.command == piRPC || o.command == piModels
	profile, err := backendstart.SelectProfile(o.ui.Profile, needPi)
	if err != nil {
		return report(err)
	}
	o.ui.Profile = string(profile)
	inputTTY := (o.command == printAnswer || o.command == piJSON) && term.IsTerminal(int(in.Fd()))
	if o.interactive(inputTTY, term.IsTerminal(int(out.Fd()))) {
		return report(openUI(ctx, o.ui))
	}
	if o.uiFlags {
		return report(errors.New("--local, --fills, --images and --motion require the interactive TUI"))
	}
	if o.command == printAnswer || o.command == piJSON {
		stop := context.AfterFunc(ctx, func() { _ = in.Close() })
		defer stop()
	}
	// Keep explicitly native options on the original submission/receipt path.
	plain := o.command == printAnswer && !o.noWait && o.key == "" && o.thread == ""
	preferPi := profile == backendstart.Pi && plain && (!o.ephemeral || o.textMode || o.forcePrint || len(o.prompt) > 1)
	target := backendstart.Options{Socket: o.socket, Bundle: o.bundle, Profile: profile}
	if o.command == rawProtocol {
		target.AttachOnly = true
		socket, err := backendstart.Resolve(ctx, target)
		if err != nil {
			return report(err)
		}
		return report(relay(ctx, socket, in, out))
	}
	if o.command == stopBackend {
		stopped, err := backendstart.Stop(ctx, target)
		if err != nil {
			return report(err)
		}
		if o.quiet {
			return 0
		}
		line := "backend is not running"
		if stopped {
			line = "backend stopped"
		}
		return report(writeValue(out, o.json, runtimeStopResult{Stopped: stopped}, line))
	}
	var sdkOpts sdk.ClientOptions
	if preferPi && !needPi {
		sdkOpts.Capabilities = map[string][]uint64{"piEvents": {sdk.PiEventsVersion}}
	}
	if o.command >= listThreads && o.command < localAuth {
		sdkOpts.Capabilities = map[string][]uint64{"threadDirectory": {sdk.ThreadManagementVersion, sdk.ThreadDirectoryVersion}}
	}
	if o.command == scheduleCommand {
		sdkOpts.Capabilities = map[string][]uint64{"schedules": {sdk.SchedulesVersion}}
	}
	if o.command == checkpointCommand {
		sdkOpts.Capabilities = map[string][]uint64{"checkpoints": {sdk.CheckpointsVersion}}
	}
	var client *sdk.Client
	if o.ephemeral {
		node, entry, cleanup, err := backendstart.LocalInvocation(ctx, o.bundle)
		if err != nil {
			return report(err)
		}
		defer func() {
			if err := cleanup(); err != nil {
				code = report(err)
			}
		}()
		host, err := startPrivate(ctx, node, entry, profile, needPi || preferPi)
		if err != nil {
			return report(err)
		}
		defer func() {
			if err := host.Close(); err != nil {
				code = report(err)
			}
		}()
		if profile == backendstart.Hush {
			sdkOpts.RequiredCapabilities = map[string]uint64{"hushProfile": 1}
		}
		client = sdk.NewClient(host.socket, sdkOpts)
	} else {
		client, err = backendstart.NewClient(ctx, target, sdkOpts)
		if err != nil {
			return report(err)
		}
	}
	defer client.Close()

	if preferPi && !needPi {
		hello, err := client.Hello(ctx)
		if err != nil {
			return report(err)
		}
		needPi = hello.Capabilities["piEvents"] == sdk.PiEventsVersion
	}
	if needPi {
		return report(runPi(ctx, client, o, in, out, inputTTY))
	}

	switch o.command {
	case printAnswer:
		text, err := readPrompt(o.prompt, in, inputTTY)
		if err != nil {
			return report(err)
		}
		return report(oneShot(ctx, client, o, text, out))
	case backendStatus:
		return report(status(ctx, client, o, out))
	case waitIdle:
		err := client.WaitIdle(ctx, o.thread)
		if err != nil {
			return report(err)
		}
		return report(writeValue(out, o.json, idleResult{o.thread, true}, fmt.Sprintf("thread %s idle", o.thread)))
	case getSubmission, waitSubmission:
		return report(submissionStatus(ctx, client, o, out))
	case scheduleCommand:
		return report(o.schedule.run(ctx, client, o.json, out))
	case checkpointCommand:
		return report(o.checkpoint.run(ctx, client, o.json, out))
	case listThreads, newThread, forkThread, archiveThread, unarchiveThread:
		return report(threadCommand(ctx, client, o, out))
	default:
		return report(errors.New("unknown command"))
	}
}

func oneShot(ctx context.Context, c *sdk.Client, o options, text string, out io.Writer) error {
	threadID := o.thread
	if threadID == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("find working directory: %w", err)
		}
		thread, err := c.NewThread(ctx, sdk.NewThread{CWD: cwd})
		if err != nil {
			return fmt.Errorf("create thread: %w", err)
		}
		threadID = thread.ID
	}
	key := o.key
	if key == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return fmt.Errorf("generate submission key: %w", err)
		}
		key = hex.EncodeToString(nonce[:])
	}
	receipt, err := c.Submit(ctx, sdk.Input{ThreadID: threadID, RequestID: key, Text: text})
	if err != nil {
		if o.ephemeral {
			return fmt.Errorf("submit: %w", err)
		}
		return fmt.Errorf("submit on thread %s (request ID %q): %w", threadID, key, err)
	}
	if o.noWait {
		row := admissionResult{threadID, receipt.SubmissionID, key}
		return writeValue(out, o.json, row, fmt.Sprintf(
			"thread %s submission %s request %q", threadID, receipt.SubmissionID, key))
	}
	settled, err := c.WaitSubmission(ctx, threadID, receipt.SubmissionID)
	if err != nil {
		if o.ephemeral {
			return fmt.Errorf("wait for submission: %w", err)
		}
		return fmt.Errorf("wait for thread %s submission %s (request ID %q): %w", threadID, receipt.SubmissionID, key, err)
	}
	if settled.Status == "unanswered" {
		if settled.Reason == "aborted" {
			return errors.New("request aborted")
		}
		return fmt.Errorf("request failed: submission ended unanswered (%q)", settled.Reason)
	}
	answer, err := finalAnswer(ctx, c, settled)
	if err != nil {
		return fmt.Errorf("read settled answer: %w", err)
	}
	switch answer.StopReason {
	case "error", "aborted":
		return fmt.Errorf("request %s", answer.StopReason)
	case "stop", "toolUse", "length":
	default:
		return errors.New("settled answer has no final stop reason")
	}
	for _, block := range answer.Blocks {
		if block.Type == "text" {
			if _, err := fmt.Fprintln(out, block.Text); err != nil {
				return fmt.Errorf("write answer: %w", err)
			}
		}
	}
	if answer.StopReason == "length" {
		return errors.New("response was truncated: the model reached its output token limit")
	}
	return nil
}

// Find the immutable entry named by the receipt, not the latest visible
// assistant message: more work may already have run on the same thread.
func finalAnswer(ctx context.Context, c *sdk.Client, s sdk.Submission) (sdk.Message, error) {
	if s.AnswerEntryID == nil {
		return sdk.Message{}, sdk.ErrProtocol
	}
	lower := s.EntryID
	for {
		// A single immutable entry can be large. Avoid requiring a page of
		// tool and answer entries to fit together in one protocol record.
		page, err := c.History(ctx, s.ThreadID, sdk.After(lower, s.AnswerEntryID), 1)
		if err != nil {
			return sdk.Message{}, err
		}
		if len(page.Entries) == 0 {
			break
		}
		e := page.Entries[0]
		if e.ID == *s.AnswerEntryID {
			if e.ThreadID != s.ThreadID {
				break
			}
			for i := len(e.Messages) - 1; i >= 0; i-- {
				if e.Messages[i].Role == "assistant" {
					return e.Messages[i], nil
				}
			}
			break
		}
		if page.NextCursor == nil {
			break
		}
		lower = &e.ID
	}
	return sdk.Message{}, errors.New("the answer entry is missing a final assistant message")
}

func status(ctx context.Context, c *sdk.Client, o options, out io.Writer) error {
	if o.thread == "" {
		h, err := c.Hello(ctx)
		if err != nil {
			return err
		}
		return writeValue(out, o.json, h, fmt.Sprintf("backend ready (native v%d, epoch %s)", h.Version, h.Epoch))
	}
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	update, open := <-c.Watch(life, o.thread, sdk.Window{Min: 1, Max: 1})
	if !open {
		if err := ctx.Err(); err != nil {
			return err
		}
		return sdk.ErrClosed
	}
	if update.Err != nil {
		return update.Err
	}
	state := update.Snapshot.State
	run := "no active run in snapshot"
	if state.Run != nil {
		run = "run " + string(state.Run.TaskID)
	}
	return writeValue(out, o.json, state, fmt.Sprintf("thread %s: %s, %d queued", o.thread, run, len(state.Queue)))
}

func submissionStatus(ctx context.Context, c *sdk.Client, o options, out io.Writer) error {
	var s sdk.Submission
	var err error
	id := o.submission
	if o.key != "" {
		s, err = c.LookupSubmission(ctx, o.thread, o.key)
		if err != nil {
			return err
		}
		id = s.SubmissionID
	}
	if o.command == waitSubmission {
		s, err = c.WaitSubmission(ctx, o.thread, id)
	} else if o.key == "" {
		s, err = c.GetSubmission(ctx, o.thread, id)
	}
	if err != nil {
		return err
	}
	line := fmt.Sprintf("thread %s submission %s %s", s.ThreadID, s.SubmissionID, s.Status)
	if s.Reason != "" {
		line += " (" + fmt.Sprintf("%q", s.Reason) + ")"
	}
	return writeValue(out, o.json, s, line)
}

type admissionResult struct {
	ThreadID     sdk.ID `json:"threadId"`
	SubmissionID sdk.ID `json:"submissionId"`
	RequestID    string `json:"requestId"`
}

type idleResult struct {
	ThreadID sdk.ID `json:"threadId"`
	Idle     bool   `json:"idle"`
}

type runtimeStopResult struct {
	Stopped bool `json:"stopped"`
}

func writeValue(out io.Writer, asJSON bool, value any, line string) error {
	if asJSON {
		return json.NewEncoder(out).Encode(value)
	}
	_, err := fmt.Fprintln(out, line)
	return err
}
