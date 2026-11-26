package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CSI data can also be inspected by a component (for example, a child view
// normalizer). Match assembled SGR prefixes, not every occurrence of ESC.
var rawPaint = regexp.MustCompile(`(?:#[[:xdigit:]]{6,8}\b|(?:\x1b\[|\x9b)[0-9;:]*m|\x1b\[(?:38|48);)`)

func TestComponentStylesStayInTheme(t *testing.T) {
	allowed := map[string]bool{
		"color_roles.go": true, "theme.go": true, "theme_files.go": true, "theme_presets.go": true, "theme_intro.go": true,
		"image_pixels.go": true, // pixels and image IDs, not UI styling
	}
	styleImports := map[string]bool{"image/color": true, "charm.land/lipgloss/v2": true, "github.com/charmbracelet/lipgloss": true, "github.com/muesli/termenv": true}
	styleSelectors := map[string]bool{
		"NewStyle": true, "Foreground": true, "Background": true, "ForegroundColor": true, "BackgroundColor": true,
		"Bold": true, "Faint": true, "Italic": true, "Reverse": true, "Underline": true, "Strikethrough": true,
		"SetStyle": true, "SetForegroundColor": true, "SetBackgroundColor": true,
	}
	fset := token.NewFileSet()
	if err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (path == "mock" || path == "examples" || path == "testdata") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || allowed[path] {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		stylePackage := map[string]bool{}
		for _, spec := range file.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			if styleImports[importPath] {
				t.Errorf("%s: component imports raw style package %s", fset.Position(spec.Pos()), importPath)
			}
			if importPath == "github.com/charmbracelet/x/ansi" || importPath == "github.com/charmbracelet/ultraviolet" {
				name := filepath.Base(importPath)
				if spec.Name != nil {
					name = spec.Name.Name
				}
				stylePackage[name] = true
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					str, _ := strconv.Unquote(n.Value)
					if rawPaint.MatchString(str) {
						t.Errorf("%s: raw paint outside theme roles: %q", fset.Position(n.Pos()), str)
					}
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && styleSelectors[sel.Sel.Name] {
					id, isPackage := sel.X.(*ast.Ident)
					if !isPackage || stylePackage[id.Name] || sel.Sel.Name != "Background" && sel.Sel.Name != "Reverse" {
						t.Errorf("%s: set %s through a theme role", fset.Position(sel.Pos()), sel.Sel.Name)
					}
				}
			}
			return true
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
