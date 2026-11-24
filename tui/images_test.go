package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/javiermolinar/hush/sdk"
)

func imageEntry(id int) sdk.Entry {
	e := entry(id, "user", "Attached screenshot")
	e.Messages[0].Blocks = append(e.Messages[0].Blocks, sdk.Block{Type: "omitted", Kind: "image"})
	return e
}

func sampleImage(w, h int) *image.NRGBA {
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			im.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 255 / max(1, w-1)), G: uint8(y * 255 / max(1, h-1)), B: 120, A: 255})
		}
	}
	return im
}

func pngImage(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, sampleImage(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestKittyTransmitHasDimensionsAndErrorReplies(t *testing.T) {
	const id = 0x009fef
	im := sampleImage(64, 32) // spans several protocol chunks
	im.SetNRGBA(0, 0, color.NRGBA{R: 242, G: 100, B: 53, A: 71})
	seq := kittyUpload(im, imagePlacement{id: id, cols: 18, rows: 5})
	var data strings.Builder
	chunks := strings.Split(strings.TrimSuffix(seq, "\x1b\\"), "\x1b\\")
	for i, chunk := range chunks {
		header, encoded, ok := strings.Cut(chunk, ";")
		if !ok || !strings.HasPrefix(header, "\x1b_G") || len(encoded) > kitty.MaxChunkSize {
			t.Fatalf("invalid kitty chunk header: %q", header)
		}
		want := []string{"m=1"}
		if i == len(chunks)-1 {
			want[0] = "m=0"
		}
		if i == 0 {
			want = append(want, "f=32", "s=64", "v=32", "q=1", "a=T", "U=1", "p=1", "i=40943", "c=18", "r=5")
		}
		for _, field := range want {
			if !strings.Contains(","+strings.TrimPrefix(header, "\x1b_G")+",", ","+field+",") {
				t.Fatalf("missing %s in %q", field, header)
			}
		}
		data.WriteString(encoded)
	}
	decoded, err := base64.StdEncoding.DecodeString(data.String())
	if err != nil || !bytes.Equal(decoded, im.Pix) {
		t.Fatal("transmitted pixels must be packed straight-alpha RGBA")
	}
	for _, mode := range [][]string{kittyPlaceholders(id, 18, 5), halfBlockPreview(im, 18, 5, true), halfBlockPreview(im, 18, 5, false)} {
		for _, line := range mode {
			if w := ansi.StringWidth(line); w != 18 {
				t.Fatalf("preview row width %d, want 18: %q", w, line)
			}
		}
	}
}

func TestBlockPreviewLayoutTracksSizeAndFills(t *testing.T) {
	preview := imagePreview{pixels: sampleImage(320, 180)}
	for _, tc := range []struct {
		cols, rows int
		fills      bool
	}{
		{36, 10, true}, {36, 10, true}, {36, 10, false}, {17, 5, false}, {36, 10, true},
	} {
		got := preview.blockRows(tc.cols, tc.rows, tc.fills)
		if want := halfBlockPreview(preview.pixels, tc.cols, tc.rows, tc.fills); !slices.Equal(got, want) {
			t.Fatalf("memoized preview differed at %+v", tc)
		}
	}
}

func TestKittyPutIsOrderedAfterItsPlaceholderFrame(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "renderer-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	o := &imageOutput{File: f, events: make(chan imageScreen, 1), painted: make(chan imagePainted, 2)}
	write := func(s string) {
		t.Helper()
		if _, err := o.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	write(ansi.SetModeAltScreenSaveCursor)
	old := imagePlacement{screen: <-o.events, cols: 2, rows: 1, id: 0x010203}
	pixels := sampleImage(4, 2)
	oldUpload := kittyUpload(pixels, old)
	if !o.uploadImage(ctx, old, imagePlacement{}, oldUpload) {
		t.Fatal("first upload failed")
	}
	write(ansi.ResetModeAltScreenSaveCursor)
	write(ansi.SetModeAltScreenSaveCursor)
	current := imagePlacement{screen: <-o.events, cols: 3, rows: 1, id: 0x010204}
	upload := kittyUpload(pixels, current)
	if !o.uploadImage(ctx, current, old, upload) {
		t.Fatal("re-entry upload failed")
	}
	// Late cells for the old epoch, an unrelated placeholder color with a
	// common decimal prefix, and styled text alone cannot trigger a put.
	styled := "\x1b[" + kittyColor(current.id) + "mtext\x1b[39m"
	unrelated := styled + kittyPlaceholders(old.id, 2, 1)[0] + kittyPlaceholders(0x01022a, 3, 1)[0]
	write(unrelated)
	if len(o.painted) != 0 {
		t.Fatal("unrelated output armed a redraw")
	}
	cells := strings.Replace(kittyPlaceholders(current.id, 3, 1)[0], kittyColor(current.id)+"m", kittyColor(current.id)+";48;2;50;60;70m", 1)
	frame := "\x1b[?2026h" + cells + "\x1b[?2026l"
	write(frame)
	select {
	case painted := <-o.painted:
		if painted.placement != current {
			t.Fatalf("wrong placement: %+v", painted)
		}
		o.putImage(ctx, painted.placement)
	default:
		t.Fatal("matching output did not arm a redraw")
	}
	write(frame) // do not refresh on every repaint
	if len(o.painted) != 0 {
		t.Fatal("placement rearmed after redraw")
	}
	want := ansi.SetModeAltScreenSaveCursor + oldUpload + ansi.ResetModeAltScreenSaveCursor +
		ansi.SetModeAltScreenSaveCursor + upload + unrelated + frame + kittyPut(current) + frame
	// A renderer write can queue a redraw right before an editor releases
	// the terminal. The queued effect must never change the main screen.
	stale := imagePlacement{screen: current.screen, cols: 2, rows: 1, id: 0x070809}
	tailUpload := kittyUpload(pixels, stale)
	if !o.uploadImage(ctx, stale, imagePlacement{}, tailUpload) {
		t.Fatal("stale test upload failed")
	}
	tailFrame := kittyPlaceholders(stale.id, stale.cols, stale.rows)[0]
	write(tailFrame)
	if len(o.painted) != 1 {
		t.Fatal("stale test did not queue a redraw")
	}
	write(ansi.ResetModeAltScreenSaveCursor)
	o.putImage(ctx, (<-o.painted).placement)
	want += tailUpload + tailFrame + ansi.ResetModeAltScreenSaveCursor
	got, err := os.ReadFile(f.Name())
	if err != nil || string(got) != want {
		t.Fatalf("put must follow only the matching frame: %v; got %q", err, got)
	}
}

func TestImageDecodeBounded(t *testing.T) {
	p := pngImage(t, 660, 330)
	pix, err := decodePreview(p)
	if err != nil || pix.Rect.Dx() != 320 || pix.Rect.Dy() != 160 {
		t.Fatalf("preview resize: %v %v", pix, err)
	}
	binary.BigEndian.PutUint32(p[16:20], 1<<31)
	if _, err := decodePreview(p); err == nil {
		t.Fatal("invalid/oversized header was accepted")
	}
}

type imageFixtureBackend struct {
	Backend
	data  []byte
	got   []sdk.ImageRef
	input sdk.Input
	err   error
}

func (b *imageFixtureBackend) RequireCapability(context.Context, string, uint64) error { return nil }
func (b *imageFixtureBackend) GetImage(_ context.Context, ref sdk.ImageRef) (sdk.Image, error) {
	b.got = append(b.got, ref)
	return sdk.Image{Data: b.data, MIMEType: "image/png"}, b.err
}
func (b *imageFixtureBackend) Submit(_ context.Context, input sdk.Input) (sdk.Receipt, error) {
	b.input = input
	return sdk.Receipt{SubmissionID: "9"}, b.err
}

func TestImagePasteStagesBytesAndRestoresOnRejection(t *testing.T) {
	backend := &imageFixtureBackend{err: errors.New("test rejection")}
	m, s := demo(t, Options{Images: ImageBlocks})
	m.backend = backend
	m.Update(m.probeImages()())
	path := filepath.Join(t.TempDir(), "local screenshot.png")
	blob := pngImage(t, 62, 41)
	if err := os.WriteFile(path, blob, 0600); err != nil {
		t.Fatal(err)
	}
	msg := tea.PasteMsg{Content: "'" + path + "'"}
	cmd := m.pastePrompt(msg)
	if cmd == nil || !s.attaching || len(s.attachments) != 0 {
		t.Fatal("paste must do disk work off Update")
	}
	m.Update(cmd().(workMsg))
	frame := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "▣ local screenshot.png") || m.editor.value() != "" {
		t.Fatalf("image was not staged as a chip:\n%s", frame)
	}
	cmd = m.sendPrompt(sdk.RejectBusy) // no caption needed
	if cmd == nil {
		t.Fatal("image-only draft did not start a submission")
	}
	m.Update(cmd().(workMsg))
	got := backend.input
	if got.Text != "" || len(got.Images) != 1 || got.Images[0].MIMEType != "image/png" || !bytes.Equal(got.Images[0].Data, blob) {
		t.Fatal("server did not get the staged bytes")
	}
	if len(s.attachments) != 1 {
		t.Fatal("known rejection lost unsent attachment")
	}
	press(m, 'c', tea.ModCtrl)
	if len(s.attachments) != 0 || m.quitting {
		t.Fatal("clear did not discard the draft attachment")
	}
	m.pastePrompt(tea.PasteMsg{Content: "please open " + path})
	if s.attaching || !strings.HasPrefix(m.editor.value(), "please open ") {
		t.Fatal("ordinary text was treated as file paths")
	}
}

func TestImagesFetchOnlyForVisibleExpandedReferences(t *testing.T) {
	m, s := demo(t, Options{Images: ImageBlocks, Fills: FillOff})
	backend := &imageFixtureBackend{data: pngImage(t, 160, 90)}
	m.backend = backend
	m.Update(m.probeImages()())
	s.merge([]sdk.Entry{imageEntry(3), imageEntry(4)})
	for i := range 37 {
		s.merge([]sdk.Entry{entry(5+i, "assistant", "Recent text")})
	}
	if before := assertFrame(t, m); strings.Contains(before, "▣ image") || m.scanImages() != nil {
		t.Fatal("an off-screen image was wanted")
	}
	s.following, s.anchor = false, rowAnchor{id: "4"}
	if before := assertFrame(t, m); !strings.Contains(before, "▣ image") || m.scanImages() != nil {
		t.Fatal("collapsed fallback must display a summary without fetching")
	}
	press(m, 'o', tea.ModCtrl)
	assertFrame(t, m)
	cmd := m.scanImages()
	if cmd == nil {
		t.Fatal("visible expanded image not scheduled")
	}
	m.Update(cmd().(imageFetched))
	frame := assertFrame(t, m)
	if !strings.Contains(frame, "█") || strings.Contains(frame, ";48;2;") || len(backend.got) != 1 || backend.got[0] != (sdk.ImageRef{ThreadID: m.active, EntryID: "4", BlockIndex: 1}) {
		t.Fatalf("wrong image scope or no half-block preview: %v", backend.got)
	}
	if m.scanImages() != nil || assertFrame(t, m) != frame {
		t.Fatal("a cached preview was fetched again or changed on an identical frame")
	}
	m.images.mode = ImageNone
	m.images.revision++
	if text := assertFrame(t, m); strings.Contains(text, "█") || !strings.Contains(text, "▣ image") || m.scanImages() != nil {
		t.Fatal("disabled images must stay text-only even when expanded")
	}
}

func TestImageScreenEpochFencesPendingEditorPlacement(t *testing.T) {
	m, s := demo(t, Options{Images: ImageKitty})
	f, err := os.CreateTemp(t.TempDir(), "wire-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := &imageOutput{File: f, events: make(chan imageScreen, 1)}
	m.images.output, m.images.capable = out, true
	s.merge([]sdk.Entry{imageEntry(3)})
	ref := sdk.ImageRef{ThreadID: m.active, EntryID: "3", BlockIndex: 1}
	m.cacheImage(imageFetched{ref, sampleImage(100, 58)})
	if frame := assertFrame(t, m); strings.ContainsRune(frame, kitty.Placeholder) || m.scanImages() != nil {
		t.Fatal("placement started before alt screen")
	}
	send := func(seq string) {
		t.Helper()
		if _, err := out.WriteString(seq); err != nil {
			t.Fatal(err)
		}
	}
	send(ansi.SetModeAltScreenSaveCursor)
	m.Update(<-out.events)
	assertFrame(t, m)
	pending := m.scanImages()
	if pending == nil {
		t.Fatal("missing alternate-screen placement")
	}
	send(ansi.ResetModeAltScreenSaveCursor)
	stale := pending().(imagePlaced)
	if stale.ok {
		t.Fatal("stale placement wrote onto the main screen")
	}
	m.Update(stale)
	send(ansi.SetModeAltScreenSaveCursor)
	m.Update(<-out.events)
	assertFrame(t, m)
	placed := m.scanImages()().(imagePlaced)
	if !placed.ok || placed.placement.id == stale.placement.id {
		t.Fatal("new epoch did not get a fresh placement")
	}
	m.Update(placed)
	if frame := assertFrame(t, m); !strings.ContainsRune(frame, kitty.Placeholder) || strings.Contains(frame, "\x1b_G") {
		t.Fatal("successful transmission did not produce placeholders separately from payload")
	}
	wire, err := os.ReadFile(f.Name())
	if prefix := ansi.SetModeAltScreenSaveCursor + ansi.ResetModeAltScreenSaveCursor + ansi.SetModeAltScreenSaveCursor + "\x1b_G"; err != nil || !bytes.HasPrefix(wire, []byte(prefix)) {
		t.Fatalf("payload not ordered after re-entry: %v", err)
	}
}
