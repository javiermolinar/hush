//go:build unix

package tui

import (
	"os"

	"golang.org/x/sys/unix"
)

func openLocalThemeFile(root *os.Root, name string) (*os.File, error) {
	// Do not follow or block on a file replaced between Lstat and open.
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
