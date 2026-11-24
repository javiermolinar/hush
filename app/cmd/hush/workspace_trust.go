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

func workspaceTrustOptions(o headless.InteractiveOptions) *tui.WorkspaceTrustOptions {
	if o.Profile != string(backendstart.Hush) {
		return nil
	}
	// Sharing links with a socket peer doesn't authorize a local grant.
	if o.Socket != "" || os.Getenv("HUSH_SOCKET") != "" || o.Local != nil && !*o.Local {
		return &tui.WorkspaceTrustOptions{}
	}
	return &tui.WorkspaceTrustOptions{Review: func(ctx context.Context, cwd string) (tui.WorkspaceTrustPreview, error) {
		out, err := runWorkspaceTrust(ctx, o.Bundle, "review-work", "--cwd="+cwd)
		if err != nil {
			return tui.WorkspaceTrustPreview{}, err
		}
		var review struct {
			Path, Expected     string
			Trusted, Dismissed bool
		}
		if err := json.Unmarshal(out, &review); err != nil || !filepath.IsAbs(review.Path) || len(review.Expected) != 64 {
			return tui.WorkspaceTrustPreview{}, errors.New("invalid local workspace trust review")
		}
		return tui.WorkspaceTrustPreview{Path: review.Path, Trusted: review.Trusted, Dismissed: review.Dismissed,
			Decide: func(ctx context.Context, grant bool) error {
				action := "dismiss-reviewed-work"
				if grant {
					action = "grant-reviewed-work"
				}
				_, err := runWorkspaceTrust(ctx, o.Bundle, action, "--cwd="+cwd, "--expect="+review.Expected)
				return err
			},
		}, nil
	}}
}

func runWorkspaceTrust(ctx context.Context, bundle string, args ...string) (out []byte, err error) {
	node, entry, cleanup, err := backendstart.LocalInvocation(ctx, bundle)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, cleanup()) }()
	cmd := exec.CommandContext(ctx, node, append([]string{entry, "trust"}, args...)...)
	cmd.Env = append(backendstart.RuntimeEnv(backendstart.Hush), "HUSH_PROFILE=hush")
	return cmd.Output()
}
