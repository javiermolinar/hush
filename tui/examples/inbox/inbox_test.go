package inbox_test

import (
	"testing"

	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui"
	"github.com/javiermolinar/hush/tui/examples/inbox"
	"github.com/javiermolinar/hush/tui/extension"
)

func TestInboxCoexistsWithBuiltInBindings(t *testing.T) {
	c := sdk.NewClient("unused.sock", sdk.ClientOptions{})
	defer c.Close()
	m, err := tui.New(t.Context(), c, tui.Options{Extensions: []extension.Registration{inbox.New()}})
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}
