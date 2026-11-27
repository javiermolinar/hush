package headless

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/javiermolinar/hush/sdk"
	"github.com/spf13/pflag"
)

type reviewOptions struct {
	launch           sdk.ReviewLaunch
	action, handle   string
	model, thinking  string
	briefs, named    []string
	checks, evidence []string
	parallel         bool
}

var reviewFlags = []string{"tracked", "untracked", "brief", "reviewer", "check", "evidence", "parallel", "prompt", "model", "thinking", "worker-timeout-ms"}

func (r *reviewOptions) flags(f *pflag.FlagSet) {
	f.StringArrayVar(&r.launch.Scope.Tracked, "tracked", nil, "")
	f.StringArrayVar(&r.launch.Scope.Untracked, "untracked", nil, "")
	f.StringArrayVar(&r.briefs, "brief", nil, "")
	f.StringArrayVar(&r.named, "reviewer", nil, "")
	f.StringArrayVar(&r.checks, "check", nil, "")
	f.StringArrayVar(&r.evidence, "evidence", nil, "")
	f.BoolVar(&r.parallel, "parallel", false, "")
	f.StringVar(&r.launch.Prompt, "prompt", "", "")
	f.StringVar(&r.model, "model", "", "")
	f.StringVar(&r.thinking, "thinking", "off", "")
	f.IntVar(&r.launch.WorkerTimeoutMS, "worker-timeout-ms", 0, "")
}

func (r *reviewOptions) parse(f *pflag.FlagSet, thread sdk.ID, key string, args []string) error {
	if !thread.Valid() || len(args) == 0 || !slices.Contains([]string{"start", "status", "stop", "transcript"}, args[0]) {
		return errors.New("use review start, status, stop, or transcript with --thread ID")
	}
	r.action, r.launch.ThreadID, r.launch.RequestID = args[0], thread, key
	var bad string
	f.Visit(func(flag *pflag.Flag) {
		name := flag.Name
		if !slices.Contains([]string{"thread", "socket", "backend", "profile", "json"}, name) &&
			!(name == "pane-epoch" && r.action == "transcript") && !(name == "request-id" && (r.action == "start" || r.action == "status")) && !(r.action == "start" && slices.Contains(reviewFlags, name)) {
			bad = name
		}
	})
	if bad != "" {
		return fmt.Errorf("--%s is not a review %s option", bad, r.action)
	}
	if r.action == "stop" || r.action == "transcript" {
		if len(args) != 2 || len(args[1]) != 32 || strings.Trim(args[1], "0123456789abcdef") != "" {
			return fmt.Errorf("use review %s HANDLE --thread ID", r.action)
		}
		r.handle = args[1]
		return nil
	}
	if len(args) != 1 || key == "" {
		return errors.New("review start and status require --request-id KEY")
	}
	if r.action == "status" {
		return nil
	}
	if n := len(r.briefs) + len(r.named); n < 1 || n > 2 || len(r.briefs) > 0 && len(r.named) > 0 ||
		len(r.launch.Scope.Tracked)+len(r.launch.Scope.Untracked) == 0 {
		return errors.New("review start requires one or two --brief or --reviewer values and exact --tracked or --untracked paths")
	}
	for i, brief := range r.briefs {
		if strings.TrimSpace(brief) == "" {
			return errors.New("review briefs must not be empty")
		}
		r.launch.Assignments = append(r.launch.Assignments, sdk.ReviewAssignment{Name: fmt.Sprintf("Reviewer %d", i+1), Brief: brief})
	}
	for _, assignment := range r.named {
		name, brief, ok := strings.Cut(assignment, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(brief) == "" {
			return errors.New("use --reviewer NAME=BRIEF")
		}
		r.launch.Assignments = append(r.launch.Assignments, sdk.ReviewAssignment{Name: name, Brief: brief})
	}
	if len(r.checks)+len(r.evidence) > 0 {
		a := &sdk.ReviewAssessment{}
		for _, check := range r.checks {
			id, description, ok := strings.Cut(check, "=")
			if !ok || id == "" || strings.TrimSpace(description) == "" {
				return errors.New("use --check ID=DESCRIPTION")
			}
			a.Checks = append(a.Checks, sdk.ReviewCheck{ID: id, Description: description})
		}
		for _, evidence := range r.evidence {
			key, text, eq := strings.Cut(evidence, "=")
			id, label, colon := strings.Cut(key, ":")
			if !eq || !colon || id == "" || strings.TrimSpace(label) == "" || strings.TrimSpace(text) == "" {
				return errors.New("use --evidence ID:LABEL=TEXT")
			}
			a.Evidence = append(a.Evidence, sdk.SuppliedReviewEvidence{ID: id, Label: label, Text: text})
		}
		r.launch.Assessment = a
	}
	if r.parallel {
		r.launch.Mode = "parallel"
	}
	if f.Changed("prompt") && strings.TrimSpace(r.launch.Prompt) == "" {
		return errors.New("--prompt must not be empty")
	}
	if f.Changed("model") {
		provider, model, ok := strings.Cut(r.model, "/")
		if !ok || provider == "" || model == "" || !slices.Contains([]string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}, r.thinking) {
			return errors.New("use --model PROVIDER/ID and an optional --thinking level")
		}
		r.launch.Selection = &sdk.ReviewSelection{Model: sdk.Model{Provider: provider, ModelID: model}, Thinking: r.thinking}
	} else if f.Changed("thinking") {
		return errors.New("--thinking requires an explicit --model for this review")
	}
	if f.Changed("worker-timeout-ms") && (r.launch.WorkerTimeoutMS < 100 || r.launch.WorkerTimeoutMS > 180000) {
		return errors.New("--worker-timeout-ms must be between 100 and 180000")
	}
	return nil
}

