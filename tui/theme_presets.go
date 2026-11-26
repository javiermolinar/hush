package tui

import "image/color"

// The catalog maps palettes to Hush's semantic foreground roles. It never
// sets the default text or the terminal background. See README for sources.
var presetThemes = [...]namedTheme{
	{name: "panther", colors: themeColors{
		roleControl:       themeRGB(0xe0d9f7),
		roleMuted:         themeRGB(0xa59ab8),
		roleAccent:        themeRGB(0xb3a0ff),
		roleLink:          themeRGB(0x8ee2ee),
		roleSuccess:       themeRGB(0x8ee8b1),
		roleWarning:       themeRGB(0xffcf82),
		roleError:         themeRGB(0xffa3bc),
		roleSyntaxKeyword: themeRGB(0xf8a0df),
		roleSyntaxLiteral: themeRGB(0xc4f393),
		roleSyntaxComment: themeRGB(0xa59ab8),
	}},
	{name: "gruvbox-dark", colors: themeColors{
		roleControl:       themeRGB(0xebdbb2),
		roleMuted:         themeRGB(0xa89984),
		roleAccent:        themeRGB(0xfabd2f),
		roleLink:          themeRGB(0x83a598),
		roleSuccess:       themeRGB(0xb8bb26),
		roleWarning:       themeRGB(0xfe8019),
		roleError:         themeRGB(0xfb4934),
		roleSyntaxKeyword: themeRGB(0xd3869b),
		roleSyntaxLiteral: themeRGB(0x8ec07c),
		roleSyntaxComment: themeRGB(0xa89984),
	}},
	{name: "rose-pine", colors: themeColors{
		roleControl:       themeRGB(0xe0def4),
		roleMuted:         themeRGB(0x908caa),
		roleAccent:        themeRGB(0xebbcba),
		roleLink:          themeRGB(0x9ccfd8),
		roleSuccess:       themeRGB(0x9ccfd8),
		roleWarning:       themeRGB(0xf6c177),
		roleError:         themeRGB(0xeb6f92),
		roleSyntaxKeyword: themeRGB(0xc4a7e7),
		roleSyntaxLiteral: themeRGB(0xebbcba),
		roleSyntaxComment: themeRGB(0x908caa),
	}},
	{name: "rose-pine-moon", colors: themeColors{
		roleControl:       themeRGB(0xe0def4),
		roleMuted:         themeRGB(0x908caa),
		roleAccent:        themeRGB(0xc4a7e7),
		roleLink:          themeRGB(0x9ccfd8),
		roleSuccess:       themeRGB(0xa7d4c3),
		roleWarning:       themeRGB(0xf6c177),
		roleError:         themeRGB(0xeb6f92),
		roleSyntaxKeyword: themeRGB(0xea9a97),
		roleSyntaxLiteral: themeRGB(0x9ccfd8),
		roleSyntaxComment: themeRGB(0x908caa),
	}},
	{name: "rose-pine-dawn", colors: themeColors{
		roleControl:       themeRGB(0x464261),
		roleMuted:         themeRGB(0x68637c),
		roleAccent:        themeRGB(0x76528c),
		roleLink:          themeRGB(0x286983),
		roleSuccess:       themeRGB(0x276853),
		roleWarning:       themeRGB(0x88580d),
		roleError:         themeRGB(0x93425a),
		roleSyntaxKeyword: themeRGB(0x86506c),
		roleSyntaxLiteral: themeRGB(0x286983),
		roleSyntaxComment: themeRGB(0x68637c),
	}},
	{name: "keanu", colors: themeColors{
		roleControl:       themeRGB(0xc2f4cc),
		roleMuted:         themeRGB(0x88b699),
		roleAccent:        themeRGB(0x63f57a),
		roleLink:          themeRGB(0x74ebc9),
		roleSuccess:       themeRGB(0xb4f477),
		roleWarning:       themeRGB(0xead879),
		roleError:         themeRGB(0xff9392),
		roleSyntaxKeyword: themeRGB(0xd5f97a),
		roleSyntaxLiteral: themeRGB(0x89ebad),
		roleSyntaxComment: themeRGB(0x77b497),
	}},
	{name: "kitt", colors: themeColors{
		roleControl:       themeRGB(0xf5e1dc),
		roleMuted:         themeRGB(0xb2a099),
		roleAccent:        themeRGB(0xff716a),
		roleLink:          themeRGB(0xffb073),
		roleSuccess:       themeRGB(0xbed88a),
		roleWarning:       themeRGB(0xffca77),
		roleError:         themeRGB(0xff918e),
		roleSyntaxKeyword: themeRGB(0xffa19b),
		roleSyntaxLiteral: themeRGB(0xffbf87),
		roleSyntaxComment: themeRGB(0xb2a099),
	}},
}

func themeRGB(hex uint32) color.NRGBA {
	return color.NRGBA{R: uint8(hex >> 16), G: uint8(hex >> 8), B: uint8(hex), A: 255}
}

func presetTheme(name string) (namedTheme, bool) {
	for _, theme := range presetThemes {
		if theme.name == name {
			return theme, true
		}
	}
	return namedTheme{}, false
}
