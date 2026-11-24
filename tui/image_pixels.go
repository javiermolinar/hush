package tui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	_ "golang.org/x/image/webp"
)

const maxPreviewAxis = 320
const maxDecodePixels = 24_000_000

func imageConfig(data []byte) (image.Config, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 8192 || cfg.Height > 8192 || cfg.Width*cfg.Height > maxDecodePixels {
		return image.Config{}, "", errors.New("invalid or oversized image")
	}
	return cfg, format, nil
}

func decodePreview(data []byte) (*image.NRGBA, error) {
	cfg, _, err := imageConfig(data)
	if err != nil {
		return nil, err
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := decoded.Bounds()
	scale := max(maxPreviewAxis, cfg.Width, cfg.Height)
	w, h := max(1, cfg.Width*maxPreviewAxis/scale), max(1, cfg.Height*maxPreviewAxis/scale)
	preview := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			preview.Set(x, y, decoded.At(b.Min.X+(2*x+1)*b.Dx()/(2*w), b.Min.Y+(2*y+1)*b.Dy()/(2*h)))
		}
	}
	return preview, nil
}

// Bound the preview to a small part of the viewport, with a 1:2 cell aspect.
func previewCells(img *image.NRGBA, width int) (cols, rows int) {
	if img == nil || width <= 0 {
		return 0, 0
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	cols = min(36, width, max(1, w), max(1, 24*w/h))
	rows = min(12, max(1, (cols*h+w)/(2*w)))
	return cols, rows
}

func halfBlockPreview(img *image.NRGBA, cols, rows int, backgrounds bool) []string {
	b := img.Bounds()
	pixel := func(x, y int) color.NRGBA {
		return img.NRGBAAt(b.Min.X+(2*x+1)*b.Dx()/(2*cols), b.Min.Y+(2*y+1)*b.Dy()/(4*rows))
	}
	lines := make([]string, rows)
	for row := range lines {
		var line strings.Builder
		for col := range cols {
			top, bottom := pixel(col, 2*row), pixel(col, 2*row+1)
			switch {
			case top.A == 0 && bottom.A == 0:
				line.WriteByte(' ')
			case bottom.A == 0:
				fmt.Fprintf(&line, "\x1b[38;2;%d;%d;%dm▀\x1b[39m", top.R, top.G, top.B)
			case top.A == 0:
				fmt.Fprintf(&line, "\x1b[38;2;%d;%d;%dm▄\x1b[39m", bottom.R, bottom.G, bottom.B)
			case backgrounds:
				fmt.Fprintf(&line, "\x1b[38;2;%d;%d;%d;48;2;%d;%d;%dm▀\x1b[39;49m", top.R, top.G, top.B, bottom.R, bottom.G, bottom.B)
			default:
				// On translucent terminals, keep the image in the foreground
				// rather than painting a second pixel as a cell background.
				fmt.Fprintf(&line, "\x1b[38;2;%d;%d;%dm█\x1b[39m",
					(int(top.R)+int(bottom.R))/2, (int(top.G)+int(bottom.G))/2, (int(top.B)+int(bottom.B))/2)
			}
		}
		lines[row] = line.String()
	}
	return lines
}

// Transmit packed straight-alpha RGBA and create a virtual placement.
// Rex rejects raw RGBA without s and v. q=1 suppresses success, not errors.
func kittyUpload(img *image.NRGBA, p imagePlacement) string {
	if img == nil || p.id <= 0 || p.id > 0xffffff || p.cols <= 0 || p.rows <= 0 {
		return ""
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	pix := make([]byte, 0, w*h*4)
	for y := range h {
		pix = append(pix, img.Pix[y*img.Stride:y*img.Stride+w*4]...)
	}
	data := []byte(base64.StdEncoding.EncodeToString(pix))
	var out strings.Builder
	for i := 0; i < len(data); {
		end := min(len(data), i+kitty.MaxChunkSize)
		more := "m=0"
		if end < len(data) {
			more = "m=1"
		}
		if i == 0 {
			out.WriteString(ansi.KittyGraphics(data[i:end], "a=T", "t=d", "f=32", "U=1", "p=1", "q=1", more,
				fmt.Sprintf("i=%d", p.id), fmt.Sprintf("s=%d", w), fmt.Sprintf("v=%d", h), fmt.Sprintf("c=%d", p.cols), fmt.Sprintf("r=%d", p.rows)))
		} else {
			out.WriteString(ansi.KittyGraphics(data[i:end], more))
		}
		i = end
	}
	return out.String()
}

func kittyColor(id int) string {
	return fmt.Sprintf("38;2;%d;%d;%d", id>>16&255, id>>8&255, id&255)
}

func kittyPlaceholders(id, cols, rows int) []string {
	lines := make([]string, rows)
	sgr := "\x1b[" + kittyColor(id) + "m"
	for y := range lines {
		var line strings.Builder
		line.WriteString(sgr)
		for x := range cols {
			line.WriteRune(kitty.Placeholder)
			line.WriteRune(kitty.Diacritic(y))
			line.WriteRune(kitty.Diacritic(x))
		}
		line.WriteString("\x1b[39m")
		lines[y] = line.String()
	}
	return lines
}

func kittyPut(p imagePlacement) string {
	return ansi.KittyGraphics(nil, "a=p", "U=1", "p=1", "q=1", fmt.Sprintf("i=%d", p.id), fmt.Sprintf("c=%d", p.cols), fmt.Sprintf("r=%d", p.rows))
}

func kittyDelete(id int) string {
	return ansi.KittyGraphics(nil, "a=d", "d=I", "q=1", fmt.Sprintf("i=%d", id))
}
