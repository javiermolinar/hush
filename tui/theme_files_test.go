package tui

import (
	"bytes"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const violetThemeJSON = `{"version":1,"base":"terminal","colors":{"accent":"#8b9df5","syntax_keyword":"#c69af7"}}`
const inkThemeJSON = `{"version":1,"base":"terminal","colors":{"accent":"#402886","control":"#35324d","link":"#173f83","syntax_literal":"#72231c"}}`

func themeFixture(t *testing.T) (themeStore, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM_PROGRAM", "")
	themeDir := filepath.Join(home, ".hush", "themes")
	if err := os.MkdirAll(themeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeThemeFixture(t, themeDir, "violet.json", violetThemeJSON)
	writeThemeFixture(t, themeDir, "ink.json", inkThemeJSON)
	return themeStore{home: home}, themeDir
}

func writeThemeFixture(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedThemeTemplateCoversAllRoles(t *testing.T) {
	value, err := decodeTheme("starter", []byte(ThemeTemplate()))
	if err != nil {
		t.Fatal(err)
	}
	for i, role := range colorRoles {
		if role.name != "" && value.colors[i] == nil || role.name == "" && value.colors[i] != nil {
			t.Errorf("template role %d (%q) unexpected: %v", i, role.name, value.colors[i])
		}
	}
	partial, err := decodeTheme("violet", []byte(violetThemeJSON))
	if err != nil || partial.colors[roleAccent] != (color.NRGBA{R: 139, G: 157, B: 245, A: 255}) || partial.colors[roleLink] != nil {
		t.Fatalf("partial theme: %+v %v", partial, err)
	}
	for _, tc := range []struct{ name, body, want string }{
		{"version", `{"version":2,"base":"terminal","colors":{}}`, "version 1"},
		{"base", `{"version":1,"base":"light","colors":{}}`, "base must be terminal"},
		{"role", `{"version":1,"base":"terminal","colors":{"unknown":"#000000"}}`, "unknown color role"},
		{"background", `{"version":1,"base":"terminal","colors":{"background":"#000000"}}`, "unknown color role"},
		{"code", `{"version":1,"base":"terminal","colors":{},"command":"exit"}`, "unknown object member name"},
		{"duplicate", `{"version":1,"base":"terminal","colors":{"accent":"\u001b[31m","accent":"#123456"}}`, "duplicate"},
		{"fieldCase", `{"Version":1,"base":"terminal","colors":{}}`, "unknown object member name"},
		{"null", `{"version":1,"base":"terminal","colors":null}`, "colors must be"},
		{"hex", `{"version":1,"base":"terminal","colors":{"accent":"#xxabcd"}}`, "#RRGGBB"},
		{"short", `{"version":1,"base":"terminal","colors":{"accent":"#abc"}}`, "#RRGGBB"},
		{"escape", `{"version":1,"base":"terminal","colors":{"accent":"\u001b[34m"}}`, "#RRGGBB"},
		{"trailing", violetThemeJSON + " {}", "invalid theme JSON"},
		{"utf8", string([]byte{0xff}), "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if theme, err := decodeTheme(tc.name, []byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) || theme.name != "" {
				t.Fatalf("got %+v, %v; want %q error", theme, err, tc.want)
			}
		})
	}
}

func TestPresetThemesOnFreshHome(t *testing.T) {
	store := themeStore{home: t.TempDir()}
	book := store.list(t.Context())
	if book.problem != "" || len(book.choices) != len(presetThemes)+1 || book.choices[0].name != terminalTheme {
		t.Fatalf("unexpected fresh catalog: %+v", book)
	}
	seen := map[string]bool{terminalTheme: true}
	for _, theme := range book.choices[1:] {
		if !validThemeName(theme.name) || seen[theme.name] || theme.colors[roleText] != nil {
			t.Fatalf("invalid or duplicate preset: %+v", theme)
		}
		seen[theme.name] = true
		bg := themeRGB(0x1e1e2e)
		if theme.name == "rose-pine-dawn" {
			bg = themeRGB(0xeff1f5)
		}
		for role, def := range colorRoles {
			if def.name != "" && (theme.colors[role] == nil || contrastRatio(theme.colors[role], bg) < 4.5) {
				t.Errorf("%s: %s doesn't meet contrast on the reference terminal", theme.name, def.name)
			}
		}
		if got, err := store.load(theme.name); err != nil || got != theme {
			t.Fatalf("preset override on first launch: %+v %v", got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(store.home, ".hush")); !os.IsNotExist(err) {
		t.Fatalf("listing or launching a preset created config: %v", err)
	}
	if got, err := store.load(""); err != nil || got.name != terminalTheme {
		t.Fatalf("preset changed the default: %+v %v", got, err)
	}
}

func TestLocalThemeOverridesPreset(t *testing.T) {
	store, dir := themeFixture(t)
	preset, _ := presetTheme("rose-pine")
	writeThemeFixture(t, dir, "rose-pine.json", violetThemeJSON)
	custom, err := store.load("rose-pine")
	if err != nil || custom.colors[roleAccent] != themeRGB(0x8b9df5) {
		t.Fatalf("local file didn't take precedence: %+v %v", custom, err)
	}
	count := 0
	for _, choice := range store.list(t.Context()).choices {
		if choice.name == custom.name {
			count++
			if choice != custom {
				t.Fatal("picker displayed the preset instead of the local file")
			}
		}
	}
	if count != 1 {
		t.Fatalf("picker listed %d choices for %s", count, custom.name)
	}
	if err := store.save(t.Context(), preset); err == nil {
		t.Fatal("saved a preset when the effective theme was the local file")
	}
	if err := store.save(t.Context(), custom); err != nil {
		t.Fatal(err)
	}
	if got, err := store.load(""); err != nil || got != custom {
		t.Fatalf("saved override didn't reload: %+v %v", got, err)
	}
	writeThemeFixture(t, dir, "rose-pine.json", "invalid")
	if got, err := store.load("rose-pine"); err == nil || got.name != terminalTheme {
		t.Fatal("bad local override was silently substituted")
	}
	book := store.list(t.Context())
	for _, choice := range book.choices {
		if choice.name == preset.name {
			t.Fatal("invalid effective theme was offered in the picker")
		}
	}
	if !strings.Contains(book.problem, "rose-pine.json") {
		t.Fatalf("missing diagnostic: %q", book.problem)
	}
	if err := os.Remove(filepath.Join(dir, "rose-pine.json")); err != nil {
		t.Fatal(err)
	}
	if got, err := store.load(""); err != nil || got != preset {
		t.Fatalf("missing override didn't fall back to the preset: %+v %v", got, err)
	}
}

func TestThemeStoreScopeAndPersistence(t *testing.T) {
	store, dir := themeFixture(t)
	chosen, err := store.load("violet")
	if err != nil || chosen.name != "violet" {
		t.Fatalf("named theme not loaded: %+v %v", chosen, err)
	}
	if err := store.save(t.Context(), chosen); err != nil {
		t.Fatal(err)
	}
	got, _ := store.load("")
	if got != chosen {
		t.Fatal("the saved choice did not load on next launch")
	}
	got, _ = store.load("ink")
	if got.name != "ink" || got == chosen {
		t.Fatal("explicit launch choice did not override the preference")
	}
	if got, _ := store.load(""); got != chosen {
		t.Fatal("explicit launch choice changed the preference")
	}

	workspace := t.TempDir()
	writeThemeFixture(t, workspace, "rogue.json", violetThemeJSON)
	t.Chdir(workspace)
	if err := os.Symlink(filepath.Join(workspace, "rogue.json"), filepath.Join(dir, "rogue.json")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rogue", "../rogue", "./violet", "A/B", "a\x1b", "ink.json", "MISSING"} {
		if got, err := store.load(name); err == nil || got.name != terminalTheme {
			t.Errorf("accepted out-of-scope theme %q: %+v %v", name, got, err)
		}
	}
	writeThemeFixture(t, dir, "huge.json", string(bytes.Repeat([]byte{' '}, maxThemeBytes+1)))
	if _, err := store.load("huge"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize theme: %v", err)
	}
	preference := filepath.Join(store.home, ".hush", "theme")
	if err := os.Remove(preference); err != nil {
		t.Fatal(err)
	}
	nontheme := filepath.Join(workspace, "unchanged")
	writeThemeFixture(t, workspace, "unchanged", "sentinel")
	if err := os.Symlink(nontheme, preference); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(""); err == nil {
		t.Fatal("followed a preference symlink")
	}
	if err := store.save(t.Context(), chosen); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(nontheme); err != nil || string(raw) != "sentinel" {
		t.Fatalf("saving followed the preference symlink: %s %v", raw, err)
	}
	if info, err := os.Lstat(preference); err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("preference not a private regular file: %v %v", info, err)
	}
}
