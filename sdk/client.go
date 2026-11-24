package sdk

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"
	"time"
	"unicode/utf8"
)

type ClientOptions struct {
	// Additional wire versions to offer in hello. The SDK always offers storeIdentity:1.
	Capabilities map[string][]uint64
	// RequiredCapabilities are offered and verified on every connection,
	// before any command, subscription, or retry can use the listener.
	// Requiring the standalone hushProfile also requires storeIdentity:1.
	RequiredCapabilities map[string]uint64
	// DialUnix optionally opens each connection, including dedicated watches.
	// It must honor the context and be safe for concurrent use. The SDK owns
	// hello and independently verifies store continuity across these connections.
	DialUnix func(context.Context, string) (net.Conn, error)
}

type Client struct {
	path     string
	offered  map[string][]uint64
	required map[string]uint64
	dialUnix func(context.Context, string) (net.Conn, error)
	life     context.Context
	close    context.CancelFunc
	gate     chan struct{}
	session  *session // guarded by gate

	storeMu      sync.Mutex
	storePin     storeProof
	storeChanged bool
	verified     map[*session]struct{}
}

// NewClient connects lazily. A client is bound to one socket and data store.
// It can be shared across goroutines. Close cancels its transport and state feeds.
func NewClient(socketPath string, options ClientOptions) *Client {
	c := &Client{path: socketPath, required: maps.Clone(options.RequiredCapabilities), dialUnix: options.DialUnix, gate: make(chan struct{}, 1)}
	if c.required["hushProfile"] == 1 {
		if _, explicit := c.required[storeIdentityCap]; !explicit {
			c.required[storeIdentityCap] = StoreIdentityVersion
		}
	}
	c.life, c.close = context.WithCancel(context.Background())
	if len(options.Capabilities) > 0 {
		c.offered = make(map[string][]uint64, len(options.Capabilities))
		for k, v := range options.Capabilities {
			c.offered[k] = slices.Clone(v)
		}
	}
	return c
}

func (c *Client) Close() { c.close() }

func (c *Client) connect(ctx context.Context) (*session, error) {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.life.Done():
		return nil, ErrClosed
	}
	defer func() { <-c.gate }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.life.Err() != nil {
		return nil, ErrClosed
	}
	if c.changedStore() {
		return nil, ErrStoreChanged
	}
	if c.session != nil {
		select {
		case <-c.session.done:
		default:
			return c.session, nil
		}
	}
	s, err := c.dial(ctx, c.life, c.offered, nil)
	if err != nil {
		return nil, err
	}
	c.session = s
	return s, nil
}

func (c *Client) Hello(ctx context.Context) (Hello, error) {
	s, err := c.connect(ctx)
	if err != nil {
		return Hello{}, err
	}
	h := s.hello
	h.Capabilities = maps.Clone(h.Capabilities)
	return h, nil
}

func (c *Client) RequireCapability(ctx context.Context, name string, version uint64) error {
	h, err := c.Hello(ctx)
	if err != nil {
		return err
	}
	if got := h.Capabilities[name]; got != 0 && got == version {
		return nil
	}
	return &ServerError{Code: "unsupported_capability", Message: "Capability was not negotiated", Capability: name}
}

func invoke[T any](ctx context.Context, c *Client, method string, params any) (T, error) {
	var zero T
	s, err := c.connect(ctx)
	if err != nil {
		return zero, err
	}
	v, err := s.call(ctx, method, params, nil)
	if err != nil {
		return zero, err
	}
	return decode[T](v)
}

// A passive wait has its own connection: cancelling it actually releases the server's
// waiter without closing other calls or withdrawing an already-admitted input.
func waitOn[T any](ctx context.Context, c *Client, method string, params any) (T, error) {
	var zero T
	if c.life.Err() != nil {
		return zero, ErrClosed
	}
	life, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.life, cancel)
	defer func() { stop(); cancel() }()
	s, err := c.dial(life, life, c.offered, nil)
	if err != nil {
		return zero, err
	}
	defer s.fail(ErrClosed)
	v, err := s.call(life, method, params, nil)
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if err != nil {
		return zero, err
	}
	return decode[T](v)
}

type NewThread struct {
	CWD   string `json:"cwd,omitempty"`
	Model *Model `json:"model,omitempty"`
}

type ThreadPage struct {
	Threads    []Thread `json:"threads"`
	NextCursor *string  `json:"nextCursor"`
}

func (c *Client) ListThreads(ctx context.Context, limit int, cursor string) (ThreadPage, error) {
	return invoke[ThreadPage](ctx, c, "thread.list", struct {
		Limit  int    `json:"limit"`
		Cursor string `json:"cursor,omitempty"`
	}{limit, cursor})
}

func (c *Client) NewThread(ctx context.Context, options NewThread) (Thread, error) {
	if !utf8.ValidString(options.CWD) || options.Model != nil &&
		(!utf8.ValidString(options.Model.Provider) || !utf8.ValidString(options.Model.ModelID)) {
		return Thread{}, ErrProtocol
	}
	return invoke[Thread](ctx, c, "thread.new", options)
}

