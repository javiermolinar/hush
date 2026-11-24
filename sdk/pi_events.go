package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const PiEventsVersion = 1

const (
	piOpen    = "ext.piEvents.open"
	piCatalog = "ext.piEvents.catalog"
	piCommand = "ext.piEvents.command"
)

func isPiMethod(method string) bool {
	return method == piOpen || method == piCatalog || method == piCommand
}
func piEnvelope(frame map[string]any) error { return piSchemaCheck("successEnvelope", frame) }

// PiOptions opens a single profile-owned thread. Session is a hush profile ID;
// importing Pi files requires the separate piSessions service.
type PiOptions struct {
	CWD                string   `json:"cwd"`
	ThreadID           ID       `json:"threadId,omitempty"`
	Session            string   `json:"session,omitempty"`
	Continue           bool     `json:"continue,omitempty"`
	Fork               bool     `json:"fork,omitempty"`
	Provider           string   `json:"provider,omitempty"`
	Model              string   `json:"model,omitempty"`
	Thinking           string   `json:"thinking,omitempty"`
	Models             []string `json:"models,omitempty"`
	Name               string   `json:"name,omitempty"`
	SystemPrompt       *string  `json:"systemPrompt,omitempty"`
	AppendSystemPrompt *string  `json:"appendSystemPrompt,omitempty"`
}

type PiHeader struct {
	Type          string `json:"type"`
	Version       uint64 `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

type PiReady struct {
	ThreadID ID       `json:"threadId"`
	Epoch    string   `json:"epoch"`
	Seq      uint64   `json:"seq"`
	Header   PiHeader `json:"header"`
	Services []string `json:"services"`
}

type PiCommand struct {
	Type string
	ID   *string
	raw  json.RawMessage
}

// ParsePiCommand checks the Pi command envelope, not the host's supported
// command set. On invalid fields it returns any usable correlation metadata.
func ParsePiCommand(line []byte) (PiCommand, error) {
	var command PiCommand
	if len(line) > MaxRecord {
		return command, ErrProtocol
	}
	tree, err := parseJSON(line)
	if err != nil {
		return command, err
	}
	if m, ok := tree.(map[string]any); ok {
		if kind, ok := m["type"].(string); ok && utf8.RuneCountInString(kind) <= 160 {
			command.Type = kind
		}
		if id, ok := m["id"].(string); ok && utf8.RuneCountInString(id) <= 4096 {
			command.ID = &id
		}
	}
	if err := piSchemaCheck("command", tree); err != nil {
		return command, err
	}
	command.raw = append(json.RawMessage(nil), line...)
	return command, nil
}

type PiResponse struct {
	Type    string          `json:"type"`
	Command string          `json:"command"`
	ID      *string         `json:"id,omitempty"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type PiEvent struct {
	ThreadID ID              `json:"threadId"`
	Epoch    string          `json:"epoch"`
	Seq      uint64          `json:"seq"`
	Event    json.RawMessage `json:"event"`
	Type     string          `json:"-"`
}

// PiRecord is either a verified event or a correlated response, in wire order.
// A successful thread switch includes its explicit new ready fence.
type PiRecord struct {
	Event    *PiEvent
	Response *PiResponse
	Ready    *PiReady
}

type PiLost struct{ Reason string }

func (e *PiLost) Error() string { return "Pi event stream lost: " + e.Reason }

// A PiStream never reconnects, retries or replays commands. Opening another
// stream is explicit and gets a new epoch, never the missing events.
type PiStream struct {
	wire   *session
	cancel context.CancelFunc
	gate   chan struct{} // order sends, holding through any rebinding reply
	slots  chan struct{} // bounded commands awaiting a response
	queue  *piBuffer
	state  PiReady // reader goroutine only; senders use the published epoch
	next   uint64
	epoch  atomic.Value // string, published by the reader before a binding reply returns
}

// piBuffer keeps a verified prefix without letting an absent consumer
// block the socket reader. Large responses count against a byte budget too.
type piBuffer struct {
	mu    sync.Mutex
	wake  chan struct{}
	rows  [256]piQueued
	start int
	count int
	size  int
}

type piQueued struct {
	value PiRecord
	bytes int
}

func (b *piBuffer) push(rec PiRecord, n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.count == len(b.rows) || b.size+n > MaxRecord*2 {
		return &PiLost{Reason: "consumer overflow"}
	}
	b.size += n
	b.rows[(b.start+b.count)%len(b.rows)] = piQueued{rec, n}
	b.count++
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return nil
}

func (b *piBuffer) pop() (PiRecord, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.count == 0 {
		return PiRecord{}, false
	}
	row := b.rows[b.start]
	b.rows[b.start] = piQueued{}
	b.start = (b.start + 1) % len(b.rows)
	b.count--
	b.size -= row.bytes
	return row.value, true
}

func openFence(value any) (PiReady, error) {
	if err := piSchemaCheck("ready", value); err != nil {
		return PiReady{}, err
	}
	v, err := decode[PiReady](value)
	if err != nil {
		return PiReady{}, err
	}
	if _, err := time.Parse(time.RFC3339Nano, v.Header.Timestamp); err != nil {
		return PiReady{}, ErrProtocol
	}
	return v, nil
}

func (c *Client) piConnection(ctx, life context.Context, recv func(map[string]any) error) (*session, error) {
	if recv == nil {
		recv = func(map[string]any) error { return ErrProtocol }
	}
	s, err := c.dial(ctx, life, map[string][]uint64{"piEvents": {PiEventsVersion}}, recv)
	if err != nil {
		return nil, err
	}
	if s.hello.Capabilities["piEvents"] != PiEventsVersion {
		s.fail(ErrClosed)
		return nil, &ServerError{Code: "unsupported_capability", Message: "Capability was not negotiated", Capability: "piEvents"}
	}
	return s, nil
}

// OpenPi negotiates and waits for the opening ready fence on a dedicated
// connection. No profile command can be sent before it succeeds.
func (c *Client) OpenPi(ctx context.Context, opts PiOptions) (*PiStream, PiReady, error) {
	params, err := json.Marshal(opts)
	if err != nil {
		return nil, PiReady{}, err
	}
	p, err := parseJSON(params)
	if err != nil {
		return nil, PiReady{}, err
	}
	if err = piSchemaCheck("open", p); err != nil {
		return nil, PiReady{}, err
	}
	life, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.life, cancel)
	stream := &PiStream{cancel: func() { stop(); cancel() }, gate: make(chan struct{}, 1), slots: make(chan struct{}, 16),
		queue: &piBuffer{wake: make(chan struct{}, 1)}}
	s, err := c.piConnection(ctx, life, stream.receive)
	if err != nil {
		stream.cancel()
		return nil, PiReady{}, err
	}
	stream.wire = s
	var ready PiReady
	_, err = s.call(ctx, piOpen, opts, func(value any) error {
		v, err := openFence(value)
		if err == nil && opts.ThreadID != "" && !opts.Fork && opts.ThreadID != v.ThreadID {
			return ErrProtocol
		}
		if err == nil {
			v.Services = slices.Clone(v.Services)
			ready = v
			stream.state, stream.next = v, 1
			stream.epoch.Store(v.Epoch)
		}
		return err
	})
	if err != nil {
		stream.Close()
		return nil, PiReady{}, err
	}
	return stream, ready, nil
}

