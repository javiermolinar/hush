//go:build !unix

package tui

import "os"

func openLocalThemeFile(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
