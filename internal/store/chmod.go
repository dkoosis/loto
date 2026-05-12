package store

import (
	"errors"
	"io/fs"
	"os"
)

// chmodFn is a package-private indirection so tests can inject EPERM
// without an OS-specific fixture.
var chmodFn = os.Chmod

// stripWrite removes all write bits (owner/group/other) from path.
func stripWrite(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return chmodFn(path, st.Mode().Perm()&^0o222)
}

// restoreWrite adds owner-write to path. Missing-file is a no-op
// (the file may have been deleted while held).
//
// restoreWrite intentionally restores ONLY owner-write (mode | 0o200).
// loto does not preserve exact pre-lock modes; a file at 0o400 round-trips
// to 0o600. Documented trade per spec §"chmod policy (no stored mode)".
func restoreWrite(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return chmodFn(path, st.Mode().Perm()|0o200)
}