// PiCatalog reads the public model catalog without opening any conversation.
func (c *Client) PiCatalog(ctx context.Context) ([]json.RawMessage, error) {
	life, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.life, cancel)
	defer func() { stop(); cancel() }()
	s, err := c.piConnection(ctx, life, nil)
	if err != nil {
		return nil, err
	}
	defer s.fail(ErrClosed)
	v, err := s.call(ctx, piCatalog, nil, func(v any) error { return piSchemaCheck("catalogResult", v) })
	if err != nil {
		return nil, err
	}
	out, err := decode[struct {
		Models []json.RawMessage `json:"models"`
	}](v)
	return out.Models, err
}

func (p *PiStream) Close() { p.cancel(); p.wire.fail(ErrClosed) }

func (p *PiStream) receive(frame map[string]any) error {
	if frame["type"] == "pi_end" {
		if err := piSchemaCheck("end", frame); err != nil {
			return err
		}
		if frame["threadId"] != string(p.state.ThreadID) || frame["epoch"] != p.state.Epoch {
			return ErrProtocol
		}
		return &PiLost{Reason: frame["reason"].(string)}
	}
	if err := piSchemaCheck("frame", frame); err != nil {
		return err
	}
	seq, ok := asUint(frame["seq"])
	if !ok || p.state.Epoch == "" || frame["threadId"] != string(p.state.ThreadID) || frame["epoch"] != p.state.Epoch || seq != p.next {
		return &PiLost{Reason: "unexpected thread, epoch or sequence"}
	}
	body := frame["event"].(map[string]any)
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	p.next++
	e := &PiEvent{p.state.ThreadID, p.state.Epoch, seq, raw, body["type"].(string)}
	return p.queue.push(PiRecord{Event: e}, len(raw))
}

