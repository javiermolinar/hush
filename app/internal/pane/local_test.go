package pane

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/javiermolinar/hush/sdk"
)

func TestLocalPaneHostChecksSocketAndDirectory(t *testing.T) {
	a, fixture := fakeAdapter(t, "rex", true)
	h := NewHandler(a, "")
	dir, sibling := t.TempDir(), t.TempDir()
	stamp, ok := localIdentity(dir, os.ModeDir)
	if !ok {
		t.Fatal("directory identity missing")
	}
	request := sdk.PaneRequest{Op: "open", Placement: "below", Dir: sibling, Directory: stamp,
		Launch: sdk.PaneLaunch{Mode: "argv", Program: "example"}}
	if got := h.Handle(t.Context(), request); got.Outcome != "rejected" || len(loadFixture(fixture).Effects) != 0 {
		t.Fatal("mismatched working directory dispatched the native command", got)
	}
	request.Dir = dir
	if got := h.Handle(t.Context(), request); got.Outcome != "confirmed" || len(loadFixture(fixture).Effects) != 1 {
		t.Fatal("local working directory did not dispatch exactly once", got)
	}
	first, second := loadFixture(fixture).Socket, filepath.Join(filepath.Dir(fixture), "forwarded")
	forwardingSocket, err := net.Listen("unix", second)
	if err != nil {
		t.Fatal(err)
	}
	defer forwardingSocket.Close()
	proof, ok := localIdentity(first, os.ModeSocket)
	if !ok || !h.LocalBackend(first, proof) || h.LocalBackend(second, proof) {
		t.Fatal("backend proof accepted a different local socket")
	}
}
