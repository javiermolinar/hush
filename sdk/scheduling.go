package sdk

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const SchedulesVersion = 1
const schedulesCapability = "schedules"
const schedulesPrefix = "ext.schedules."

var scheduleID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var scheduleOffset = regexp.MustCompile(`^[+-]([01][0-9]|2[0-3]):[0-5][0-9]$`)

type ScheduleTime struct {
	DueMs int64  `json:"dueMs"`
	Zone  string `json:"zone"` // UTC or an explicit RFC3339 offset
}

func (t ScheduleTime) valid() bool {
	return t.DueMs > 0 && t.DueMs <= 253402300799999 && (t.Zone == "UTC" || scheduleOffset.MatchString(t.Zone))
}

func ParseScheduleTime(value string) (ScheduleTime, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err == nil && t.Nanosecond()%int(time.Millisecond) == 0 && len(value) >= len("2006-01-02T15:04:05Z") {
		zone := "UTC"
		if !strings.HasSuffix(value, "Z") {
			zone = value[len(value)-6:]
		}
		when := ScheduleTime{DueMs: t.UnixMilli(), Zone: zone}
		if when.valid() {
			return when, nil
		}
	}
	return ScheduleTime{}, fmt.Errorf("use an RFC3339 time with an explicit zone, e.g. 2027-01-02T13:00:00+01:00 (up to milliseconds)")
}

func (t ScheduleTime) String() string {
	where := time.UTC
	if t.Zone != "UTC" && scheduleOffset.MatchString(t.Zone) {
		h, _ := strconv.Atoi(t.Zone[1:3])
		m, _ := strconv.Atoi(t.Zone[4:6])
		sign := 1
		if t.Zone[0] == '-' {
			sign = -1
		}
		where = time.FixedZone(t.Zone, sign*(h*3600+m*60))
	}
	when := time.UnixMilli(t.DueMs).In(where)
	if where == time.UTC {
		return when.Format(time.RFC3339Nano)
	}
	return when.Format("2006-01-02T15:04:05.999999999") + t.Zone
}

type SchedulePrerequisite struct {
	ThreadID     ID `json:"threadId"`
	SubmissionID ID `json:"submissionId"`
}

func (d *SchedulePrerequisite) valid() bool {
	return d == nil || d.ThreadID.Valid() && d.SubmissionID.Valid()
}

func ParseSchedulePrerequisite(value string) (*SchedulePrerequisite, error) {
	if value == "" {
		return nil, nil
	}
	a, b, ok := strings.Cut(value, "/")
	v := &SchedulePrerequisite{ID(a), ID(b)}
	if !ok || !v.valid() {
		return nil, fmt.Errorf("use THREAD/SUBMISSION for the prerequisite")
	}
	return v, nil
}

type ScheduleInput struct {
	ScheduleTime
	ThreadID       ID                    `json:"threadId"`
	TargetThreadID ID                    `json:"targetThreadId"`
	Text           string                `json:"text"`
	Prerequisite   *SchedulePrerequisite `json:"prerequisite,omitempty"`
	AllowLate      bool                  `json:"allowLate"`
}

type ScheduleItem struct {
	ScheduleTime
	ID             string                `json:"id"`
	ThreadID       ID                    `json:"threadId"`
	TargetThreadID ID                    `json:"targetThreadId"`
	Prerequisite   *SchedulePrerequisite `json:"prerequisite,omitempty"`
	AllowLate      bool                  `json:"allowLate"`
	State          string                `json:"state"`
	Reason         string                `json:"reason,omitempty"`
	EligibleMs     int64                 `json:"eligibleMs,omitempty"`
	SubmissionID   ID                    `json:"submissionId,omitempty"`
	ReportEntryID  ID                    `json:"reportEntryId,omitempty"`
}

func (r ScheduleItem) valid() bool {
	if !scheduleID.MatchString(r.ID) || !r.ThreadID.Valid() || !r.TargetThreadID.Valid() || !r.ScheduleTime.valid() ||
		!r.Prerequisite.valid() || (r.SubmissionID != "" && !r.SubmissionID.Valid()) ||
		(r.ReportEntryID != "" && !r.ReportEntryID.Valid()) || r.EligibleMs < 0 ||
		r.EligibleMs != 0 && r.EligibleMs < r.DueMs {
		return false
	}
	switch r.State {
	case "pending":
		return r.SubmissionID == "" && r.ReportEntryID == "" && r.Reason == ""
	case "needs_attention":
		return r.SubmissionID == "" && r.ReportEntryID == "" &&
			(r.Reason == "late" || r.Reason == "settlement_time_unknown" || r.Reason == "target_missing" || r.Reason == "target_archived")
	case "admitted":
		return r.SubmissionID.Valid() && r.ReportEntryID == "" && r.Reason == ""
	case "done":
		return r.SubmissionID.Valid() && r.ReportEntryID.Valid() && r.Reason == ""
	case "failed":
		return r.ReportEntryID.Valid() && (r.Reason == "prerequisite_failed" || r.Reason == "target_unanswered" ||
			r.Reason == "answer_missing" || r.Reason == "task_unavailable")
	case "cancelled":
		return r.SubmissionID == "" && r.ReportEntryID.Valid() && r.Reason == "cancelled"
	}
	return false
}