func (r reviewOptions) run(ctx context.Context, client *sdk.Client, out io.Writer, json bool) error {
	switch r.action {
	case "start":
		receipt, err := client.StartCodeReview(ctx, r.launch)
		if err != nil {
			return err
		}
		line := "Review " + receipt.RequestID + " reserved (" + receipt.Mode + ")"
		if receipt.SubmissionID != "" {
			line += " · parent submission " + string(receipt.SubmissionID)
		}
		for _, worker := range receipt.Workers {
			line += "\n" + strconv.QuoteToGraphic(worker.Name) + "  " + worker.Handle
		}
		return writeValue(out, json, receipt, line)
	case "status":
		view, err := client.CodeReview(ctx, r.launch.ThreadID, r.launch.RequestID)
		if err != nil {
			return err
		}
		line := "Review " + view.Receipt.RequestID + " · snapshot " + view.Receipt.SnapshotID + " · " + view.Target
		for _, worker := range view.Workers {
			line += "\n" + reviewWorkerLine(worker)
		}
		return writeValue(out, json, view, line)
	case "transcript":
		view, err := client.InspectReviewWorker(ctx, r.launch.ThreadID, r.handle, true)
		if err != nil {
			return err
		}
		var text strings.Builder
		fmt.Fprintf(&text, "%s · %s · read-only\nSnapshot %s · %s at inspection\n", view.Worker.Name, view.Worker.State, view.Receipt.SnapshotID, view.Target)
		if view.Transcript.Truncated {
			text.WriteString("\nEarlier entries omitted.\n")
		}
		if len(view.Transcript.Entries) == 0 {
			text.WriteString("\nNo public transcript entries.\n")
		}
		for _, entry := range view.Transcript.Entries {
			fmt.Fprintf(&text, "\n%s\n%s\n", entry.Kind, entry.Text)
		}
		if view.Worker.Report != "" {
			fmt.Fprintf(&text, "\nRecorded report · advisory\n%s\n", view.Worker.Report)
		}
		return writeValue(out, json, view, strings.TrimRight(text.String(), "\n"))
	case "stop":
		worker, err := client.StopReviewWorker(ctx, r.launch.ThreadID, r.handle)
		if err != nil {
			return err
		}
		return writeValue(out, json, worker, reviewWorkerLine(worker))
	}
	return sdk.ErrProtocol
}

func reviewWorkerLine(worker sdk.ReviewWorker) string {
	line := strconv.QuoteToGraphic(worker.Name) + "  " + worker.Handle + "  " + strings.ReplaceAll(worker.State, "_", " ")
	if worker.Report != "" {
		return line + " · advisory · " + strconv.QuoteToGraphic(worker.Report)
	}
	if worker.Detail != "" {
		return line + " · " + strconv.QuoteToGraphic(worker.Detail)
	}
	return line
}
