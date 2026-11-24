package sdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

const MaxRecord = 8 * 1024 * 1024
const maxSafe = 1<<53 - 1

type ID string

func (id ID) Valid() bool {
	if len(id) == 0 || len(id) > 16 || id[0] < '1' || id[0] > '9' {
		return false
	}
	n, err := strconv.ParseUint(string(id), 10, 64)
	return err == nil && n <= maxSafe && string(id) == strconv.FormatUint(n, 10)
}

func (id *ID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if v := ID(s); v.Valid() {
		*id = v
		return nil
	}
	return fmt.Errorf("invalid durable ID")
}

func cmpID(a, b ID) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

type Hello struct {
	Version      int               `json:"version"`
	Epoch        string            `json:"epoch"`
	Capabilities map[string]uint64 `json:"capabilities"`
}

type Model struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

type ForkPoint struct {
	ThreadID  ID `json:"threadId"`
	AtEntryID ID `json:"atEntryId"`
}

type Owner struct {
	ThreadID ID `json:"threadId"`
	TaskID   ID `json:"taskId"`
}

type Thread struct {
	ID     ID         `json:"id"`
	Parent *ForkPoint `json:"parent,omitempty"`
	Owner  *Owner     `json:"owner,omitempty"`
}

type Agent struct {
	Model             *Model `json:"model,omitempty"`
	ThinkingLevel     string `json:"thinkingLevel,omitempty"`
	CWD               string `json:"cwd,omitempty"`
	CompactionEnabled *bool  `json:"compactionEnabled,omitempty"`
}

type Usage struct {
	InputTokens      uint64  `json:"inputTokens"`
	OutputTokens     uint64  `json:"outputTokens"`
	CacheReadTokens  uint64  `json:"cacheReadTokens"`
	CacheWriteTokens uint64  `json:"cacheWriteTokens"`
	TotalTokens      uint64  `json:"totalTokens"`
	CostUSD          float64 `json:"costUsd"`
}

// Block covers text, thinking, toolCall and omitted. Exactly one shape is populated.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Kind      string          `json:"kind,omitempty"`
}

type Message struct {
	Role        string  `json:"role"`
	TimestampMS uint64  `json:"timestampMs"`
	Blocks      []Block `json:"blocks"`
	StopReason  string  `json:"stopReason,omitempty"`
	Error       string  `json:"error,omitempty"`
	CallID      string  `json:"callId,omitempty"`
	Name        string  `json:"name,omitempty"`
	IsError     *bool   `json:"isError,omitempty"`
}

type Entry struct {
	ID          ID        `json:"id"`
	ThreadID    ID        `json:"threadId"`
	Kind        string    `json:"kind"`
	HeadEntryID *ID       `json:"headEntryId,omitempty"`
	Messages    []Message `json:"messages"`
}

type Window struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type TranscriptWindow struct {
	BeforeEntryID *ID     `json:"beforeEntryId"`
	LastEntryID   *ID     `json:"lastEntryId"`
	Entries       []Entry `json:"entries"`
}

type Tool struct {
	CallID        string  `json:"callId"`
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	TaskID        *ID     `json:"taskId,omitempty"`
	ResultEntryID *ID     `json:"resultEntryId,omitempty"`
	Output        *string `json:"output,omitempty"`
	DroppedBytes  *uint64 `json:"droppedBytes,omitempty"`
	DroppedLines  *uint64 `json:"droppedLines,omitempty"`
}

type QueueItem struct {
	SubmissionID ID     `json:"submissionId"`
	Mode         string `json:"mode"`
	Text         string `json:"text"`
}

type Compaction struct {
	TaskID   ID     `json:"taskId"`
	Reason   string `json:"reason"`
	Blocking bool   `json:"blocking"`
	Attempt  uint64 `json:"attempt"`
}

type Failure struct {
	TaskID ID     `json:"taskId"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

type Run struct {
	TaskID        ID   `json:"taskId"`
	SubmissionIDs []ID `json:"submissionIds"`
}

type Retry struct {
	AtMS   uint64 `json:"atMs"`
	Detail string `json:"detail"`
}

type Streaming struct {
	Attempt uint64  `json:"attempt"`
	Blocks  []Block `json:"blocks"`
	Retry   *Retry  `json:"retry,omitempty"`
}

type State struct {
	Thread            Thread           `json:"thread"`
	Agent             Agent            `json:"agent"`
	Usage             Usage            `json:"usage"`
	Window            TranscriptWindow `json:"window"`
	Tools             []Tool           `json:"tools"`
	Queue             []QueueItem      `json:"queue"`
	Compactions       []Compaction     `json:"compactions"`
	CompactionFailure *Failure         `json:"compactionFailure,omitempty"`
	Run               *Run             `json:"run,omitempty"`
	Failure           *Failure         `json:"failure,omitempty"`
	Streaming         *Streaming       `json:"streaming,omitempty"`
}

// Snapshots and entries returned by this package must be treated as read-only.
type Snapshot struct {
	SubscriptionID string `json:"subscriptionId"`
	Epoch          string `json:"epoch"`
	Rev            uint64 `json:"rev"`
	State          State  `json:"state"`
}

type Receipt struct {
	SubmissionID ID `json:"submissionId"`
}

type Submission struct {
	ThreadID      ID     `json:"threadId"`
	SubmissionID  ID     `json:"submissionId"`
	Status        string `json:"status"`
	EntryID       *ID    `json:"entryId,omitempty"`
	AnswerEntryID *ID    `json:"answerEntryId,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

func (s Submission) Terminal() bool { return s.Status == "done" || s.Status == "unanswered" }

type ServerError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Capability string `json:"capability,omitempty"`
}

func (e *ServerError) Error() string {
	if e.Capability != "" {
		return e.Code + " (" + e.Capability + "): " + e.Message
	}
	return e.Code + ": " + e.Message
}

func IsCode(err error, code string) bool {
	var e *ServerError
	return errors.As(err, &e) && e.Code == code
}

type TransportError struct {
	Err error
	// Indeterminate means this call may have reached the server without a usable reply.
	// Do not retry an unkeyed mutation, even if Err is cancellation.
	Indeterminate bool
}

func (e *TransportError) Error() string {
	if e.Indeterminate {
		return "hush transport (outcome indeterminate): " + e.Err.Error()
	}
	return "hush transport: " + e.Err.Error()
}
func (e *TransportError) Unwrap() error { return e.Err }

var (
	ErrClosed            = errors.New("hush client closed")
	ErrProtocol          = errors.New("invalid hush protocol frame")
	ErrStale             = errors.New("hush state needs a fresh snapshot")
	ErrStoreChanged      = errors.New("hush cannot verify store continuity (store replaced, restored, or legacy backend restarted); start a fresh attach without reusing thread state or submission keys")
	ErrStoreIncompatible = errors.New("incompatible or fenced Hush history; no automatic migration")
)

func token(s string) bool {
	if len(s) < 1 || len(s) > 160 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func oneOf(s string, options ...string) bool {
	for _, option := range options {
		if s == option {
			return true
		}
	}
	return false
}
