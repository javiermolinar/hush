package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/javiermolinar/hush/app/internal/backendstart"
	"github.com/javiermolinar/hush/app/internal/headless"
	"github.com/javiermolinar/hush/tui"
)

func localGitTrust(o headless.InteractiveOptions) func(context.Context, string) (tui.GitTrustPreview, error) {
	// Sharing the workspace for links is not permission to grant trust for a
	// socket peer. Only the managed Hush profile can use this host-local helper.
	if o.Profile != string(backendstart.Hush) || o.Socket != "" || os.Getenv("HUSH_SOCKET") != "" || o.Local != nil && !*o.Local {
		return nil
	}
	return func(ctx context.Context, cwd string) (tui.GitTrustPreview, error) {
		out, err := runGitTrust(ctx, o.Bundle, "review-git", "--cwd="+cwd)
		if err != nil {
			return tui.GitTrustPreview{}, err
		}
		var review struct{ Path, Expected string }
		if err := json.Unmarshal(out, &review); err != nil || !filepath.IsAbs(review.Path) || len(review.Expected) != 64 {
			return tui.GitTrustPreview{}, errors.New("invalid local Git trust review")
		}
		return tui.GitTrustPreview{GitDirectory: review.Path, Grant: func(ctx context.Context) error {
			_, err := runGitTrust(ctx, o.Bundle, "grant-reviewed-git", "--cwd="+cwd, "--expect="+review.Expected)
			return err
		}}, nil
	}
}

func runGitTrust(ctx context.Context, bundle string, args ...string) (out []byte, err error) {
	node, entry, cleanup, err := backendstart.LocalInvocation(ctx, bundle)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, cleanup()) }()
	cmd := exec.CommandContext(ctx, node, append([]string{entry, "trust"}, args...)...)
	cmd.Env = append(backendstart.RuntimeEnv(backendstart.Hush), "HUSH_PROFILE=hush")
	return cmd.Output()
}
