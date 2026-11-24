package sdk

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

func TestImageSubmitRetriesOnlySameAdmission(t *testing.T) {
	ctx, path := runContext(t)
	var attempts atomic.Uint32
	wire := make(chan wireReq, 3)
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		q, err := readReq(r)
		if err != nil || q.Method != "hello" {
			return
		}
		reply(c, q, Hello{Version: 1, Epoch: "test-images", Capabilities: map[string]uint64{"images": ImagesVersion}})
		q, err = readReq(r)
		if err != nil {
			return
		}
		wire <- q
		if attempts.Add(1) == 1 {
			return // admission may have succeeded; lose its reply
		}
		reply(c, q, Receipt{SubmissionID: "7"})
		q, err = readReq(r)
		if err != nil {
			return
		}
		wire <- q
		reply(c, q, Image{MIMEType: "image/png", Data: []byte{1, 2, 3}})
		// Keep the connection until the caller has consumed the reply.
		_, _ = r.ReadByte()
	})
	c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{"images": {ImagesVersion}}})
	defer c.Close()
	input := Input{ThreadID: "1", RequestID: "same-key", Images: []Image{{MIMEType: "image/png", Data: []byte{1, 2, 3}}}}
	receipt, err := c.Submit(ctx, input)
	if err != nil || receipt.SubmissionID != "7" || attempts.Load() != 2 {
		t.Fatalf("image admission retry: %+v %v", receipt, err)
	}
	q1, q2 := <-wire, <-wire
	a, _ := json.Marshal(q1.Params)
	b, _ := json.Marshal(q2.Params)
	if q1.Method != imageSubmit || q2.Method != imageSubmit || q1.Params["requestId"] != "same-key" || !bytes.Equal(a, b) {
		t.Fatal("retry did not preserve the same endpoint, key and image request")
	}
	ref := ImageRef{ThreadID: "1", EntryID: "2", MessageIndex: 2, BlockIndex: 3}
	img, err := c.GetImage(ctx, ref)
	if err != nil || !bytes.Equal(img.Data, input.Images[0].Data) {
		t.Fatalf("image get: %v %v", img, err)
	}
	q := <-wire
	if q.Method != imageGet || q.Params["entryId"] != "2" || q.Params["threadId"] != "1" || q.Params["blockIndex"] != float64(3) {
		t.Fatalf("wrong reference: %+v", q)
	}
}

func TestImagesRequireNegotiationBeforeSendingBytes(t *testing.T) {
	ctx, path := runContext(t)
	var submitted atomic.Uint32
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		if greeting(c, r, "no-images") {
			if _, err := readReq(r); err == nil {
				submitted.Add(1)
			}
		}
	})
	c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{"images": {ImagesVersion}}})
	defer c.Close()
	_, err := c.Submit(ctx, Input{ThreadID: "1", RequestID: "disallowed", Images: []Image{{MIMEType: "image/png", Data: []byte{1}}}})
	var w *ServerError
	if !errors.As(err, &w) || w.Code != "unsupported_capability" || w.Capability != "images" || submitted.Load() != 0 {
		t.Fatalf("images negotiation not enforced: %v", err)
	}
}
