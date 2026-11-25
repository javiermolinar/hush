package filelist

import "golang.org/x/sys/unix"

func exchangePlanFile(from int, source string, to int, destination string) error {
	return unix.RenameatxNp(from, source, to, destination, unix.RENAME_SWAP)
}
