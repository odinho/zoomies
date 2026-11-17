package backend

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Both file descriptors were opened through roots. Linking through /proc keeps
// a concurrent cache writer from redirecting the agent through a parent symlink.
func linkPNPMFile(source *os.File, target *os.Root, name string) error {
	parent, err := target.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer parent.Close()
	err = unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", source.Fd()), int(parent.Fd()), filepath.Base(name), unix.AT_SYMLINK_FOLLOW)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}
