package sdk

import (
	"context"
	"encoding/base64"
)

const (
	ImagesVersion = 1
	MaxImageBytes = 5 << 20 // total decoded attachment bytes per input; also the limit for each fetched image
	MaxImageCount = 4
	imageSubmit   = "ext.images.submit"
	imageGet      = "ext.images.get"
)

// Image is an encoded PNG, JPEG, GIF or WebP. Do not mutate Data during a request.
type Image struct {
	MIMEType string `json:"mimeType"`
	Data     []byte `json:"data"`
}

func (i Image) valid() bool {
	switch i.MIMEType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return len(i.Data) > 0 && len(i.Data) <= MaxImageBytes
	default:
		return false
	}
}

func validImageInput(input Input) bool {
	if len(input.Images) > MaxImageCount || len(input.Text) > 32<<10 {
		return false
	}
	total := 0
	for _, i := range input.Images {
		if !i.valid() {
			return false
		}
		total += len(i.Data)
	}
	return total <= MaxImageBytes
}

// ImageRef addresses the omitted/image block in an immutable committed entry.
// ThreadID is the thread whose history exposes it, not necessarily its original authoring thread.
type ImageRef struct {
	ThreadID     ID  `json:"threadId"`
	EntryID      ID  `json:"entryId"`
	MessageIndex int `json:"messageIndex"`
	BlockIndex   int `json:"blockIndex"`
}

func (c *Client) imageSession(ctx context.Context) (*session, error) {
	s, err := c.connect(ctx)
	if err == nil && s.hello.Capabilities["images"] != ImagesVersion {
		err = &ServerError{Code: "unsupported_capability", Message: "Capability was not negotiated", Capability: "images"}
	}
	return s, err
}

// GetImage resolves one reference. State and history never contain the image bytes.
func (c *Client) GetImage(ctx context.Context, ref ImageRef) (Image, error) {
	const limit = 1 << 20
	if !ref.ThreadID.Valid() || !ref.EntryID.Valid() ||
		ref.MessageIndex < 0 || ref.MessageIndex >= limit || ref.BlockIndex < 0 || ref.BlockIndex >= limit {
		return Image{}, ErrProtocol
	}
	s, err := c.imageSession(ctx)
	if err != nil {
		return Image{}, err
	}
	v, err := s.call(ctx, imageGet, ref, nil)
	if err != nil {
		return Image{}, err
	}
	wire, ok := v.(map[string]any)
	if !ok {
		return Image{}, ErrProtocol
	}
	data, _ := wire["data"].(string)
	mime, _ := wire["mimeType"].(string)
	blob, err := base64.StdEncoding.Strict().DecodeString(data)
	img := Image{MIMEType: mime, Data: blob}
	if err != nil || !img.valid() || data != base64.StdEncoding.EncodeToString(blob) {
		return Image{}, ErrProtocol
	}
	return img, nil
}
