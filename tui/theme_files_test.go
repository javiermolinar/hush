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
