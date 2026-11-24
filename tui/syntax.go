package tui

import (
	"slices"
	"strings"
)

// The tokenizer adapted from the approved hush mock carries state between code lines.
type syntax struct {
	line     []string    // line comment openers
	block    [][2]string // block comment openers and closers
	quotes   []string    // string delimiters, longest first
	raw      []string    // delimiters whose strings have no escapes
	multi    []string    // delimiters whose strings may span lines
	keywords map[string]bool
	fold     bool // keywords ignore case (SQL, Dockerfile)
	hashWord bool // # starts a comment only at the start of a word (shells)
	tags     bool // markup: <name …> names are keywords
	lifetime bool // Rust: 'a is a lifetime, not a character
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(s) {
		out[w] = true
	}
	return out
}

var (
	jsWords = words(`break case catch class const continue debugger default delete do else export extends finally for
		function if import in instanceof let new return super switch this throw try typeof var void while with yield
		async await of static get set enum implements interface package private protected public type declare
		namespace abstract as from readonly keyof infer is satisfies true false null undefined`)
	syntaxes = map[string]*syntax{}
)

func init() {
	js := &syntax{line: []string{"//"}, block: [][2]string{{"/*", "*/"}}, quotes: []string{"`", `"`, `'`}, multi: []string{"`"},
		keywords: jsWords}
	golang := &syntax{line: []string{"//"}, block: [][2]string{{"/*", "*/"}}, quotes: []string{"`", `"`, `'`},
		raw: []string{"`"}, multi: []string{"`"}, keywords: words(`break case chan const continue default defer else fallthrough for func go
		goto if import interface map package range return select struct switch type var true false nil iota`)}
	rust := &syntax{line: []string{"//"}, block: [][2]string{{"/*", "*/"}}, quotes: []string{`"`, `'`}, lifetime: true,
		keywords: words(`as async await break const continue crate dyn else enum extern false fn for if impl in let loop
		match mod move mut pub ref return self Self static struct super trait true type unsafe use where while`)}
	c := &syntax{line: []string{"//"}, block: [][2]string{{"/*", "*/"}}, quotes: []string{`"`, `'`}}
	c.keywords = words(`auto break case char const continue default do double else enum extern float for goto if inline
		int long register return short signed sizeof static struct switch typedef union unsigned void volatile while
		class public private protected new delete this try catch throw namespace using template typename virtual
		override final import package interface extends implements boolean byte true false null nullptr fun val var
		when object func guard let struct protocol extension self super is as in`)
	python := &syntax{line: []string{"#"}, quotes: []string{`"""`, `'''`, `"`, `'`}, multi: []string{`"""`, `'''`},
		keywords: words(`False None True and as assert async await break class continue def del elif else except
		finally for from global if import in is lambda nonlocal not or pass raise return try while with yield match
		case self`)}
	shell := &syntax{line: []string{"#"}, quotes: []string{`"`, `'`}, raw: []string{`'`}, multi: []string{`"`, `'`}, hashWord: true,
		keywords: words(`if then else elif fi case esac for while until do done in function select return exit
		export local readonly declare unset source alias`)}
	ruby := &syntax{line: []string{"#"}, quotes: []string{`"`, `'`}, keywords: words(`def end class module if elsif
		else unless while until do return yield self nil true false require begin rescue ensure then case when in
		and or not`)}
	sql := &syntax{line: []string{"--"}, block: [][2]string{{"/*", "*/"}}, quotes: []string{`'`, `"`}, fold: true,
		keywords: words(`select from where and or not insert into values update set delete create table alter add
		column drop index primary key foreign references join left right inner outer full on group by order having
		limit offset as null is in exists default unique begin commit rollback transaction constraint if case when
		then else end distinct union all returning with check cascade`)}
	lua := &syntax{line: []string{"--"}, block: [][2]string{{"--[[", "]]"}}, quotes: []string{`"`, `'`},
		keywords: words(`and break do else elseif end false for function goto if in local nil not or repeat return
		then true until while`)}
	data := &syntax{line: []string{"#"}, quotes: []string{`"""`, `"`, `'`}, multi: []string{`"""`},
		keywords: words(`true false null yes no on off`)} // YAML, TOML
	json := &syntax{quotes: []string{`"`}, keywords: words(`true false null`)}
	css := &syntax{block: [][2]string{{"/*", "*/"}}, quotes: []string{`"`, `'`}, keywords: words(`!important`)}
	markup := &syntax{block: [][2]string{{"<!--", "-->"}}, quotes: []string{`"`, `'`}, tags: true}
	docker := &syntax{line: []string{"#"}, quotes: []string{`"`, `'`}, fold: true, keywords: words(`from run cmd copy add
		env arg workdir expose entrypoint user volume label healthcheck shell onbuild stopsignal as`)}
	for _, l := range []struct {
		names string
		s     *syntax
	}{
		{"ts tsx typescript js jsx javascript mjs cjs", js},
		{"go golang", golang},
		{"rs rust", rust},
		{"c h cpp cc cxx hpp c++ cs csharp java kotlin kt swift scala dart php objc", c},
		{"py python python3 py3", python},
		{"sh bash zsh shell console fish ksh", shell},
		{"rb ruby", ruby},
		{"sql psql postgres postgresql mysql sqlite", sql},
		{"lua", lua},
		{"yaml yml toml ini conf", data},
		{"json jsonc json5", json},
		{"css scss sass less", css},
		{"html htm xml svg vue xhtml", markup},
		{"dockerfile docker containerfile", docker},
	} {
		for _, n := range strings.Fields(l.names) {
			syntaxes[n] = l.s
		}
	}
}

