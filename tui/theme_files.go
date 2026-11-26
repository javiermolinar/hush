package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image/color"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	terminalTheme = "terminal"
	maxThemeBytes = 8 << 10
	maxThemeCount = 64
)

type namedTheme struct {
	name   string
	colors themeColors
}

func validThemeName(name string) bool {
	if len(name) == 0 || len(name) > 48 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '-' || c == '_')) {
			return false
		}
	}
	return true
}

type themeFile struct {
	Version int               `json:"version"`
	Base    string            `json:"base"`
	Colors  map[string]string `json:"colors"`
}

func decodeTheme(name string, source []byte) (namedTheme, error) {
	if !utf8.Valid(source) {
		return namedTheme{}, errors.New("expected UTF-8")
	}
	var file themeFile
	if err := json.Unmarshal(source, &file, json.RejectUnknownMembers(true)); err != nil {
		return namedTheme{}, fmt.Errorf("invalid theme JSON: %w", err)
	}
	if file.Version != 1 {
		return namedTheme{}, errors.New("expected theme version 1")
	}
	if file.Base != terminalTheme {
		return namedTheme{}, errors.New("base must be terminal")
	}
	if file.Colors == nil {
		return namedTheme{}, errors.New("colors must be a JSON object")
	}
	out := namedTheme{name: name}
	for name, value := range file.Colors {
		var role colorRole
		valid := false
		for i, def := range colorRoles {
			if def.name != "" && def.name == name {
				role, valid = colorRole(i), true
				break
			}
		}
		if !valid {
			return namedTheme{}, fmt.Errorf("unknown color role %q", name)
		}
		if len(value) != 7 || value[0] != '#' {
			return namedTheme{}, fmt.Errorf("%s must be a #RRGGBB color", name)
		}
		rgb, err := hex.DecodeString(value[1:])
		if err != nil {
			return namedTheme{}, fmt.Errorf("%s must be a #RRGGBB color", name)
		}
		out.colors[role] = color.NRGBA{R: rgb[0], G: rgb[1], B: rgb[2], A: 255}
	}
	return out, nil
}

// These are illustrative foregrounds for a dark terminal, not new defaults.
// Keep the template's keys and the validator in sync via colorRoles.
var themeExample = [roleCount]string{
	roleControl:       "#babdd8",
	roleMuted:         "#a6adc8",
	roleAccent:        "#8b9df5",
	roleLink:          "#89dceb",
	roleSuccess:       "#a6e3a1",
	roleWarning:       "#f9e2af",
	roleError:         "#f38ba8",
	roleSyntaxKeyword: "#c69af7",
	roleSyntaxLiteral: "#94e2d5",
	roleSyntaxComment: "#a6adc8",
}

// ThemeTemplate returns an editable v1 JSON theme. Remove any override to use
// the corresponding terminal role. The template does not read local files.
func ThemeTemplate() string {
	file := themeFile{Version: 1, Base: terminalTheme, Colors: map[string]string{}}
	for i, def := range colorRoles {
		if def.name != "" {
			file.Colors[def.name] = themeExample[i]
		}
	}
	body, _ := json.Marshal(file, json.Deterministic(true), jsontext.WithIndent("  "))
	return string(body) + "\n"
}

type themeStore struct{ home string }

func openThemeDir(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("expected a directory (not a symbolic link)")
	}
	return root.OpenRoot(name)
}

