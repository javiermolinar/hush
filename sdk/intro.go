package sdk

import (
	"context"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	IntroVersion        = 1 // read-only report
	IntroActionsVersion = 2 // richer report and thread presentation actions
	introGet            = "ext.intro.get"
	introRename         = "ext.intro.rename"
	introRetry          = "ext.intro.retry"
	introDismiss        = "ext.intro.dismiss"
)

// IntroReport is the Hush host's read-only inventory and prompt sizing report.
// Skills are names only; this capability does not invoke them.
type IntroReport struct {
	Revision      uint64           `json:"revision"`
	StartedAtMS   int64            `json:"startedAtMs"` // zero if the runtime start is unknown
	CWD           string           `json:"cwd"`
	Model         *Model           `json:"model,omitempty"`
	Thinking      string           `json:"thinking,omitempty"`
	Sessions      IntroSessions    `json:"sessions"`
	Attention     []IntroAttention `json:"attention"`
	MoreAttention int              `json:"moreAttention"`
	Thread        *IntroThread     `json:"thread,omitempty"`
	Skills        []string         `json:"skills"`
	MoreSkills    int              `json:"moreSkills"`
	Context       *IntroContext    `json:"context,omitempty"`
}

type IntroSessions struct {
	Open    int  `json:"open"`
	Running int  `json:"running"`
	Quiet   int  `json:"quiet"`
	Partial bool `json:"partial"`
}

type IntroAttention struct {
	Title    string         `json:"title"`
	Status   string         `json:"status"`
	Reason   string         `json:"reason,omitempty"`
	UntilMS  *int64         `json:"untilMs,omitempty"`
	ThreadID ID             `json:"threadId,omitempty"`
	Issue    *IntroRunIssue `json:"issue,omitempty"`
}

// IntroThread is presentation metadata for an explicitly selected thread in v2.
type IntroThread struct {
	ID                 ID             `json:"id"`
	Title              string         `json:"title,omitempty"`
	Issue              *IntroRunIssue `json:"issue,omitempty"`
	DismissedFailureID ID             `json:"dismissedFailureId,omitempty"`
}

// IntroRunIssue refers to a settled, actionable failure, not a live run. The
// diagnostic, when present, is a safe projection rather than a provider payload.
type IntroRunIssue struct {
	TaskID      ID     `json:"taskId"`
	Reason      string `json:"reason"`
	Detail      string `json:"detail,omitempty"`
	Retry       bool   `json:"retry"`
	SwitchModel bool   `json:"switchModel"`
	EndedAtMS   *int64 `json:"endedAtMs,omitempty"`
}

type IntroContext struct {
	Window         uint64  `json:"window"`
	RenderedBytes  uint64  `json:"renderedBytes"`
	ReportedTokens *uint64 `json:"reportedTokens,omitempty"`
}

type IntroUpdate struct {
	Report IntroReport
	Err    error
}

// IntroScope selects either an existing thread or a prospective local
// workspace. The zero value selects the backend's main conversation.
type IntroScope struct {
	ThreadID ID     `json:"threadId,omitempty"`
	CWD      string `json:"cwd,omitempty"`
}

func (i IntroScope) valid() bool {
	if i.ThreadID != "" {
		return i.CWD == "" && i.ThreadID.Valid()
	}
	return i.CWD == "" || filepath.IsAbs(i.CWD) && !strings.ContainsRune(i.CWD, 0) && utf8.ValidString(i.CWD) && utf8.RuneCountInString(i.CWD) <= 4096
}

type introQuery struct {
	IntroScope
	After *uint64 `json:"after,omitempty"`
}

func introReport(ctx context.Context, s *session, query introQuery) (IntroReport, error) {
	version := s.hello.Capabilities["intro"]
	if version != IntroVersion && version != IntroActionsVersion {
		return IntroReport{}, introUnsupported()
	}
	result, err := s.call(ctx, introGet, query, nil)
	if err != nil {
		return IntroReport{}, err
	}
	out, err := decode[IntroReport](result)
	if err == nil && (query.After != nil && out.Revision <= *query.After ||
		out.Sessions.Running > out.Sessions.Open || out.Sessions.Quiet > out.Sessions.Open-out.Sessions.Running) {
		err = ErrProtocol
	}
	if err == nil && version == IntroActionsVersion {
		invalidIssue := func(issue *IntroRunIssue) bool { return issue != nil && !issue.TaskID.Valid() }
		if out.Thread == nil && query.ThreadID != "" || out.Thread != nil &&
			(out.Thread.ID != query.ThreadID || invalidIssue(out.Thread.Issue) ||
				out.Thread.DismissedFailureID != "" && !out.Thread.DismissedFailureID.Valid()) {
			return IntroReport{}, ErrProtocol
		}
		seen := make(map[ID]bool, len(out.Attention))
		for _, row := range out.Attention {
			if !row.ThreadID.Valid() || invalidIssue(row.Issue) || seen[row.ThreadID] {
				return IntroReport{}, ErrProtocol
			}
			seen[row.ThreadID] = true
		}
	}
	return out, err
}

