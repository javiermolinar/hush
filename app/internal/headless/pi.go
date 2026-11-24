package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/javiermolinar/hush/sdk"
)

const maxPiInput = 2 * 1024 * 1024

func runPi(ctx context.Context, client *sdk.Client, o options, in io.Reader, out io.Writer, inputTTY bool) error {
	if o.command == piModels {
		return printModels(ctx, client, o.modelFilter, out)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	o.pi.CWD, o.pi.ThreadID = cwd, o.thread
	var prompts []string
	if o.command != piRPC {
		prompts, err = piInputs(o.prompt, o.fileArgs, in, inputTTY, cwd)
		if err != nil {
			return err
		}
	}
	if o.pi.SystemPrompt != nil {
		text, err := promptOverride(*o.pi.SystemPrompt, cwd)
		if err != nil {
			return err
		}
		o.pi.SystemPrompt = &text
	}
	if len(o.appendPrompt) > 0 {
		parts := make([]string, len(o.appendPrompt))
		for i, arg := range o.appendPrompt {
			parts[i], err = promptOverride(arg, cwd)
			if err != nil {
				return err
			}
		}
		text := strings.Join(parts, "\n\n")
		o.pi.AppendSystemPrompt = &text
	}
	stream, ready, err := client.OpenPi(ctx, o.pi)
	if err != nil {
		return fmt.Errorf("open Pi session: %w", err)
	}
	defer stream.Close()
	if o.command == piRPC {
		return runRPC(ctx, stream, in, out)
	}
	return runPiOneShot(ctx, stream, ready, prompts, out, o.command == piJSON)
}

func writeLine(out io.Writer, b []byte) error {
	line := append(b, '\n')
	n, err := out.Write(line)
	if err != nil {
		return fmt.Errorf("write Pi output: %w", err)
	}
	if n != len(line) {
		return io.ErrShortWrite
	}
	return nil
}

func objectLine(out io.Writer, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeLine(out, b)
}

type piAnswerError struct{ diagnostic string }

func (e *piAnswerError) Error() string { return e.diagnostic }

type assistantAnswer struct {
	Role       string `json:"role"`
	StopReason string `json:"stopReason"`
	Diagnostic string `json:"errorMessage"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func runPiOneShot(ctx context.Context, stream *sdk.PiStream, ready sdk.PiReady, prompts []string, out io.Writer, asJSON bool) error {
	if asJSON {
		if err := objectLine(out, ready.Header); err != nil {
			return err
		}
	}
	var answer *assistantAnswer
	for i, prompt := range prompts {
		answer = nil
		line, err := json.Marshal(map[string]string{"type": "prompt", "id": strconv.Itoa(i + 1), "message": prompt})
		if err != nil {
			return err
		}
		cmd, err := sdk.ParsePiCommand(line)
		if err != nil {
			return err
		}
		if err := stream.Send(ctx, cmd); err != nil {
			return err
		}
		var replied, settled bool
		for !replied || !settled {
			row, err := stream.Next(ctx)
			if err != nil {
				return err
			}
			if row.Response != nil {
				if !row.Response.Success {
					return errors.New(row.Response.Error)
				}
				var admission struct{ Disposition string }
				if err := json.Unmarshal(row.Response.Data, &admission); err != nil || admission.Disposition != "started" {
					return errors.New("unexpected Pi one-shot admission")
				}
				replied = true
				continue
			}
			e := row.Event
			switch e.Type {
			case "turn_end":
				var terminal struct{ Message assistantAnswer }
				if err := json.Unmarshal(e.Event, &terminal); err != nil || terminal.Message.Role != "assistant" {
					return errors.New("invalid Pi turn result")
				}
				answer = &terminal.Message
			case "agent_settled":
				if answer == nil {
					return errors.New("Pi run settled without an observed terminal turn")
				}
				settled = true
			}
			if asJSON {
				if err := writeLine(out, e.Event); err != nil {
					return err
				}
			}
		}
	}
	if asJSON || answer == nil {
		return nil
	}
	switch answer.StopReason {
	case "error", "aborted":
		if answer.Diagnostic != "" {
			return &piAnswerError{answer.Diagnostic}
		}
		return &piAnswerError{"Request " + answer.StopReason}
	case "stop", "length", "toolUse":
	default:
		return errors.New("Pi run has no supported final stop reason")
	}
	for _, block := range answer.Content {
		if block.Type == "text" {
			if _, err := fmt.Fprintln(out, block.Text); err != nil {
				return err
			}
		}
	}
	if answer.StopReason == "length" {
		return &piAnswerError{"Response was truncated: the model reached its output token limit."}
	}
	return nil
}

func piInputs(messages, files []string, in io.Reader, tty bool, cwd string) ([]string, error) {
	var first strings.Builder
	if !tty {
		b, err := io.ReadAll(io.LimitReader(in, maxPiInput+1))
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if len(b) > maxPiInput || !utf8.Valid(b) {
			return nil, errors.New("Pi input exceeds the limit or is not UTF-8")
		}
		first.WriteString(strings.TrimSpace(string(b)))
	}
	for _, file := range files {
		if file == "" {
			return nil, errors.New("an @file argument needs a path")
		}
		path, err := localPath(file, cwd)
		if err != nil {
			return nil, err
		}
		text, err := readLocalText(path, maxPiInput-first.Len())
		if err != nil {
			return nil, err
		}
		if text != "" {
			fmt.Fprintf(&first, "<file name=\"%s\">\n%s\n</file>\n", path, text)
			if first.Len() > maxPiInput {
				return nil, errors.New("Pi file input exceeds the limit")
			}
		}
	}
	if len(messages) > 0 {
		first.WriteString(messages[0])
		messages = messages[1:]
	}
	if first.Len() > 0 {
		messages = append([]string{first.String()}, messages...)
	}
	for _, message := range messages {
		if len(message) > maxPiInput || !utf8.ValidString(message) || strings.ContainsRune(message, 0) {
			return nil, errors.New("Pi input exceeds the limit or is not UTF-8 text")
		}
		encoded, _ := json.Marshal(message)
		if len(encoded) > sdk.MaxRecord-1024 {
			return nil, errors.New("Pi input exceeds the wire limit when encoded")
		}
	}
	return messages, nil
}

func localPath(value, cwd string) (string, error) {
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~"))
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	return filepath.Join(cwd, value), nil
}

func readLocalText(path string, limit int) (string, error) {
	// Don't let a FIFO supplied as a file argument block startup.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("read Pi input file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("Pi input file is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(b) > limit {
		return "", errors.New("Pi input file exceeds the limit")
	}
	if strings.HasPrefix(http.DetectContentType(b), "image/") {
		return "", errors.New("unsupported capability: images")
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return "", errors.New("Pi input file is not UTF-8 text")
	}
	return strings.TrimPrefix(string(b), "\uFEFF"), nil
}

func promptOverride(value, cwd string) (string, error) {
	path, err := localPath(value, cwd)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(path); value != "" && err == nil && !info.IsDir() {
		return readLocalText(path, 256*1024)
	}
	if !utf8.ValidString(value) || len(value) > 256*1024 {
		return "", errors.New("invalid system prompt override")
	}
	return value, nil
}

func printModels(ctx context.Context, client *sdk.Client, filter string, out io.Writer) error {
	raw, err := client.PiCatalog(ctx)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		_, err := fmt.Fprintln(out, "No models available.")
		return err
	}
	rows := [][]string{{"provider", "model", "context", "max-out", "thinking", "images"}}
	for _, v := range raw {
		var m struct {
			Provider, ID             string
			ContextWindow, MaxTokens int
			Reasoning                bool
			Input                    []string
		}
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		if filter != "" && filter != "*" && !matchesModel(m.Provider+" "+m.ID, filter) {
			continue
		}
		thinking, images := "no", "no"
		if m.Reasoning {
			thinking = "yes"
		}
		for _, modality := range m.Input {
			if modality == "image" {
				images = "yes"
			}
		}
		rows = append(rows, []string{m.Provider, m.ID, tokenCount(m.ContextWindow), tokenCount(m.MaxTokens), thinking, images})
	}
	if len(rows) == 1 {
		_, err := fmt.Fprintf(out, "No models matching %q\n", filter)
		return err
	}
	items := rows[1:]
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		return a[0] < b[0] || a[0] == b[0] && a[1] < b[1]
	})
	var widths [6]int
	for _, row := range rows {
		for i, value := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(value))
		}
	}
	for _, row := range rows {
		var line strings.Builder
		for i, value := range row {
			if i > 0 {
				line.WriteString("  ")
			}
			line.WriteString(value)
			line.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(value)))
		}
		if _, err := fmt.Fprintln(out, line.String()); err != nil {
			return err
		}
	}
	return nil
}

func matchesModel(value, query string) bool {
	for _, word := range strings.Fields(strings.ToLower(query)) {
		target := strings.ToLower(value)
		for _, r := range word {
			i := strings.IndexRune(target, r)
			if i < 0 {
				return false
			}
			target = target[i+utf8.RuneLen(r):]
		}
	}
	return true
}

func tokenCount(n int) string {
	for _, unit := range []struct {
		count int
		name  string
	}{{1_000_000, "M"}, {1_000, "K"}} {
		if n >= unit.count {
			if n%unit.count == 0 {
				return fmt.Sprintf("%d%s", n/unit.count, unit.name)
			}
			return fmt.Sprintf("%.1f%s", float64(n)/float64(unit.count), unit.name)
		}
	}
	return strconv.Itoa(n)
}
