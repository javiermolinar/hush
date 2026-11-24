package headless

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/javiermolinar/hush/sdk"
	"github.com/spf13/pflag"
)

type scheduleOptions struct {
	op, id, target, at, after, text string
	allowLate                       bool
	thread                          sdk.ID
	when                            sdk.ScheduleTime
	dep                             *sdk.SchedulePrerequisite
}

var scheduleFlags = []string{"target", "at", "after", "text", "allow-late"}

func (s *scheduleOptions) flags(f *pflag.FlagSet) {
	f.StringVar(&s.target, "target", "", "")
	f.StringVar(&s.at, "at", "", "")
	f.StringVar(&s.after, "after", "", "")
	f.StringVar(&s.text, "text", "", "")
	f.BoolVar(&s.allowLate, "allow-late", false, "")
}

func (*scheduleOptions) haveFlags(f *pflag.FlagSet) bool {
	return slices.ContainsFunc(scheduleFlags, f.Changed)
}

func (s *scheduleOptions) parse(f *pflag.FlagSet, thread sdk.ID, args []string) error {
	if len(args) == 0 {
		return errors.New("use schedule add, list, run, reschedule, or cancel")
	}
	if !thread.Valid() {
		return errors.New("schedule requires --thread")
	}
	s.op, s.thread = args[0], thread
	allowed := []string{"thread", "socket", "backend", "profile", "json"}
	want := 1
	switch s.op {
	case "add":
		allowed = append(allowed, scheduleFlags...)
		if !sdk.ID(s.target).Valid() || s.text == "" || f.Changed("after") && s.after == "" {
			return errors.New("schedule add requires --target, --at, and --text; --after takes THREAD/SUBMISSION")
		}
		var err error
		if s.dep, err = sdk.ParseSchedulePrerequisite(s.after); err != nil {
			return err
		}
	case "reschedule":
		allowed = append(allowed, "at")
		want = 2
	case "run", "cancel":
		want = 2
	case "list":
	default:
		return errors.New("use schedule add, list, run, reschedule, or cancel")
	}
	if len(args) != want {
		return errors.New("schedule run, reschedule and cancel require one occurrence ID; add and list use flags only")
	}
	if want == 2 {
		s.id = args[1]
	}
	var invalid string
	f.Visit(func(v *pflag.Flag) {
		if invalid == "" && !slices.Contains(allowed, v.Name) {
			invalid = v.Name
		}
	})
	if invalid != "" {
		return fmt.Errorf("--%s is not valid for schedule %s", invalid, s.op)
	}
	if s.op == "add" || s.op == "reschedule" {
		var err error
		s.when, err = sdk.ParseScheduleTime(s.at)
		return err
	}
	return nil
}

func (s scheduleOptions) run(ctx context.Context, c *sdk.Client, json bool, out io.Writer) error {
	if s.op == "list" {
		items, err := c.ListSchedules(ctx, s.thread)
		if err != nil {
			return err
		}
		if json {
			return writeValue(out, true, struct {
				Items []sdk.ScheduleItem `json:"items"`
			}{items}, "")
		}
		for _, r := range items {
			if _, err := fmt.Fprintln(out, scheduleLine(r)); err != nil {
				return err
			}
		}
		return nil
	}
	var r sdk.ScheduleItem
	var err error
	if s.op == "add" {
		r, err = c.AddSchedule(ctx, sdk.ScheduleInput{ScheduleTime: s.when, ThreadID: s.thread, TargetThreadID: sdk.ID(s.target),
			Text: s.text, Prerequisite: s.dep, AllowLate: s.allowLate})
	} else {
		r, err = c.ChangeSchedule(ctx, sdk.ScheduleChange{ThreadID: s.thread, ID: s.id, Op: s.op, Time: s.when})
	}
	if err != nil {
		return err
	}
	return writeValue(out, json, r, scheduleLine(r))
}

func scheduleLine(r sdk.ScheduleItem) string {
	line := fmt.Sprintf("%s  %s  %s  target #%s", r.ID, r.State, r.ScheduleTime, r.TargetThreadID)
	if r.Prerequisite != nil {
		line += fmt.Sprintf("  after #%s submission #%s", r.Prerequisite.ThreadID, r.Prerequisite.SubmissionID)
	}
	if r.Reason != "" {
		line += "  " + r.Reason
	}
	if r.SubmissionID.Valid() {
		line += "  admitted submission #" + string(r.SubmissionID)
	}
	if r.ReportEntryID.Valid() {
		line += "  report #" + string(r.ReportEntryID)
	}
	return line
}
