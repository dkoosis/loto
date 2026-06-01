package store

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

var errNotRegular = errors.New("not a regular file")

// errMultiLinked is returned by stripWrite when the open fd's inode has
// more than one hardlink. validateFileTarget rejects Nlink>1 up front, but
// a racing process can add a link between validation and the fchmod
// (loto-ta02); re-checking on the open fd closes that TOCTOU.
var errMultiLinked = errors.New("multiple hardlinks")

// fchmodFn is a package-private indirection so tests can inject EPERM
// without an OS-specific fixture. Tests filter by f.Name() when needed.
var fchmodFn = func(f *os.File, mode os.FileMode) error {
	return f.Chmod(mode)
}

// afterOpenHook is a package-private indirection that fires inside both
// stripWrite and restoreWrite right after the fd is opened, before the fd is
// re-stat'd. Tests inject a racing hardlink here to exercise the
// validate→chmod TOCTOU deterministically. Production default is a no-op.
var afterOpenHook = func(string) {}

// safeOpenRegular opens path with O_NOFOLLOW and verifies the result is a
// regular file. This binds subsequent fchmod calls to the inode that was
// validated, closing the TOCTOU window where a symlink swap between Stat
// and Chmod could redirect chmod onto an attacker-chosen file.
//
// Returns the os.OpenFile error untouched so callers can distinguish
// ENOENT (treat as no-op for restore) from ELOOP (symlink — refuse).
func safeOpenRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, &fs.PathError{
			Op:   "open",
			Path: path,
			Err:  errNotRegular,
		}
	}
	return f, nil
}

// stripWrite removes all write bits (owner/group/other) from path.
// Refuses symlinks and non-regular files to prevent TOCTOU swap.
func stripWrite(path string) error {
	f, err := safeOpenRegular(path)
	if err != nil {
		return err
	}
	defer f.Close()
	afterOpenHook(path)
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// Re-check Nlink on the OPEN fd, not the path: a racing process can add
	// a hardlink between validateFileTarget's Lstat and this fchmod, which
	// would otherwise clear write bits on an attacker-chosen name sharing
	// the inode (loto-ta02). The fd binds the check to the inode we mutate.
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
		return &fs.PathError{Op: "stripwrite", Path: path, Err: errMultiLinked}
	}
	return fchmodFn(f, st.Mode().Perm()&^0o222)
}

// restoreWrite adds owner-write to path. Missing-file is a no-op
// (the file may have been deleted while held). Refuses symlinks and
// non-regular files.
//
// restoreWrite intentionally restores ONLY owner-write (mode | 0o200).
// loto does not preserve exact pre-lock modes; a file at 0o400 round-trips
// to 0o600. Documented trade per spec §"chmod policy (no stored mode)".
func restoreWrite(path string) error {
	f, err := safeOpenRegular(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	afterOpenHook(path)
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// Re-check Nlink on the OPEN fd, mirroring stripWrite (loto-ta02): a racing
	// process can hardlink the locked inode between the validated strip at
	// acquire and this restore at release/break/reclaim. Restoring owner-write
	// would then silently add write to an attacker-chosen name on the shared
	// inode. Refuse Nlink>1 so the caller audits a mode_restore_failed event
	// (loto-pduc).
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
		return &fs.PathError{Op: "restorewrite", Path: path, Err: errMultiLinked}
	}
	return fchmodFn(f, st.Mode().Perm()|0o200)
}
