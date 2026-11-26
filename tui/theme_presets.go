package tui

import "image/color"

// The catalog maps palettes to Hush's semantic foreground roles. It never
// sets the default text or the terminal background, so each preset has a
// set for dark terminals (colors) and one for light terminals (light), and
// the reported background picks between them. Gruvbox and Rosé Pine colors
// that miss 4.5:1 on their palette's own background are lightened on dark
// sets and darkened on light ones. Rosé Pine strings use gold, as its
// upstream highlights do, and links use pine. See README for sources.
var presetThemes = [...]namedTheme{
	{name: "panther", colors: themeColors{
		roleControl:       themeRGB(0xe0d9f7),
		roleQuiet:         themeRGB(0xa59ab8),
		roleAccent:        themeRGB(0xb3a0ff),
		roleLink:          themeRGB(0x8ee2ee),
		roleSuccess:       themeRGB(0x8ee8b1),
		roleWarning:       themeRGB(0xffcf82),
		roleError:         themeRGB(0xffa3bc),
		roleSyntaxKeyword: themeRGB(0xf8a0df),
		roleSyntaxLiteral: themeRGB(0xc4f393),
		roleSyntaxComment: themeRGB(0xa59ab8),
	}, light: themeColors{
		roleControl:       themeRGB(0x443d56),
		roleQuiet:         themeRGB(0x6d627e),
		roleAccent:        themeRGB(0x705bb3),
		roleLink:          themeRGB(0x14747f),
		roleSuccess:       themeRGB(0x147949),
		roleWarning:       themeRGB(0x8d6100),
		roleError:         themeRGB(0xa24f69),
		roleSyntaxKeyword: themeRGB(0x9d4d89),
		roleSyntaxLiteral: themeRGB(0x4d750d),
		roleSyntaxComment: themeRGB(0x6d627e),
	}},
	// Gruvbox's bright colors on dark0, and its faded colors on light0.
	{name: "gruvbox", colors: themeColors{
		roleControl:       themeRGB(0xebdbb2),
		roleQuiet:         themeRGB(0xa89984),
		roleAccent:        themeRGB(0xfabd2f),
		roleLink:          themeRGB(0x83a598),
		roleSuccess:       themeRGB(0xb8bb26),
		roleWarning:       themeRGB(0xfe8019),
		roleError:         themeRGB(0xff565e),
		roleSyntaxKeyword: themeRGB(0xd3869b),
		roleSyntaxLiteral: themeRGB(0x8ec07c),
		roleSyntaxComment: themeRGB(0xa89984),
	}, light: themeColors{
		roleControl:       themeRGB(0x3c3836),
		roleQuiet:         themeRGB(0x75685d),
		roleAccent:        themeRGB(0x995c00),
		roleLink:          themeRGB(0x076678),
		roleSuccess:       themeRGB(0x726d00),
		roleWarning:       themeRGB(0xba3f00),
		roleError:         themeRGB(0x9d0006),
		roleSyntaxKeyword: themeRGB(0x8f3f71),
		roleSyntaxLiteral: themeRGB(0x3d7654),
		roleSyntaxComment: themeRGB(0x75685d),
	}},
	// Both Rosé Pine presets use Dawn on a light terminal, like upstream's
	// automatic variant.
	{name: "rose-pine", colors: themeColors{
		roleControl:       themeRGB(0xe0def4),
		roleQuiet:         themeRGB(0x908caa),
		roleAccent:        themeRGB(0xebbcba),
		roleLink:          themeRGB(0x65a7c3),
		roleSuccess:       themeRGB(0x9ccfd8),
		roleWarning:       themeRGB(0xf6c177),
		roleError:         themeRGB(0xeb6f92),
		roleSyntaxKeyword: themeRGB(0xc4a7e7),
		roleSyntaxLiteral: themeRGB(0xf6c177),
		roleSyntaxComment: themeRGB(0x908caa),
	}, light: rosePineDawn},
	{name: "rose-pine-moon", colors: themeColors{
		roleControl:       themeRGB(0xe0def4),
		roleQuiet:         themeRGB(0x908caa),
		roleAccent:        themeRGB(0xc4a7e7),
		roleLink:          themeRGB(0x58a8ca),
		roleSuccess:       themeRGB(0x9ccfd8),
		roleWarning:       themeRGB(0xf6c177),
		roleError:         themeRGB(0xeb6f92),
		roleSyntaxKeyword: themeRGB(0xea9a97),
		roleSyntaxLiteral: themeRGB(0xf6c177),
		roleSyntaxComment: themeRGB(0x908caa),
	}, light: rosePineDawn},
	{name: "keanu", colors: themeColors{
		roleControl:       themeRGB(0xc2f4cc),
		roleQuiet:         themeRGB(0x88b699),
		roleAccent:        themeRGB(0x63f57a),
		roleLink:          themeRGB(0x63ebe0),
		roleSuccess:       themeRGB(0xb4f477),
		roleWarning:       themeRGB(0xead879),
		roleError:         themeRGB(0xff9392),
		roleSyntaxKeyword: themeRGB(0xd5f97a),
		roleSyntaxLiteral: themeRGB(0x89ebad),
		roleSyntaxComment: themeRGB(0x77b497),
	}, light: themeColors{
		roleControl:       themeRGB(0x1c4929),
		roleQuiet:         themeRGB(0x447056),
		roleAccent:        themeRGB(0x007b00),
		roleLink:          themeRGB(0x00776f),
		roleSuccess:       themeRGB(0x4c6600),
		roleWarning:       themeRGB(0x7b6900),
		roleError:         themeRGB(0xad4a4c),
		roleSyntaxKeyword: themeRGB(0x5e7111),
		roleSyntaxLiteral: themeRGB(0x00662c),
		roleSyntaxComment: themeRGB(0x367258),
	}},
	{name: "kitt", colors: themeColors{
		roleControl:       themeRGB(0xf5e1dc),
		roleQuiet:         themeRGB(0xb2a099),
		roleAccent:        themeRGB(0xff5b5a),
		roleLink:          themeRGB(0xffa33c),
		roleSuccess:       themeRGB(0xbed88a),
		roleWarning:       themeRGB(0xffca77),
		roleError:         themeRGB(0xff918e),
		roleSyntaxKeyword: themeRGB(0xffa19b),
		roleSyntaxLiteral: themeRGB(0xffbf87),
		roleSyntaxComment: themeRGB(0xb2a099),
	}, light: themeColors{
		roleControl:       themeRGB(0x4d3d39),
		roleQuiet:         themeRGB(0x73635d),
		roleAccent:        themeRGB(0xcb242f),
		roleLink:          themeRGB(0x874000),
		roleSuccess:       themeRGB(0x5b7124),
		roleWarning:       themeRGB(0x906000),
		roleError:         themeRGB(0x9f5360),
		roleSyntaxKeyword: themeRGB(0xa5504d),
		roleSyntaxLiteral: themeRGB(0x935e1f),
		roleSyntaxComment: themeRGB(0x73635d),
	}},
}

var rosePineDawn = themeColors{
	roleControl:       themeRGB(0x464261),
	roleQuiet:         themeRGB(0x68637c),
	roleAccent:        themeRGB(0x76528c),
	roleLink:          themeRGB(0x286983),
	roleSuccess:       themeRGB(0x276853),
	roleWarning:       themeRGB(0x88580d),
	roleError:         themeRGB(0x983e54),
	roleSyntaxKeyword: themeRGB(0x994a48),
	roleSyntaxLiteral: themeRGB(0x88580d),
	roleSyntaxComment: themeRGB(0x68637c),
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
