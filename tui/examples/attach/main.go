// Command attach is a development example for running the TUI against an
// already listening hush backend. It does not start a backend or replace the
// headless hush CLI.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui"
	"github.com/javiermolinar/hush/tui/examples/inbox"
	"github.com/javiermolinar/hush/tui/extension"
)

func introStatePath() string {
	path := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(path) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(path, "hush", "intro")
}

func main() {
	path := flag.String("socket", "", "path to a running backend's Unix socket")
	thread := flag.String("thread", "", "initial native thread ID")
	local := flag.Bool("local", false, "allow file links to open in local workspaces")
	fills := flag.String("fills", "auto", "background fills: auto, on or off")
	images := flag.String("images", "auto", "images: auto, kitty, blocks or none")
	motion := flag.Bool("motion", true, "animate live effects and blink the cursor")
	flag.Parse()
	if *path == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := sdk.NewClient(*path, sdk.ClientOptions{Capabilities: map[string][]uint64{"modelCatalog": {sdk.ModelCatalogDraftVersion, sdk.ModelCatalogSearchVersion, sdk.ModelCatalogVersion}, "piEvents": {sdk.PiEventsVersion}, "intro": {sdk.IntroActionsVersion, sdk.IntroVersion}, "threadDirectory": {sdk.ThreadManagementVersion, sdk.ThreadDirectoryVersion}, "images": {sdk.ImagesVersion}}})
	defer client.Close()
	cwd, err := os.Getwd()
	if err == nil {
		var m *tui.Model
		m, err = tui.New(ctx, client, tui.Options{
			ThreadID: sdk.ID(*thread), NewThread: sdk.NewThread{CWD: cwd},
			WorkspaceLocal: *local, Fills: tui.FillMode(*fills), Images: tui.ImageMode(*images), ReducedMotion: !*motion,
			CustomModels: true, Extensions: []extension.Registration{inbox.New()},
			Intro: &tui.IntroOptions{StatePath: introStatePath()},
		})
		if err == nil {
			err = m.Run()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
