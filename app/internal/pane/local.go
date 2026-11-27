package pane

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"

	"github.com/javiermolinar/hush/sdk"
)

var errLocalDirectory = errors.New("the working directory does not match the client filesystem")

func localIdentity(path string, kind fs.FileMode) (sdk.PaneFileIdentity, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Type() != kind {
		return sdk.PaneFileIdentity{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return sdk.PaneFileIdentity{}, false
	}
	birth, ok := localBirth(path, st)
	if !ok {
		return sdk.PaneFileIdentity{}, false
	}
	return sdk.PaneFileIdentity{Device: strconv.FormatUint(uint64(st.Dev), 10), Inode: strconv.FormatUint(st.Ino, 10), Birth: strconv.FormatInt(birth, 10)}, true
}

// Matching the pathname alone must not authorize a launch.
func checkDirectory(path string, want sdk.PaneFileIdentity) error {
	if actual, ok := localIdentity(path, fs.ModeDir); !ok || actual != want {
		return errLocalDirectory
	}
	return nil
}
