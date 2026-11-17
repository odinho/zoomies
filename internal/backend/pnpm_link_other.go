//go:build !linux

package backend

import (
	"errors"
	"os"
)

func linkPNPMFile(_ *os.File, _ *os.Root, _ string) error {
	return errors.ErrUnsupported
}
