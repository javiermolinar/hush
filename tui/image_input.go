package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const pasteImageOp x.LocalOp = "hush-paste-image"
const pasteImageID x.ActionID = coreID + ".paste-image"

type draftImage struct {
	label string
	image sdk.Image
}

func attachmentsSize(items []draftImage) int {
	var size int
	for _, item := range items {
		size += len(item.image.Data)
	}
	return size
}

func (t *threadState) addImages(items []draftImage) bool {
	room := sdk.MaxImageBytes - attachmentsSize(t.attachments)
	all := true
	for _, item := range items {
		n := len(item.image.Data)
		if len(t.attachments) == sdk.MaxImageCount || n > room {
			all = false
			continue
		}
		t.attachments = append(t.attachments, item)
		room -= n
	}
	return all
}

func pathImage(s string) string {
	if !filepath.IsAbs(s) && !strings.HasPrefix(s, "file://") && !strings.HasPrefix(s, "~/") || hasControl(s) {
		return ""
	}
	if strings.HasPrefix(s, "file://") {
		u, err := url.Parse(s)
		if err != nil || u.Host != "" && u.Host != "localhost" || u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		s = u.Path
		if !filepath.IsAbs(s) || hasControl(s) {
			return ""
		}
	}
	switch strings.ToLower(filepath.Ext(s)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return s
	}
	return ""
}

// A drop is either one unquoted path or a shell-escaped list of paths. Never
// inspect file-looking substrings in prose or a partly matched list.
func dropCandidates(text string) [][]string {
	if len(text) > maxPromptBytes {
		return nil
	}
	text = strings.TrimSpace(text)
	var out [][]string
	if s := pathImage(text); s != "" {
		out = append(out, []string{s})
	}
	words, err := editArgs(text)
	if err != nil || len(words) == 0 || len(words) > sdk.MaxImageCount {
		return out
	}
	for i, w := range words {
		words[i] = pathImage(w)
		if words[i] == "" {
			return out
		}
	}
	if len(out) == 0 || len(words) != 1 || out[0][0] != words[0] {
		out = append(out, words)
	}
	return out
}

func readDraftImage(path string, cap int) (draftImage, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return draftImage{}, err
		}
		path = filepath.Join(home, path[2:])
	}
	// Reject devices/FIFOs, including a regular file swapped for a FIFO
	// between stat and open. The loop never reads the image itself.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > int64(cap) {
		return draftImage{}, errors.New("cannot attach this file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return draftImage{}, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return draftImage{}, errors.New("cannot attach this file")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(cap)+1))
	if err != nil || len(data) > cap {
		return draftImage{}, errors.New("image attachment exceeds the byte limit")
	}
	_, format, err := imageConfig(data)
	if err != nil {
		return draftImage{}, err
	}
	return draftImage{filepath.Base(path), sdk.Image{MIMEType: "image/" + format, Data: data}}, nil
}

func (m *Model) pastePrompt(msg tea.PasteMsg) tea.Cmd {
	if !m.images.capable {
		return m.updatePrompt(msg)
	}
	sets := dropCandidates(msg.Content)
	if len(sets) == 0 {
		return m.updatePrompt(msg)
	}
	return m.attachImages(msg.Content, func(ctx context.Context, room, count int) ([]draftImage, error) {
		for _, paths := range sets {
			if len(paths) > count {
				continue
			}
			var images []draftImage
			remaining := room
			for _, path := range paths {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				item, err := readDraftImage(path, remaining)
				if err != nil {
					break
				}
				remaining -= len(item.image.Data)
				images = append(images, item)
			}
			if len(images) == len(paths) {
				return images, nil
			}
		}
		return nil, errors.New("image path could not be attached")
	})
}

func (m *Model) pasteClipboardImage() tea.Cmd {
	if !m.images.capable || runtime.GOOS != "darwin" {
		return nil
	}
	return m.attachImages("", clipboardImages)
}

func (m *Model) attachImages(fallback string, read func(context.Context, int, int) ([]draftImage, error)) tea.Cmd {
	r := m.route(m.active, coreID)
	t := m.threads[r.owner]
	if t.attaching || t.sending {
		return m.notifyRoute(r, "Wait for the pending attachment or send", x.Warning)
	}
	room, count := sdk.MaxImageBytes-attachmentsSize(t.attachments), sdk.MaxImageCount-len(t.attachments)
	if room <= 0 || count <= 0 {
		return m.notifyRoute(r, "Attachment limit reached", x.Warning)
	}
	t.attaching = true
	revision := m.editor.revision
	budget := maxEffects
	return m.startWork(r, func(ctx context.Context) (any, error) { return read(ctx, room, count) }, func(v any, err error, _ *int) tea.Cmd {
		t.attaching = false
		if err == nil {
			t.addImages(v.([]draftImage))
			return nil
		}
		if fallback != "" {
			if m.foreground(r) && m.editor.revision == revision {
				return tea.Batch(m.updatePrompt(tea.PasteMsg{Content: fallback}), m.refreshFilePicker())
			}
			m.restoreText(r.owner, fallback)
			return nil
		}
		return m.notifyRoute(r, "No supported clipboard image within the attachment limit", x.Warning)
	}, &budget)
}

const pasteboardPNG = `ObjC.import('AppKit');
function run(argv) {
  const limit = Number(argv[0]);
  const pb = $.NSPasteboard.generalPasteboard;
  let data = pb.dataForType($.NSPasteboardTypePNG);
  if (data.isNil()) {
    const tiff = pb.dataForType($.NSPasteboardTypeTIFF);
    if (tiff.isNil() || Number(tiff.length) > 32 * 1024 * 1024) return '';
    const bitmap = $.NSBitmapImageRep.imageRepWithData(tiff);
    if (bitmap.isNil()) return '';
    const w = Number(bitmap.pixelsWide), h = Number(bitmap.pixelsHigh);
    if (w < 1 || h < 1 || w > 8192 || h > 8192 || w*h > 24000000) return '';
    data = bitmap.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $({}));
  }
  if (data.isNil() || Number(data.length) > limit) return '';
  return ObjC.unwrap(data.base64EncodedStringWithOptions(0));
}`

func clipboardImages(ctx context.Context, room, _ int) ([]draftImage, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", pasteboardPNG, "--", strconv.Itoa(room))
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	cap := base64.StdEncoding.EncodedLen(room) + 2
	encoded, readErr := io.ReadAll(io.LimitReader(out, int64(cap)+1))
	if len(encoded) > cap {
		cancel()
	}
	if err := cmd.Wait(); err != nil || readErr != nil || len(encoded) > cap {
		return nil, errors.New("clipboard read failed")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(data) > room {
		return nil, errors.New("clipboard image is too large")
	}
	if _, format, err := imageConfig(data); err != nil || format != "png" {
		return nil, errors.New("clipboard has no PNG image")
	}
	return []draftImage{{"clipboard.png", sdk.Image{MIMEType: "image/png", Data: data}}}, nil
}