func (s themeStore) agent(create bool) (*os.Root, error) {
	home, err := os.OpenRoot(s.home)
	if err != nil {
		return nil, err
	}
	defer home.Close()
	if create {
		if err := home.Mkdir(".hush", 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return openThemeDir(home, ".hush")
}

// Reject non-regular files, including symlinks, and bound the bytes read.
// A rooted open never resolves outside the opened directory.
func readThemeFile(root *os.Root, name string, maxBytes int) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("expected a regular file (not a symbolic link)")
	}
	f, err := openLocalThemeFile(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, errors.New("expected a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err == nil && len(raw) > maxBytes {
		err = fmt.Errorf("file exceeds %d bytes", maxBytes)
	}
	return raw, err
}

func readNamedTheme(dir *os.Root, name string) (namedTheme, error) {
	path := "~/.hush/themes/" + name + ".json"
	raw, err := readThemeFile(dir, name+".json", maxThemeBytes)
	if err == nil {
		var out namedTheme
		out, err = decodeTheme(name, raw)
		if err == nil {
			return out, nil
		}
	}
	return namedTheme{}, fmt.Errorf("%s: %w", path, err)
}

// Local files keep precedence, including validation errors for an existing
// file. Only a missing file can fall back to a preset of the same name.
func readThemeChoice(dir *os.Root, name string) (namedTheme, error) {
	chosen, err := readNamedTheme(dir, name)
	if errors.Is(err, fs.ErrNotExist) {
		if preset, ok := presetTheme(name); ok {
			return preset, nil
		}
	}
	return chosen, err
}

func resolveTheme(agent *os.Root, name string) (namedTheme, error) {
	themes, err := openThemeDir(agent, "themes")
	if errors.Is(err, fs.ErrNotExist) {
		if preset, ok := presetTheme(name); ok {
			return preset, nil
		}
	}
	if err != nil {
		return namedTheme{}, fmt.Errorf("open ~/.hush/themes for %q: %w", name, err)
	}
	defer themes.Close()
	return readThemeChoice(themes, name)
}

func (s themeStore) load(override string) (namedTheme, error) {
	fallback := namedTheme{name: terminalTheme}
	if override != "" && !validThemeName(override) {
		return fallback, errors.New("invalid --theme name: use lowercase letters, digits, hyphens and underscores")
	}
	if override == terminalTheme {
		return fallback, nil
	}
	agent, err := s.agent(false)
	if errors.Is(err, fs.ErrNotExist) {
		if override == "" {
			return fallback, nil
		}
		if preset, ok := presetTheme(override); ok {
			return preset, nil
		}
	}
	if err != nil {
		return fallback, fmt.Errorf("open ~/.hush: %w", err)
	}
	defer agent.Close()
	name := override
	if name == "" {
		current, err := readThemeFile(agent, "theme", 128)
		if errors.Is(err, fs.ErrNotExist) {
			return fallback, nil
		}
		if err != nil {
			return fallback, fmt.Errorf("read ~/.hush/theme: %w", err)
		}
		name = strings.TrimSpace(string(current))
		if name == terminalTheme {
			return fallback, nil
		}
		if !validThemeName(name) {
			return fallback, errors.New("~/.hush/theme: expected one valid theme name")
		}
	}
	chosen, err := resolveTheme(agent, name)
	if err != nil {
		return fallback, err
	}
	return chosen, nil
}

func (s themeStore) launch(override string) (namedTheme, error) {
	chosen, err := s.load(override)
	if err == nil {
		return chosen, nil
	}
	fallback := namedTheme{name: terminalTheme}
	if override != "" {
		if saved, savedErr := s.load(""); savedErr == nil {
			fallback = saved
		}
	}
	return fallback, err
}

type themeBook struct {
	choices []namedTheme
	problem string
}

func (s themeStore) list(ctx context.Context) themeBook {
	book := themeBook{choices: append([]namedTheme{{name: terminalTheme}}, presetThemes[:]...)}
	agent, err := s.agent(false)
	if errors.Is(err, fs.ErrNotExist) {
		return book
	}
	if err != nil {
		book.problem = "Open ~/.hush: " + err.Error()
		return book
	}
	defer agent.Close()
	dir, err := openThemeDir(agent, "themes")
	if errors.Is(err, fs.ErrNotExist) {
		return book
	}
	if err != nil {
		book.problem = "Open ~/.hush/themes: " + err.Error()
		return book
	}
	defer dir.Close()
	// Resolve preset names directly, even if an override would be outside
	// the bounded directory scan. Each effective name appears only once.
	book.choices = book.choices[:1]
	for _, preset := range presetThemes {
		theme, err := readThemeChoice(dir, preset.name)
		if err != nil {
			if book.problem == "" {
				book.problem = err.Error()
			}
			continue
		}
		book.choices = append(book.choices, theme)
	}
	f, err := dir.Open(".")
	if err != nil {
		book.problem = "List ~/.hush/themes: " + err.Error()
		return book
	}
	defer f.Close()
	const maxScanned = 512
	files, err := f.ReadDir(maxScanned + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		book.problem = "List ~/.hush/themes: " + err.Error()
		return book
	}
	if len(files) > maxScanned {
		book.problem = "Showing up to 512 entries in ~/.hush/themes"
		files = files[:maxScanned]
	}
	count := 0
	for _, file := range files {
		if ctx.Err() != nil {
			break
		}
		name, ok := strings.CutSuffix(file.Name(), ".json")
		if !ok || !validThemeName(name) || name == terminalTheme {
			continue
		}
		if _, preset := presetTheme(name); preset {
			continue // resolved above
		}
		t, err := readNamedTheme(dir, name)
		if err != nil {
			if book.problem == "" {
				book.problem = err.Error()
			}
			continue
		}
		if count == maxThemeCount {
			book.problem = "Showing up to 64 additional themes in ~/.hush/themes"
			break
		}
		book.choices = append(book.choices, t)
		count++
	}
	return book
}

func (s themeStore) save(ctx context.Context, chosen namedTheme) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := chosen.name
	if !validThemeName(name) {
		return errors.New("invalid theme name")
	}
	agent, err := s.agent(true)
	if err != nil {
		return err
	}
	defer agent.Close()
	// Don't publish a broken choice if the theme changed on disk while
	// previewing. The palette in memory remains the last usable one.
	if name != terminalTheme {
		onDisk, err := resolveTheme(agent, name)
		if err != nil {
			return err
		}
		if onDisk != chosen {
			return errors.New("theme changed on disk; reopen Themes to preview the edited file")
		}
	}
	// Replace, rather than follow or truncate, the preference name.
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".theme-" + hex.EncodeToString(nonce[:])
	f, err := agent.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer agent.Remove(tmp)
	_, writeErr := io.WriteString(f, name+"\n")
	if err := errors.Join(writeErr, f.Close(), ctx.Err()); err != nil {
		return err
	}
	return agent.Rename(tmp, "theme")
}

func clientThemeStore() (themeStore, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return themeStore{}, errors.New("themes require an absolute client home directory")
	}
	return themeStore{home: home}, nil
}