// hlState carries what a line leaves open into the next: a block comment's closer, or a string's delimiter.
type codeSpan struct{ s, ink string }

type hlState struct {
	comment, str string
}

func (sx *syntax) line1(l string, st hlState) ([]codeSpan, hlState) {
	var out []codeSpan
	emit := func(s string, style codeSpan) {
		if s == "" {
			return
		}
		if n := len(out); n > 0 && out[n-1].ink == style.ink {
			out[n-1].s += s
			return
		}
		style.s = s
		out = append(out, style)
	}
	comment, literal, keyword := codeSpan{ink: "\x1b[2;3m"}, codeSpan{ink: "\x1b[36m"}, codeSpan{ink: "\x1b[35m"}
	i, plainFrom := 0, 0
	flush := func() { emit(l[plainFrom:i], codeSpan{}) }
	// what the line above left open
	if st.comment != "" {
		end := strings.Index(l, st.comment)
		if end < 0 {
			emit(l, comment)
			return out, st
		}
		i = end + len(st.comment)
		emit(l[:i], comment)
		st.comment, plainFrom = "", i
	} else if st.str != "" {
		end := sx.closeString(l, 0, st.str)
		if end < 0 {
			emit(l, literal)
			return out, st
		}
		i = end
		emit(l[:i], literal)
		st.str, plainFrom = "", i
	}
	for i < len(l) {
		c := l[i]
		if sx.tags && c == '<' && i+1 < len(l) && (isIdentStart(l[i+1]) || l[i+1] == '/') {
			flush()
			j := i + 1
			if l[j] == '/' {
				j++
			}
			k := j
			for k < len(l) && (isIdentByte(l[k]) || l[k] == '-' || l[k] == ':') {
				k++
			}
			emit(l[i:j], codeSpan{})
			emit(l[j:k], keyword)
			i, plainFrom = k, k
			continue
		}
		if opener, closer, ok := sx.blockAt(l, i); ok {
			flush()
			end := strings.Index(l[i+len(opener):], closer)
			if end < 0 {
				emit(l[i:], comment)
				st.comment = closer
				return out, st
			}
			stop := i + len(opener) + end + len(closer)
			emit(l[i:stop], comment)
			i, plainFrom = stop, stop
			continue
		}
		if sx.lineAt(l, i) {
			flush()
			emit(l[i:], comment)
			return out, st
		}
		if q := sx.quoteAt(l, i); q != "" {
			flush()
			end := sx.closeString(l, i+len(q), q)
			if end < 0 {
				emit(l[i:], literal)
				if slices.Contains(sx.multi, q) {
					st.str = q
				}
				return out, st
			}
			emit(l[i:end], literal)
			i, plainFrom = end, end
			continue
		}
		if c >= '0' && c <= '9' && (i == 0 || !isIdentByte(l[i-1])) {
			flush()
			j := i + 1
			for j < len(l) && (isIdentByte(l[j]) || l[j] == '.' && j+1 < len(l) && l[j+1] >= '0' && l[j+1] <= '9') {
				j++
			}
			emit(l[i:j], literal)
			i, plainFrom = j, j
			continue
		}
		if isIdentStart(c) || c == '!' && sx.keywords["!important"] {
			j := i + 1
			for j < len(l) && isIdentByte(l[j]) {
				j++
			}
			w := l[i:j]
			if sx.fold {
				w = strings.ToLower(w)
			}
			if sx.keywords[w] && (i == 0 || l[i-1] != '.') { // obj.type is a name
				flush()
				emit(l[i:j], keyword)
				plainFrom = j
			}
			i = j
			continue
		}
		i++
	}
	flush()
	return out, st
}

func (sx *syntax) blockAt(l string, i int) (string, string, bool) {
	for _, b := range sx.block {
		if strings.HasPrefix(l[i:], b[0]) {
			return b[0], b[1], true
		}
	}
	return "", "", false
}

func (sx *syntax) lineAt(l string, i int) bool {
	for _, p := range sx.line {
		if strings.HasPrefix(l[i:], p) {
			if sx.hashWord && p == "#" && i > 0 && l[i-1] != ' ' && l[i-1] != '\t' {
				return false // ${#var}, a#b: not a comment
			}
			return true
		}
	}
	return false
}

func (sx *syntax) quoteAt(l string, i int) string {
	for _, q := range sx.quotes {
		if !strings.HasPrefix(l[i:], q) {
			continue
		}
		if sx.lifetime && q == "'" { // 'a' and '\n' are characters; 'a alone is a lifetime
			if end := sx.closeString(l, i+1, q); end < 0 || end-i > 4 {
				return ""
			}
		}
		return q
	}
	return ""
}

// closeString returns the index just past the delimiter q that closes a string starting at from, or -1.
func (sx *syntax) closeString(l string, from int, q string) int {
	escapes := !slices.Contains(sx.raw, q)
	for i := from; i < len(l); i++ {
		if escapes && l[i] == '\\' {
			i++
			continue
		}
		if strings.HasPrefix(l[i:], q) {
			return i + len(q)
		}
	}
	return -1
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentByte(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }
