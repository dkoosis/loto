package store

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

var errNotRegular = errors.New("not a regular file")

// errMultiLinked is returned by restoreWrite when the open fd's inode has
// more than one hardlink. A racing process can add a link between the Lstat
// and the fchmod (loto-ta02); re-checking on the open fd closes that TOCTOU.
var errMultiLinked = errors.New("multiple hardlinks")

// fchmodFn is a package-private indirection so tests can inject EPERM
// without an OS-specific fixture. Tests filter by f.Name() when needed.
var fchmodFn = func(f *os.File, mode os.FileMode) error {
	return f.Chmod(mode)
}

// afterOpenHook is a package-private indirection that fires inside restoreWrite
// right after the fd is opened, before the fd is re-stat'd. Tests inject a
// racing hardlink here to exercise the validate→chmod TOCTOU
// deterministically. Production default is a no-op.
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

// permAfterNlinkCheck stats an open fd and re-checks Nlink on it — the
// validate→chmod TOCTOU guard restoreWrite needs. A racing process can add a
// hardlink between the Lstat and the fchmod,
// which would otherwise redirect the mode change onto an attacker-chosen name
// sharing the inode (loto-ta02). Binding the check to the open fd closes that
// window. op names the caller for the PathError; returns the inode's current
// permission bits for the caller to mask.
func permAfterNlinkCheck(f *os.File, path, op string) (os.FileMode, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
		return 0, &fs.PathError{Op: op, Path: path, Err: errMultiLinked}
	}
	return st.Mode().Perm(), nil
}

// restoreWrite adds owner-write to path. Missing-file is a no-op (the file may
// have been deleted since). Refuses symlinks and non-regular files.
//
// ‡ Sole surviving caller: doctor's chmod-era migration (loto-zssw). loto no
// longer strips a write bit anywhere — peer protection is carried entirely by
// the harness gate — so this exists to undo bits an older loto set and left
// behind when a session died between acquire and release.
//
// It restores ONLY owner-write (mode | 0o200). loto never stored the pre-lock
// mode, so a file that was deliberately 0o400 round-trips to 0o600. That was
// the trade when the strip shipped; it is now paid once, at migration.
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
	// Mirror stripWrite's open-fd Nlink guard: a racing process can hardlink the
	// locked inode between the validated strip at acquire and this restore at
	// release/break/reclaim. Restoring owner-write would then silently add write
	// to an attacker-chosen name on the shared inode. Refusing Nlink>1 makes the
	// caller audit a mode_restore_failed event (loto-pduc).
	perm, err := permAfterNlinkCheck(f, path, "restorewrite")
	if err != nil {
		return err
	}
	return fchmodFn(f, perm|0o200)
}
