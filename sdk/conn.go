package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type answer struct {
	value any
	err   error
}

type pendingCall struct {
	method string
	reply  func(any) error
	reject func(*ServerError) error
	done   chan answer
}

type session struct {
	wire net.Conn
	done chan struct{}
	gate chan struct{} // serializes writes and deadline changes

	mu      sync.Mutex
	cause   error
	next    uint64
	pending map[string]pendingCall

	hello    Hello // set before publishing the session
	verified bool  // guarded by mu; events are forbidden during the handshake
	event    func(map[string]any) error
}

func (c *Client) dial(ctx, lifetime context.Context, offered map[string][]uint64, event func(map[string]any) error) (_ *session, err error) {
	if c.life.Err() != nil {
		return nil, ErrClosed
	}
	if c.changedStore() {
		return nil, ErrStoreChanged
	}
	dialCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	defer func() { stop(); cancel() }()
	var conn net.Conn
	if c.dialUnix == nil {
		conn, err = (&net.Dialer{}).DialContext(dialCtx, "unix", c.path)
	} else {
		conn, err = c.dialUnix(dialCtx, c.path)
	}
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, &TransportError{Err: err}
	}
	if conn == nil {
		return nil, ErrProtocol
	}
	s := &session{wire: conn, done: make(chan struct{}), gate: make(chan struct{}, 1),
		pending: make(map[string]pendingCall), event: event}
	release := context.AfterFunc(lifetime, func() { s.fail(ErrClosed) })
	go func() { s.read(); release(); c.untrack(s) }()
	defer func() {
		if err != nil {
			s.fail(err)
			var loss *TransportError
			if errors.As(err, &loss) {
				err = &TransportError{Err: loss.Err} // only read-only negotiation was sent
			}
		}
	}()
	offer := make(map[string][]uint64, len(offered)+len(c.required)+1)
	maps.Copy(offer, offered)
	for name, version := range c.required {
		offer[name] = []uint64{version}
	}
	offer[storeIdentityCap] = []uint64{StoreIdentityVersion}
	p := struct {
		Versions     []int               `json:"versions"`
		Capabilities map[string][]uint64 `json:"capabilities,omitempty"`
	}{[]int{1}, offer}
	v, err := s.call(dialCtx, "hello", p, func(v any) error {
		h, err := decode[Hello](v)
		if err != nil {
			return err
		}
		for name, version := range h.Capabilities {
			found := false
			for _, candidate := range offer[name] {
				found = found || version == candidate
			}
			if !found {
				return ErrProtocol
			}
		}
		s.mu.Lock()
		s.hello = h
		s.mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	h := s.hello
	proof := storeProof{epoch: h.Epoch}
	if h.Capabilities[storeIdentityCap] == StoreIdentityVersion {
		v, err = s.call(dialCtx, storeIdentityGet, nil, nil)
		if err != nil {
			return nil, err
		}
		id, err := parseStoreIdentity(v)
		if err != nil {
			return nil, err
		}
		proof = storeProof{identity: id}
	}
	if err := c.verifyStore(dialCtx, lifetime, s, h, proof); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *session) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cause != nil {
		return
	}
	s.cause = err
	close(s.done)
	_ = s.wire.Close()
}

func (s *session) lost(indeterminate bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &TransportError{Err: s.cause, Indeterminate: indeterminate}
}

func (s *session) read() {
	r := bufio.NewReaderSize(s.wire, 32*1024)
	for {
		data, err := readRecord(r)
		if err != nil {
			s.fail(err)
			return
		}
		v, err := parseJSON(data)
		if err == nil {
			if f, ok := v.(map[string]any); ok {
				err = s.receive(f)
			} else {
				err = ErrProtocol
			}
		}
		if err != nil {
			s.fail(err)
			return
		}
	}
}

