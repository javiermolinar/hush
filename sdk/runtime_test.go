package sdk

import (
	"bufio"
	"errors"
	"net"
	"testing"
)

func TestStopRuntime(t *testing.T) {
	for _, mode := range []string{"acknowledged", "unnegotiated", "malformed", "lost"} {
		t.Run(mode, func(t *testing.T) {
			ctx, path := runContext(t)
			listener(t, path, func(c net.Conn, r *bufio.Reader) {
				q, err := readReq(r)
				if err != nil || q.Method != "hello" {
					return
				}
				capabilities := map[string]uint64{}
				if mode != "unnegotiated" {
					capabilities[runtimeCapability] = RuntimeVersion
				}
				reply(c, q, Hello{Version: 1, Epoch: "runtime-fixture", Capabilities: capabilities})
				if mode == "unnegotiated" {
					if q, err := readReq(r); err == nil {
						t.Errorf("stop was sent without negotiation: %+v", q)
					}
					return
				}
				q, err = readReq(r)
				if err != nil || q.Method != runtimeStopMethod || len(q.Params) != 0 {
					t.Errorf("runtime stop request = %+v, %v", q, err)
					return
				}
				if mode == "lost" {
					return
				}
				result := any(struct{}{})
				if mode == "malformed" {
					result = map[string]any{"unexpected": true}
				}
				reply(c, q, result)
			})
			client := NewClient(path, ClientOptions{Capabilities: map[string][]uint64{runtimeCapability: {RuntimeVersion}}})
			err := client.StopRuntime(ctx)
			client.Close()
			switch mode {
			case "acknowledged":
				if err != nil {
					t.Fatal(err)
				}
			case "unnegotiated":
				var server *ServerError
				if !errors.As(err, &server) || server.Code != "unsupported_capability" || server.Capability != runtimeCapability {
					t.Fatalf("unnegotiated stop = %v", err)
				}
			case "malformed":
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("malformed stop reply = %v", err)
				}
			case "lost":
				var transport *TransportError
				if !errors.As(err, &transport) || !transport.Indeterminate {
					t.Fatalf("lost stop reply = %v", err)
				}
			}
		})
	}
}
