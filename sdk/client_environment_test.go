package sdk

import (
	"bufio"
	"errors"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestClientEnvironmentNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		capable, images bool
		method          string
	}{
		{"old text host", false, false, "submit"},
		{"old image host", false, true, imageSubmit},
		{"new host", true, false, environmentSubmit},
		{"new host with image", true, true, environmentSubmit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, path := runContext(t)
			wire := make(chan wireReq, 1)
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					return
				}
				selected := map[string]uint64{}
				if tc.capable {
					selected[clientEnvironmentCapability] = ClientEnvironmentVersion
				}
				if tc.images {
					selected["images"] = ImagesVersion
				}
				reply(c, q, Hello{Version: 1, Epoch: "environment-fixture", Capabilities: selected})
				q, err = readReq(r)
				if err != nil {
					return
				}
				wire <- q
				reply(c, q, Receipt{SubmissionID: "2"})
				_, _ = r.ReadByte()
			})
			c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{
				clientEnvironmentCapability: {ClientEnvironmentVersion}, "images": {ImagesVersion},
			}})
			defer c.Close()
			input := Input{ThreadID: "1", RequestID: "key", Text: " unchanged text ", Environment: ClientEnvironment{
				OS: "macos", Terminal: "my-internal-terminal-name", Multiplexer: "/tmp/private.sock", Shell: "fish", LoginShellHint: "zsh",
			}}
			if tc.images {
				input.Images = []Image{{MIMEType: "image/png", Data: []byte{1, 2, 3}}}
			}
			if got, err := c.Submit(ctx, input); err != nil || got.SubmissionID != "2" {
				t.Fatalf("submit: %+v %v", got, err)
			}
			q := <-wire
			if q.Method != tc.method {
				t.Fatalf("sent %s, want %s", q.Method, tc.method)
			}
			p := q.Params
			if tc.capable {
				want := map[string]any{"os": "macos", "shell": "fish"}
				if !reflect.DeepEqual(p["environment"], want) {
					t.Errorf("wrong environment: %v", p["environment"])
				}
				p, _ = p["input"].(map[string]any)
			}
			if _, ok := p["environment"]; ok || p["text"] != input.Text || p["requestId"] != input.RequestID || (p["images"] != nil) != tc.images {
				t.Fatalf("wrong base input: %v", p)
			}
		})
	}
}

func TestClientEnvironmentRetryDoesNotDowngrade(t *testing.T) {
	ctx, path := runContext(t)
	var peers atomic.Int32
	listener(t, path, func(c net.Conn, r *bufio.Reader) {
		first := peers.Add(1) == 1
		extra, epoch := map[string]uint64{}, "second-boot"
		if first {
			extra[clientEnvironmentCapability], epoch = ClientEnvironmentVersion, "first-boot"
		}
		if !storeGreeting(t, c, r, epoch, fixtureStore, extra) {
			return
		}
		q, err := readReq(r)
		if first && (err != nil || q.Method != environmentSubmit) {
			t.Errorf("wrong first write: %+v %v", q, err)
		}
		if !first && err == nil {
			t.Errorf("replayed via %s after capability loss", q.Method)
		}
	})
	c := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{clientEnvironmentCapability: {ClientEnvironmentVersion}}})
	defer c.Close()
	_, err := c.Submit(ctx, Input{ThreadID: "1", RequestID: "key", Text: "hello", Environment: ClientEnvironment{Shell: "fish"}})
	var lost *TransportError
	var server *ServerError
	if peers.Load() != 2 || !errors.As(err, &lost) || !lost.Indeterminate || !errors.As(err, &server) || server.Capability != clientEnvironmentCapability {
		t.Fatalf("missing uncertain result after lost capability: %v", err)
	}
}
