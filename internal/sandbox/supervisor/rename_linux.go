//go:build linux

package supervisor

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func renameExclusive(root *os.Root, oldName, newName string) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // The directory descriptor is only used for the rename.
	err = unix.Renameat2(int(dir.Fd()), oldName, int(dir.Fd()), newName, unix.RENAME_NOREPLACE)
	if errors.Is(err, syscall.EEXIST) {
		return os.ErrExist
	}
	return err
}