func (s *session) receive(f map[string]any) error {
	typ, _ := f["type"].(string)
	if typ == "patch" || typ == "state_end" {
		s.mu.Lock()
		verified := s.verified
		s.mu.Unlock()
		if !verified || s.event == nil {
			return ErrProtocol
		}
		return s.event(f)
	}
	if typ != "response" {
		return ErrProtocol
	}
	ok, present := f["ok"].(bool)
	if !present {
		return ErrProtocol
	}
	schema := "failureReply"
	if ok {
		schema = "success"
	}
	method, _ := f["method"].(string)
	storeReply := strings.HasPrefix(method, "ext.storeIdentity.")
	if storeReply {
		s.mu.Lock()
		version := s.hello.Capabilities[storeIdentityCap]
		s.mu.Unlock()
		if err := storeEnvelope(f, method, version, ok); err != nil {
			return err
		}
	} else if ok && (method == introGet || method == introRename || method == introRetry || method == introDismiss) {
		s.mu.Lock()
		version := s.hello.Capabilities["intro"]
		s.mu.Unlock()
		key := "successEnvelope"
		if version == IntroActionsVersion {
			key = "successEnvelopeV2"
		}
		if err := intros()[key].Validate(f); err != nil {
			return fmt.Errorf("%w: intro schema", ErrProtocol)
		}
	} else if ok && (method == threadDirectoryList || method == threadDirectoryGet || method == threadDirectoryNew || method == threadDirectoryArchive) {
		s.mu.Lock()
		version, workspace := s.hello.Capabilities["threadDirectory"], s.hello.Capabilities["threadWorkspace"]
		s.mu.Unlock()
		key := "successEnvelope"
		if version == ThreadManagementVersion {
			key = "successEnvelopeV2"
			if method == threadDirectoryList {
				switch workspace {
				case ThreadWorkspaceSearchVersion:
					key = "successWorkspaceSearch"
				case ThreadWorkspaceVersion:
					if result, ok := f["result"].(map[string]any); ok {
						if _, hasScope := result["workspaceMatches"]; hasScope {
							key = "successWorkspace"
						}
					}
				}
			}
		} else if version != ThreadDirectoryVersion || method != threadDirectoryList {
			return fmt.Errorf("%w: thread directory negotiation", ErrProtocol)
		}
		if err := threadDirectoryWire()[key].Validate(f); err != nil {
			return fmt.Errorf("%w: thread directory schema", ErrProtocol)
		}
	} else if ok && (method == imageSubmit || method == imageGet) {
		s.mu.Lock()
		version := s.hello.Capabilities["images"]
		s.mu.Unlock()
		if version != ImagesVersion || imagesWire()["successEnvelope"].Validate(f) != nil {
			return fmt.Errorf("%w: images schema or negotiation", ErrProtocol)
		}
	} else if ok && (method == planRun || method == planAttach || method == planWait) {
		s.mu.Lock()
		version := s.hello.Capabilities[planCapability]
		s.mu.Unlock()
		if err := optionalEnvelope(f, version, PlanVersion); err != nil {
			return err
		}
	} else if ok && (method == planWorkspaceWait || method == planWorkspaceInspect || method == planWorkspaceHistory || method == planWorkspaceClose || method == planWorkspaceUse) {
		s.mu.Lock()
		version := s.hello.Capabilities[planWorkspace]
		s.mu.Unlock()
		if err := optionalEnvelope(f, version, PlanWorkspaceVersion); err != nil {
			return err
		}
	} else if ok && (method == planReviewWait || method == planReviewStart || method == planReviewCancel) {
		s.mu.Lock()
		version := s.hello.Capabilities[planReviewCapability]
		s.mu.Unlock()
		if err := optionalEnvelope(f, version, PlanReviewVersion); err != nil {
			return err
		}
	} else if ok && method == loadedContextGet {
		s.mu.Lock()
		version := s.hello.Capabilities[loadedContextCapability]
		s.mu.Unlock()
		if version != LoadedContextVersion && version != LoadedContextAgentsVersion {
			return ErrProtocol
		}
		if err := optionalEnvelope(f, version, version); err != nil {
			return err
		}
	} else if ok && (method == workspaceSkillsList || method == workspaceSkillsSubmit) {
		s.mu.Lock()
		version := s.hello.Capabilities[workspaceSkillsCapability]
		s.mu.Unlock()
		if !supportedWorkspaceSkills(version) {
			return ErrProtocol
		}
		if err := optionalEnvelope(f, version, version); err != nil {
			return err
		}
		if method == workspaceSkillsSubmit {
			if err := schemaCheck("success", map[string]any{
				"type": f["type"], "id": f["id"], "method": "submit", "ok": true, "result": f["result"],
			}); err != nil {
				return err
			}
		}
	} else if ok && method == environmentSubmit {
		s.mu.Lock()
		version := s.hello.Capabilities[clientEnvironmentCapability]
		s.mu.Unlock()
		if version != ClientEnvironmentVersion || len(f) != 5 || !keys(f, "type", "id", "method", "ok", "result") {
			return fmt.Errorf("%w: client environment envelope or negotiation", ErrProtocol)
		}
		// The extension returns the exact core submit receipt under its own method.
		if err := schemaCheck("success", map[string]any{
			"type": f["type"], "id": f["id"], "method": "submit", "ok": true, "result": f["result"],
		}); err != nil {
			return err
		}
	} else if ok && (method == gitDiffWaitMethod || method == workspaceScopeWaitMethod) {
		capability, expected := gitDiffWatchCapability, uint64(GitDiffWatchVersion)
		if method == workspaceScopeWaitMethod {
			capability, expected = workspaceScopeWatchCapability, WorkspaceScopeWatchVersion
		}
		s.mu.Lock()
		version := s.hello.Capabilities[capability]
		s.mu.Unlock()
		if err := optionalEnvelope(f, version, expected); err != nil {
			return err
		}
	} else if ok && (method == workspaceFilesPrefix+"scope" || method == workspaceFilesPrefix+"list" || method == workspaceFileSearchMethod) {
		s.mu.Lock()
		version := s.hello.Capabilities[workspaceFilesCapability]
		s.mu.Unlock()
		if version != WorkspaceFileProjectPathsVersion && version != WorkspaceFileSearchVersion &&
			(version != WorkspaceFilesVersion || method == workspaceFileSearchMethod) {
			return ErrProtocol
		}
		if err := optionalEnvelope(f, version, version); err != nil {
			return err
		}
	} else if ok && (method == gitDiffPrefix+"summary" || method == gitDiffPrefix+"file") {
		s.mu.Lock()
		version := s.hello.Capabilities[gitDiffCapability]
		s.mu.Unlock()
		if err := optionalEnvelope(f, version, GitDiffVersion); err != nil {
			return err
		}
	} else if ok && strings.HasPrefix(method, checkpointPrefix) {
		s.mu.Lock()
		version := s.hello.Capabilities[checkpointCapability]
		s.mu.Unlock()
		if err := optionalEnvelope(f, version, CheckpointsVersion); err != nil {
			return err
		}
	} else if ok && strings.HasPrefix(method, schedulesPrefix) {
		s.mu.Lock()
		version := s.hello.Capabilities[schedulesCapability]
		s.mu.Unlock()
		if err := optionalEnvelope(f, version, SchedulesVersion); err != nil {
			return err
		}
	} else if ok && (method == settingsGetMethod || method == settingsSetMethod) {
		s.mu.Lock()
		version := s.hello.Capabilities[settingsCapability]
		s.mu.Unlock()
		if err := optionalEnvelope(f, version, SettingsVersion); err != nil {
			return err
		}
	} else if ok && method == runtimeStopMethod {
		s.mu.Lock()
		version := s.hello.Capabilities[runtimeCapability]
		s.mu.Unlock()
		if version != RuntimeVersion || len(f) != 5 || !keys(f, "type", "id", "method", "ok", "result") {
			return fmt.Errorf("%w: runtime stop envelope or negotiation", ErrProtocol)
		}
		if id, ok := f["id"].(string); !ok || !token(id) {
			return ErrProtocol
		}
		if result, ok := f["result"].(map[string]any); !ok || len(result) != 0 {
			return fmt.Errorf("%w: runtime stop result", ErrProtocol)
		}
	} else if ok && method == nativeStopMethod {
		s.mu.Lock()
		version := s.hello.Capabilities["nativeStop"]
		s.mu.Unlock()
		if version != NativeStopVersion || len(f) != 5 || !keys(f, "type", "id", "method", "ok", "result") {
			return fmt.Errorf("%w: native stop envelope or negotiation", ErrProtocol)
		}
		if id, ok := f["id"].(string); !ok || !token(id) {
			return ErrProtocol
		}
	} else if ok && (method == modelCatalogList || method == modelCatalogNew) {
		s.mu.Lock()
		version := s.hello.Capabilities[modelCatalogCapability]
		s.mu.Unlock()
		if !validModelCatalogVersion(version) || method == modelCatalogNew && version != ModelCatalogDraftVersion ||
			len(f) != 5 || !keys(f, "type", "id", "method", "ok", "result") {
			return fmt.Errorf("%w: model catalog envelope or negotiation", ErrProtocol)
		}
		if id, ok := f["id"].(string); !ok || !token(id) {
			return ErrProtocol
		}
		if method == modelCatalogNew {
			// Creation returns the core thread.new result under its own method.
			if err := schemaCheck("success", map[string]any{
				"type": f["type"], "id": f["id"], "method": "thread.new", "ok": true, "result": f["result"],
			}); err != nil {
				return err
			}
		}
	} else if err := schemaCheck(schema, f); err != nil {
		return err
	}
	id := f["id"].(string)
	s.mu.Lock()
	call, pending := s.pending[id]
	issued := s.next
	if pending {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !pending {
		if storeReply {
			return ErrProtocol
		}
		// A caller can cancel a wait without cancelling admitted work.
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil || n == 0 || n > issued || strconv.FormatUint(n, 10) != id {
			return ErrProtocol
		}
		return nil
	}
	if f["method"] != call.method {
		return ErrProtocol
	}
	if !ok {
		out, err := decode[ServerError](f["error"])
		if err != nil {
			return err
		}
		if call.reject != nil {
			if err := call.reject(&out); err != nil {
				return err
			}
		}
		call.done <- answer{err: &out}
		return nil
	}
	v := f["result"]
	if call.reply != nil {
		if err := call.reply(v); err != nil {
			return err
		}
	}
	call.done <- answer{value: v}
	return nil
}

func (s *session) send(ctx context.Context, method string, params any, receive func(any) error, reject func(*ServerError) error) (uint64, chan answer, error) {
	if params == nil {
		params = struct{}{}
	}
	paramBytes, err := json.Marshal(params)
	if err != nil {
		return 0, nil, err
	}
	p, err := parseJSON(paramBytes)
	if err != nil {
		return 0, nil, err
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-s.done:
		return 0, nil, s.lost(false)
	}
	defer func() { <-s.gate }()
	return s.write(ctx, method, p, receive, reject)
}

func (s *session) call(ctx context.Context, method string, params any, receive func(any) error) (any, error) {
	n, waiter, err := s.send(ctx, method, params, receive, nil)
	if err != nil {
		return nil, err
	}
	return s.await(ctx, n, waiter)
}

func (s *session) await(ctx context.Context, n uint64, waiter chan answer) (any, error) {
	id := strconv.FormatUint(n, 10)
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	select {
	case out := <-waiter:
		return out.value, out.err
	case <-ctx.Done():
		select {
		case out := <-waiter:
			return out.value, out.err
		default:
		}
		return nil, &TransportError{Err: ctx.Err(), Indeterminate: true}
	case <-s.done:
		select {
		case out := <-waiter:
			return out.value, out.err
		default:
		}
		return nil, s.lost(true)
	}
}

func (s *session) write(ctx context.Context, method string, p any, receive func(any) error, reject func(*ServerError) error) (uint64, chan answer, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	s.mu.Lock()
	if s.cause != nil {
		err := s.cause
		s.mu.Unlock()
		return 0, nil, &TransportError{Err: err}
	}
	n := s.next + 1
	s.mu.Unlock()
	id := strconv.FormatUint(n, 10)
	f := map[string]any{"type": "request", "id": id, "method": method, "params": p}
	if err := schemaCheck("request", f); err != nil {
		return 0, nil, err
	}
	bytes, err := json.Marshal(f)
	if err != nil {
		return 0, nil, err
	}
	if len(bytes) > MaxRecord {
		return 0, nil, fmt.Errorf("%w: record too large", ErrProtocol)
	}
	waiter := make(chan answer, 1)
	s.mu.Lock()
	s.next, s.pending[id] = n, pendingCall{method: method, done: waiter, reply: receive, reject: reject}
	s.mu.Unlock()

	// SetWriteDeadline and its cancellation callback are serialized with every other writer.
	if at, ok := ctx.Deadline(); ok {
		_ = s.wire.SetWriteDeadline(at)
	}
	signalled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = s.wire.SetWriteDeadline(time.Now()); close(signalled) })
	written, err := s.wire.Write(append(bytes, '\n'))
	if !stop() {
		<-signalled
	}
	_ = s.wire.SetWriteDeadline(time.Time{})
	if err == nil && written != len(bytes)+1 {
		err = io.ErrShortWrite
	}
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			err = cause
		}
		s.fail(err)
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return 0, nil, &TransportError{Err: err, Indeterminate: written > 0}
	}
	return n, waiter, nil
}
