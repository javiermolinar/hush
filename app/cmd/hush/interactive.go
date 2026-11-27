package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/javiermolinar/hush/app/internal/authproviders"
	"github.com/javiermolinar/hush/app/internal/backendstart"
	"github.com/javiermolinar/hush/app/internal/headless"
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

func runInteractive(ctx context.Context, o headless.InteractiveOptions) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	local := o.Socket == "" && os.Getenv("HUSH_SOCKET") == ""
	if o.Local != nil {
		local = *o.Local
	}
	caps := map[string][]uint64{
		"modelCatalog": {sdk.ModelCatalogDraftVersion, sdk.ModelCatalogSearchVersion, sdk.ModelCatalogVersion}, "intro": {sdk.IntroActionsVersion, sdk.IntroVersion},
		"threadDirectory": {sdk.ThreadManagementVersion, sdk.ThreadDirectoryVersion}, "threadWorkspace": {sdk.ThreadWorkspaceSearchVersion, sdk.ThreadWorkspaceVersion},
		"images": {sdk.ImagesVersion}, "nativeStop": {sdk.NativeStopVersion}, "schedules": {sdk.SchedulesVersion}, "plan": {sdk.PlanVersion},
		"planWorkspace": {sdk.PlanWorkspaceVersion}, "planReview": {sdk.PlanReviewVersion}, "planDraft": {sdk.PlanDraftVersion}, "planChat": {sdk.PlanChatVersion}, "planWorktree": {sdk.PlanWorktreeVersion}, "planCatalog": {sdk.PlanCatalogVersion},
		"planChatActivity": {sdk.PlanChatActivityVersion},
		"checkpoints":      {sdk.CheckpointsVersion}, "gitDiff": {sdk.GitDiffVersion}, "settings": {sdk.SettingsVersion},
		"workspaceFiles":      {sdk.WorkspaceFileProjectPathsVersion, sdk.WorkspaceFileSearchVersion, sdk.WorkspaceFilesVersion},
		"workspaceScopeWatch": {sdk.WorkspaceScopeWatchVersion}, "gitDiffWatch": {sdk.GitDiffWatchVersion},
		"workspaceSkills": {sdk.WorkspaceSkillDraftVersion, sdk.WorkspaceSkillsVersion}, "loadedContext": {sdk.LoadedContextAgentsVersion, sdk.LoadedContextVersion},
	}
	caps["clientEnvironment"] = []uint64{sdk.ClientEnvironmentVersion}
	caps["delegation"] = []uint64{sdk.DelegationVersion}
	caps["workerActivity"] = []uint64{sdk.WorkerActivityVersion}
	caps["subagents"] = []uint64{sdk.SubagentOverviewVersion}
	environment := detectClientEnvironment(ctx)
	target := backendstart.Options{Socket: o.Socket, Bundle: o.Bundle, RequireWorkTrust: true}
	client, err := backendstart.NewClient(ctx, target, sdk.ClientOptions{Capabilities: caps})
	if err != nil {
		return err
	}
	defer client.Close()
	m, err := tui.New(ctx, client, tui.Options{
		ThreadID: o.Thread, NewThread: sdk.NewThread{CWD: cwd}, WorkspaceLocal: local,
		Fills: tui.FillMode(o.Fills), Images: tui.ImageMode(o.Images), ThemeName: o.Theme,
		ReducedMotion: !o.Motion, CustomModels: true,
		Extensions:        []extension.Registration{inbox.New()},
		ProviderLogin:     localProviderLogin(o),
		WorkspaceTrust:    workspaceTrustOptions(o),
		Intro:             &tui.IntroOptions{StatePath: introStatePath()},
		ClientEnvironment: environment,
		OpenReviewPane:    reviewPaneOpener(ctx, target, environment.Multiplexer),
	})
	if err != nil {
		return err
	}
	err = m.Run()
	if err == nil && o.Socket == "" && os.Getenv("HUSH_SOCKET") == "" {
		backendstart.ReportServiceAfterExit(ctx, os.Stderr)
	}
	return err
}

func localProviderLogin(o headless.InteractiveOptions) *tui.ProviderLoginOptions {
	if o.Socket != "" || os.Getenv("HUSH_SOCKET") != "" {
		return nil
	}
	var choices []extension.Choice
	for _, p := range authproviders.All() {
		choices = append(choices, extension.Choice{ID: p.ID, Label: p.Label, Description: p.Description})
	}
	return &tui.ProviderLoginOptions{
		Providers: choices,
		Command: func(ctx context.Context, provider string) (*exec.Cmd, error) {
			if _, ok := authproviders.Lookup(provider); !ok {
				return nil, errors.New("unsupported sign-in provider")
			}
			exe, err := os.Executable()
			if err != nil {
				return nil, err
			}
			args := []string{"auth", "login"}
			if provider != authproviders.Default {
				args = append(args, provider)
			}
			if o.Bundle != "" {
				args = append(args, "--backend", o.Bundle)
			}
			cmd := exec.CommandContext(ctx, exe, args...)
			// Let the child cancel the auth flow and restore the tty before it exits.
			cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
			cmd.WaitDelay = 5 * time.Second
			return cmd, nil
		},
	}
}
