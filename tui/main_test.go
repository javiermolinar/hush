package tui

import (
	"fmt"
	"os"
	"testing"
)

// Client preferences such as the theme and fills live under the home
// directory. Run the package against an empty one, so a developer's saved
// choices can't change frames. Tests that need a home still set their own.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "hush-tui-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