type ScheduleChange struct {
	ThreadID ID
	ID       string
	Op       string // run, reschedule, or cancel
	Time     ScheduleTime
}

func optionalCall[T any](ctx context.Context, c *Client, capability string, version uint64, method string, input any) (T, error) {
	var zero T
	s, err := c.connect(ctx)
	if err != nil {
		return zero, err
	}
	if s.hello.Capabilities[capability] != version {
		return zero, &ServerError{Code: "unsupported_capability", Capability: capability, Message: "Capability was not negotiated"}
	}
	v, err := s.call(ctx, method, input, nil)
	if err != nil {
		return zero, err
	}
	return decode[T](v)
}

func optionalEnvelope(f map[string]any, negotiated, expected uint64) error {
	if negotiated != expected || len(f) != 5 || !keys(f, "type", "id", "method", "ok", "result") {
		return ErrProtocol
	}
	if id, ok := f["id"].(string); !ok || !token(id) {
		return ErrProtocol
	}
	if _, ok := f["result"].(map[string]any); !ok {
		return ErrProtocol
	}
	return nil
}

func (c *Client) ListSchedules(ctx context.Context, thread ID) ([]ScheduleItem, error) {
	if !thread.Valid() {
		return nil, ErrProtocol
	}
	type result struct {
		Items []ScheduleItem `json:"items"`
	}
	out, err := optionalCall[result](ctx, c, schedulesCapability, SchedulesVersion, schedulesPrefix+"list", map[string]ID{"threadId": thread})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(out.Items))
	if out.Items == nil || len(out.Items) > 128 {
		return nil, ErrProtocol
	}
	for _, r := range out.Items {
		if !r.valid() || r.ThreadID != thread || seen[r.ID] {
			return nil, ErrProtocol
		}
		seen[r.ID] = true
	}
	return out.Items, nil
}

func (c *Client) AddSchedule(ctx context.Context, input ScheduleInput) (ScheduleItem, error) {
	if !input.ThreadID.Valid() || !input.TargetThreadID.Valid() || !input.ScheduleTime.valid() || !input.Prerequisite.valid() ||
		strings.TrimSpace(input.Text) == "" || len(input.Text) > 32*1024 || !utf8.ValidString(input.Text) {
		return ScheduleItem{}, ErrProtocol
	}
	r, err := optionalCall[ScheduleItem](ctx, c, schedulesCapability, SchedulesVersion, schedulesPrefix+"add", input)
	if err == nil && (!r.valid() || r.ThreadID != input.ThreadID || r.TargetThreadID != input.TargetThreadID ||
		r.ScheduleTime != input.ScheduleTime || r.AllowLate != input.AllowLate || r.State != "pending" ||
		(input.Prerequisite == nil) != (r.Prerequisite == nil) || r.Prerequisite != nil && *r.Prerequisite != *input.Prerequisite) {
		return ScheduleItem{}, ErrProtocol
	}
	return r, err
}

func (c *Client) ChangeSchedule(ctx context.Context, input ScheduleChange) (ScheduleItem, error) {
	if !input.ThreadID.Valid() || !scheduleID.MatchString(input.ID) {
		return ScheduleItem{}, ErrProtocol
	}
	p := map[string]any{"threadId": input.ThreadID, "id": input.ID}
	switch input.Op {
	case "run", "cancel":
	case "reschedule":
		if !input.Time.valid() {
			return ScheduleItem{}, ErrProtocol
		}
		p["dueMs"], p["zone"] = input.Time.DueMs, input.Time.Zone
	default:
		return ScheduleItem{}, ErrProtocol
	}
	r, err := optionalCall[ScheduleItem](ctx, c, schedulesCapability, SchedulesVersion, schedulesPrefix+input.Op, p)
	if err == nil && (!r.valid() || r.ThreadID != input.ThreadID || r.ID != input.ID) {
		return ScheduleItem{}, ErrProtocol
	}
	return r, err
}
