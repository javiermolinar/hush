package tui

import (
	"bytes"
	"context"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

type imageScreen struct {
	epoch uint64
	up    bool
}

// imageOutput shares ordering with the renderer, not its asynchronous Raw
// buffer. Only completed writes of the alternate-screen sequence open an
// epoch. In-flight work from before an external editor cannot write into the
// main screen or the next alternate screen.
type imageOutput struct {
	*os.File
	mu      sync.Mutex
	screen  imageScreen
	events  chan imageScreen
	painted chan imagePainted // only needed for Kitty's re-entry redraw
	pending map[int]pendingImage
}

type imagePainted struct{ placement imagePlacement }

type pendingImage struct {
	placement imagePlacement
	color     string
	queued    bool
}

func (o *imageOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.File.Write(p)
	p = p[:n]
	enter, exit := bytes.LastIndex(p, []byte(ansi.SetModeAltScreenSaveCursor)), bytes.LastIndex(p, []byte(ansi.ResetModeAltScreenSaveCursor))
	if enter >= 0 || exit >= 0 {
		o.screen = imageScreen{o.screen.epoch + 1, enter > exit}
		clear(o.pending)
		select {
		case <-o.events:
		default:
		}
		o.events <- o.screen
	} else if err == nil && o.screen.up && len(o.pending) > 0 && bytes.Contains(p, []byte(string(kitty.Placeholder))) {
		for id, image := range o.pending {
			if !image.queued && paintsPlaceholder(p, image.color) {
				select {
				case o.painted <- imagePainted{image.placement}:
					image.queued = true
					o.pending[id] = image
				default: // leave it eligible on a later write, never block rendering
				}
			}
		}
	}
	return n, err
}

func (o *imageOutput) WriteString(s string) (int, error) { return o.Write([]byte(s)) }

// A new placeholder run starts with its truecolor foreground, possibly in
// the same SGR as other parameters. Match cells, not SGR-colored prose.
func paintsPlaceholder(frame []byte, color string) bool {
	for {
		i := bytes.Index(frame, []byte(color))
		if i < 0 {
			return false
		}
		start := i > 0 && (frame[i-1] == '[' || frame[i-1] == ';')
		frame = frame[i+len(color):]
		if !start || len(frame) == 0 || frame[0] != ';' && frame[0] != 'm' {
			continue
		}
		end := 0
		for end < len(frame) && (frame[end] == ';' || frame[end] == ':' || frame[end] >= '0' && frame[end] <= '9') {
			end++
		}
		if end < len(frame) && frame[end] == 'm' && bytes.HasPrefix(frame[end+1:], []byte(string(kitty.Placeholder))) {
			return true
		}
	}
}

func (o *imageOutput) uploadImage(ctx context.Context, place, old imagePlacement, value string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.screen != place.screen || !place.screen.up || ctx.Err() != nil || value == "" {
		return false
	}
	if old.id != 0 && old.screen == place.screen {
		delete(o.pending, old.id)
		value = kittyDelete(old.id) + value
	}
	n, err := o.File.WriteString(value)
	if err != nil || n != len(value) {
		return false
	}
	if o.painted != nil && place.screen.epoch > 1 {
		if o.pending == nil {
			o.pending = make(map[int]pendingImage, previewCacheSize)
		}
		o.pending[place.id] = pendingImage{placement: place, color: kittyColor(place.id)}
	}
	return true
}

// Kitty 0.45 can leave newly written placeholders blank on re-entry.
// Reassert the same virtual placement once, in a later render cycle. Do not
// re-upload pixels, change the cursor, or refresh an evicted/old-screen image.
func (o *imageOutput) redrawImage(ctx context.Context, place imagePlacement) tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(120 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			o.putImage(ctx, place)
		}
		return nil
	}
}

func (o *imageOutput) putImage(ctx context.Context, place imagePlacement) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if place.screen == o.screen && place.screen.up && ctx.Err() == nil && o.pending[place.id].queued {
		delete(o.pending, place.id)
		_, _ = o.File.WriteString(kittyPut(place))
	}
}

func (o *imageOutput) deleteImage(ctx context.Context, place imagePlacement) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if place.screen == o.screen && place.screen.up && ctx.Err() == nil {
		delete(o.pending, place.id)
		_, _ = o.File.WriteString(kittyDelete(place.id))
	}
}

func waitImagePaint(ctx context.Context, o *imageOutput) tea.Cmd {
	if o == nil || o.painted == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case v := <-o.painted:
			return v
		case <-ctx.Done():
			return nil
		}
	}
}

func waitImageScreen(ctx context.Context, o *imageOutput) tea.Cmd {
	if o == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case v := <-o.events:
			return v
		case <-ctx.Done():
			return nil
		}
	}
}
