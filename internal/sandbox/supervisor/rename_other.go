//go:build !linux

package supervisor

import (
	"errors"
	"os"
)

func renameExclusive(*os.Root, string, string) error {
	return errors.New("sandbox execution requires Linux")
}
