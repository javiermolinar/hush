package filelist

import (
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestHelperDrainsOneReplyAndIgnoresLateCancel(t *testing.T) {
	search := searchFixture(t, "generator.go")
	listing := search
	listing.Query, listing.SearchKey = "", ""
	input, send := io.Pipe()
	receive, output := io.Pipe()
	defer send.Close()
	defer receive.Close()
	timeout := time.AfterFunc(3*time.Second, func() { send.Close(); receive.Close() })
	defer timeout.Stop()
	finished := make(chan int, 1)
	go func() { finished <- Run(input, output) }()
	tx, rx := json.NewEncoder(send), json.NewDecoder(receive)
	write := func(cmd command) {
		t.Helper()
		if err := tx.Encode(cmd); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id uint64) response {
		t.Helper()
		var reply response
		if err := rx.Decode(&reply); err != nil || reply.ID != id {
			t.Fatalf("waiting for %d: %+v, %v", id, reply, err)
		}
		return reply
	}
	write(command{ID: 1, request: search})
	write(command{ID: 1, Cancel: true})
	if reply := read(1); reply.Search == nil && reply.Error != "cancelled" {
		t.Fatalf("expected a result or cancellation: %+v", reply)
	}
	write(command{ID: 2, request: listing})
	write(command{ID: 1, Cancel: true}) // possibly received during request 2
	if reply := read(2); reply.Page == nil || len(reply.Page.Entries) != 1 {
		t.Fatalf("stale cancel interfered with request 2: %+v", reply)
	}
	send.Close()
	if code := <-finished; code != 0 {
		t.Fatalf("helper exited with %d", code)
	}
}