type threadParam struct {
	ThreadID ID `json:"threadId"`
}

func (c *Client) Fork(ctx context.Context, thread, at ID) (Thread, error) {
	if !thread.Valid() || !at.Valid() {
		return Thread{}, ErrProtocol
	}
	return invoke[Thread](ctx, c, "thread.fork", struct {
		threadParam
		AtEntryID ID `json:"atEntryId"`
	}{threadParam{thread}, at})
}

// Field models configure's omission, explicit null, and replacement.
type Field[T any] struct {
	present bool
	value   *T
}

func Set[T any](value T) Field[T] { return Field[T]{true, &value} }
func Clear[T any]() Field[T]      { return Field[T]{present: true} }

type Configure struct {
	Model             Field[Model]
	ThinkingLevel     Field[string]
	CompactionEnabled Field[bool]
}

func (c *Client) ConfigureThread(ctx context.Context, thread ID, change Configure) error {
	if !thread.Valid() {
		return ErrProtocol
	}
	fields := make(map[string]any, 3)
	if change.Model.present {
		m := change.Model.value
		if m != nil && (!utf8.ValidString(m.Provider) || !utf8.ValidString(m.ModelID)) {
			return ErrProtocol
		}
		fields["model"] = m
	}
	if change.ThinkingLevel.present {
		fields["thinkingLevel"] = change.ThinkingLevel.value
	}
	if change.CompactionEnabled.present {
		fields["compactionEnabled"] = change.CompactionEnabled.value
	}
	_, err := invoke[struct{}](ctx, c, "thread.configure", map[string]any{"threadId": thread, "change": fields})
	return err
}

func (c *Client) Abort(ctx context.Context, thread ID) error {
	if !thread.Valid() {
		return ErrProtocol
	}
	_, err := invoke[struct{}](ctx, c, "thread.abort", threadParam{thread})
	return err
}

const NativeStopVersion = 1
const StopCandidateLimit = 8
const nativeStopMethod = "ext.nativeStop.stop"

const (
	Withdrawn    = "withdrawn"
	NotWithdrawn = "not_withdrawn"
	StopUnknown  = "unknown"
)

type StopCandidate struct {
	SubmissionID ID     `json:"submissionId"`
	Outcome      string `json:"outcome"`
}

type StopResult struct {
	Stopped    bool            `json:"stopped"`
	Candidates []StopCandidate `json:"candidates"`
}

// Stop attempts to withdraw these thread-scoped inputs before stopping ordinary
// activity. Only Withdrawn proves this call removed a not-yet-placed input.
// A lost reply is not retried; an unnegotiated call performs no work.
func (c *Client) Stop(ctx context.Context, thread ID, ids []ID) (StopResult, error) {
	if !thread.Valid() || len(ids) > StopCandidateLimit {
		return StopResult{}, ErrProtocol
	}
	seen := make(map[ID]bool, len(ids))
	for _, id := range ids {
		if !id.Valid() || seen[id] {
			return StopResult{}, ErrProtocol
		}
		seen[id] = true
	}
	s, err := c.connect(ctx)
	if err != nil {
		return StopResult{}, err
	}
	if s.hello.Capabilities["nativeStop"] != NativeStopVersion {
		return StopResult{}, &ServerError{Code: "unsupported_capability", Message: "Withdrawal evidence was not negotiated", Capability: "nativeStop"}
	}
	if ids == nil {
		ids = []ID{}
	}
	v, err := s.call(ctx, nativeStopMethod, struct {
		threadParam
		SubmissionIDs []ID `json:"submissionIds"`
	}{threadParam{thread}, ids}, nil)
	if err != nil {
		return StopResult{}, err
	}
	result, ok := v.(map[string]any)
	if !ok || len(result) != 2 {
		return StopResult{}, ErrProtocol
	}
	stopped, sOK := result["stopped"].(bool)
	candidates, cOK := result["candidates"].([]any)
	if !sOK || !cOK || len(candidates) != len(ids) {
		return StopResult{}, ErrProtocol
	}
	out := StopResult{Stopped: stopped, Candidates: make([]StopCandidate, 0, len(ids))}
	for i, candidate := range candidates {
		row, ok := candidate.(map[string]any)
		if !ok || len(row) != 2 {
			return StopResult{}, ErrProtocol
		}
		id, _ := row["submissionId"].(string)
		outcome, _ := row["outcome"].(string)
		if ID(id) != ids[i] || !oneOf(outcome, Withdrawn, NotWithdrawn, StopUnknown) {
			return StopResult{}, ErrProtocol
		}
		out.Candidates = append(out.Candidates, StopCandidate{SubmissionID: ID(id), Outcome: outcome})
	}
	return out, nil
}