// Next returns the next verified frame in receive order, or terminal transport
// loss after any buffered verified prefix. Consume it from one goroutine.
func (p *PiStream) Next(ctx context.Context) (PiRecord, error) {
	for {
		if item, ok := p.queue.pop(); ok {
			return item, nil
		}
		select {
		case <-p.queue.wake:
		case <-p.wire.done:
			if item, ok := p.queue.pop(); ok {
				return item, nil
			}
			return PiRecord{}, p.wire.lost(len(p.slots) != 0)
		case <-ctx.Done():
			return PiRecord{}, ctx.Err()
		}
	}
}

func samePiID(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// Send writes one command once. Its response is delivered via Next, not here.
// A rebinding command holds the send lane until the new fence is known.
func (p *PiStream) Send(ctx context.Context, cmd PiCommand) error {
	if len(cmd.raw) == 0 {
		return ErrProtocol
	}
	// Exported fields are convenient for diagnostics, but the parsed wire payload
	// is the authority for response correlation.
	original, err := ParsePiCommand(cmd.raw)
	if err != nil || original.Type != cmd.Type || !samePiID(original.ID, cmd.ID) {
		return ErrProtocol
	}
	select {
	case p.gate <- struct{}{}:
	case <-p.wire.done:
		return p.wire.lost(false)
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.gate }()
	select {
	case p.slots <- struct{}{}:
	case <-p.wire.done:
		return p.wire.lost(false)
	case <-ctx.Done():
		return ctx.Err()
	}
	var released sync.Once
	release := func() { released.Do(func() { <-p.slots }) }
	oldEpoch := p.epoch.Load().(string)
	switches := cmd.Type == "new_session" || cmd.Type == "fork" || cmd.Type == "clone" || cmd.Type == "switch_session"
	params := struct {
		Epoch   string          `json:"epoch"`
		Command json.RawMessage `json:"command"`
	}{oldEpoch, cmd.raw}
	seq, waiter, err := p.wire.send(ctx, piCommand, params, func(v any) error {
		if err := piSchemaCheck("callResult", v); err != nil {
			return err
		}
		m := v.(map[string]any)
		response, err := decode[PiResponse](m["response"])
		if err != nil || response.Command != original.Type || !samePiID(response.ID, original.ID) || p.state.Epoch != oldEpoch {
			return ErrProtocol
		}
		rec := PiRecord{Response: &response}
		ready, rebound := m["ready"]
		if rebound != (switches && response.Success) {
			return ErrProtocol
		}
		if rebound {
			v, err := openFence(ready)
			if err != nil || !switches || !response.Success || v.Epoch == oldEpoch || v.ThreadID == p.state.ThreadID {
				return ErrProtocol
			}
			rec.Ready = &v
			p.state, p.next = v, 1
			p.epoch.Store(v.Epoch)
		}
		size := len(response.Data) + len(response.Error) + len(response.Command) + 64
		if rec.Ready != nil {
			size += len(rec.Ready.Header.CWD) + len(rec.Ready.Header.ParentSession) + 256
		}
		if response.ID != nil {
			size += len(*response.ID)
		}
		if err := p.queue.push(rec, size); err != nil {
			return err
		}
		release()
		return nil
	}, func(serverError *ServerError) error { return serverError })
	if err != nil {
		release()
		return err
	}
	if switches {
		_, err = p.wire.await(ctx, seq, waiter)
		if err != nil {
			var transport *TransportError
			if errors.As(err, &transport) && transport.Indeterminate {
				p.wire.fail(err)
			}
		}
	}
	return err
}

// PiLineReader accepts LF and CRLF only (not Unicode line separators). A
// partial record on EOF is not a command. An oversized record ends the reader.
type PiLineReader struct{ r *bufio.Reader }

func NewPiLineReader(r io.Reader) *PiLineReader {
	return &PiLineReader{bufio.NewReaderSize(r, 32*1024)}
}
func (r *PiLineReader) Read() ([]byte, error) { return readRecord(r.r) }