// Intro reads an immediate report. The caller must offer intro version 1 or 2.
func (c *Client) Intro(ctx context.Context, scope IntroScope) (IntroReport, error) {
	if !scope.valid() {
		return IntroReport{}, ErrProtocol
	}
	s, err := c.connect(ctx)
	if err != nil {
		return IntroReport{}, err
	}
	return introReport(ctx, s, introQuery{IntroScope: scope})
}

func introUnsupported() *ServerError {
	return &ServerError{Code: "unsupported_capability", Capability: "intro", Message: "Capability was not negotiated"}
}

func introAction[T any](ctx context.Context, c *Client, method string, params any) (T, error) {
	var zero T
	s, err := c.connect(ctx)
	if err != nil {
		return zero, err
	}
	if s.hello.Capabilities["intro"] != IntroActionsVersion {
		return zero, introUnsupported()
	}
	result, err := s.call(ctx, method, params, nil)
	if err != nil {
		return zero, err
	}
	return decode[T](result)
}

// RenameThread sets the Hush presentation name and returns its normalized value.
// It does not change the Pi session name. Requires intro version 2.
func (c *Client) RenameThread(ctx context.Context, thread ID, title string) (string, error) {
	if !thread.Valid() || !utf8.ValidString(title) || strings.TrimSpace(title) == "" || utf8.RuneCountInString(title) > 256 {
		return "", ErrProtocol
	}
	reply, err := introAction[struct{ Title string }](ctx, c, introRename, struct {
		ThreadID ID     `json:"threadId"`
		Title    string `json:"title"`
	}{thread, title})
	return reply.Title, err
}

type failureTarget struct {
	ThreadID ID `json:"threadId"`
	TaskID   ID `json:"taskId"`
}

// RetryFailure continues the specified failed run, if it is still the current
// issue on that thread. The receipt is admission only. Requires intro version 2.
func (c *Client) RetryFailure(ctx context.Context, thread, task ID) (Receipt, error) {
	if !thread.Valid() || !task.Valid() {
		return Receipt{}, ErrProtocol
	}
	r, err := introAction[Receipt](ctx, c, introRetry, failureTarget{thread, task})
	if err == nil && !r.SubmissionID.Valid() {
		err = ErrProtocol
	}
	return r, err
}

// DismissFailure hides the specified failure without changing its settlement.
// It cannot hide later failures on the thread. Requires intro version 2.
func (c *Client) DismissFailure(ctx context.Context, thread, task ID) error {
	if !thread.Valid() || !task.Valid() {
		return ErrProtocol
	}
	_, err := introAction[struct{}](ctx, c, introDismiss, failureTarget{thread, task})
	return err
}

// WatchIntro waits for replacement reports on one dedicated connection, not
// a conversation watch. Cancellation releases the server's passive wait.
// A transport loss starts a fresh snapshot; no revision crosses an epoch.
func (c *Client) WatchIntro(ctx context.Context, scope IntroScope) <-chan IntroUpdate {
	out := make(chan IntroUpdate, 1)
	go func() {
		defer close(out)
		if !scope.valid() {
			out <- IntroUpdate{Err: ErrProtocol}
			return
		}
		life, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(c.life, cancel)
		defer func() { stop(); cancel() }()
		publish := func(v IntroUpdate) {
			select {
			case <-out:
			default:
			}
			out <- v
		}
		for delay := 250 * time.Millisecond; life.Err() == nil; delay = min(delay*2, 4*time.Second) {
			ready, err := c.watchIntro(life, scope, publish)
			if life.Err() != nil {
				return
			}
			publish(IntroUpdate{Err: err})
			if !retryableTransport(err) {
				return
			}
			if ready {
				delay = 250 * time.Millisecond
			}
			if !pause(life, c.life, delay) {
				return
			}
		}
	}()
	return out
}

func (c *Client) watchIntro(life context.Context, scope IntroScope, publish func(IntroUpdate)) (bool, error) {
	ctx, cancel := context.WithTimeout(life, 10*time.Second)
	defer cancel()
	s, err := c.dial(ctx, life, c.offered, nil)
	if err != nil {
		return false, err
	}
	defer s.fail(ErrClosed)
	query := introQuery{IntroScope: scope}
	r, err := introReport(ctx, s, query)
	cancel()
	for err == nil {
		publish(IntroUpdate{Report: r})
		rev := r.Revision
		query.After = &rev
		r, err = introReport(life, s, query)
	}
	return query.After != nil, err
}