func (c *Client) WaitIdle(ctx context.Context, thread ID) error {
	if !thread.Valid() {
		return ErrProtocol
	}
	_, err := waitOn[struct{}](ctx, c, "thread.wait", threadParam{thread})
	return err
}

func (c *Client) Compact(ctx context.Context, thread ID, instructions *string) (ID, error) {
	if !thread.Valid() || instructions != nil && !utf8.ValidString(*instructions) {
		return "", ErrProtocol
	}
	r, err := invoke[struct {
		TaskID ID `json:"taskId"`
	}](ctx, c, "thread.compact", struct {
		threadParam
		Instructions *string `json:"instructions,omitempty"`
	}{threadParam{thread}, instructions})
	return r.TaskID, err
}

func (c *Client) Reset(ctx context.Context, thread ID, handoff *string) error {
	if !thread.Valid() || handoff != nil && !utf8.ValidString(*handoff) {
		return ErrProtocol
	}
	_, err := invoke[struct{}](ctx, c, "thread.reset", struct {
		threadParam
		Handoff *string `json:"handoff,omitempty"`
	}{threadParam{thread}, handoff})
	return err
}

type BusyMode string

const (
	RejectBusy BusyMode = "reject"
	Steer      BusyMode = "steer"
	FollowUp   BusyMode = "followUp"
)

type Input struct {
	ThreadID    ID                `json:"threadId"`
	RequestID   string            `json:"requestId"`
	Text        string            `json:"text"`
	WhenBusy    BusyMode          `json:"whenBusy"`
	Images      []Image           `json:"images,omitempty"` // requires images:1; bytes, never paths
	Environment ClientEnvironment `json:"-"`                // optional; sent only through clientEnvironment:1
}

// Submit makes at most one automatic retry after a connection loss and only if
// store continuity is verified. It uses the same thread, key and payload. The
// receipt describes admission only.
func (c *Client) Submit(ctx context.Context, input Input) (Receipt, error) {
	if !input.ThreadID.Valid() || input.RequestID == "" || !utf8.ValidString(input.RequestID) || !utf8.ValidString(input.Text) {
		return Receipt{}, ErrProtocol
	}
	if input.WhenBusy == "" {
		input.WhenBusy = RejectBusy
	}
	if len(input.Images) != 0 && !validImageInput(input) {
		return Receipt{}, ErrProtocol
	}
	input.Environment = input.Environment.normalized()
	var method string
	receipt, err := c.submitInput(ctx, input, &method)
	var e *TransportError
	if !retryableTransport(err) || !errors.As(err, &e) || ctx.Err() != nil || c.life.Err() != nil {
		return receipt, err
	}
	if !pause(ctx, c.life, 25*time.Millisecond) {
		return receipt, err
	}
	second, retryErr := c.submitInput(ctx, input, &method)
	if retryErr != nil && e.Indeterminate {
		return Receipt{}, &TransportError{Err: retryErr, Indeterminate: true}
	}
	return second, retryErr
}

func (c *Client) Continue(ctx context.Context, thread ID) (Receipt, error) {
	if !thread.Valid() {
		return Receipt{}, ErrProtocol
	}
	return invoke[Receipt](ctx, c, "continue", threadParam{thread})
}

func checkSubmission(r Submission, err error, thread, id ID, terminal bool) (Submission, error) {
	if err != nil {
		return Submission{}, err
	}
	if r.ThreadID != thread || id != "" && r.SubmissionID != id || terminal && !r.Terminal() {
		return Submission{}, fmt.Errorf("%w: wrong settlement scope", ErrProtocol)
	}
	return r, nil
}

func (c *Client) LookupSubmission(ctx context.Context, thread ID, key string) (Submission, error) {
	if !thread.Valid() || !utf8.ValidString(key) {
		return Submission{}, ErrProtocol
	}
	r, err := invoke[Submission](ctx, c, "submission.get", struct {
		threadParam
		RequestID string `json:"requestId"`
	}{threadParam{thread}, key})
	return checkSubmission(r, err, thread, "", false)
}

func (c *Client) GetSubmission(ctx context.Context, thread, id ID) (Submission, error) {
	return c.submission(ctx, "submission.get", thread, id)
}

func (c *Client) WaitSubmission(ctx context.Context, thread, id ID) (Submission, error) {
	return c.submission(ctx, "submission.wait", thread, id)
}

func (c *Client) submission(ctx context.Context, method string, thread, id ID) (Submission, error) {
	if !thread.Valid() || !id.Valid() {
		return Submission{}, ErrProtocol
	}
	params := struct {
		threadParam
		SubmissionID ID `json:"submissionId"`
	}{threadParam{thread}, id}
	if method == "submission.wait" {
		r, err := waitOn[Submission](ctx, c, method, params)
		return checkSubmission(r, err, thread, id, true)
	}
	r, err := invoke[Submission](ctx, c, method, params)
	return checkSubmission(r, err, thread, id, false)
}

func pause(ctx, life context.Context, delay time.Duration) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-life.Done():
		return false
	case <-t.C:
		return true
	}
}
