package identity

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
)

var errInvalidSessionID = errors.New("invalid session id")

// homeDir returns the user's home directory, preferring os.UserHomeDir ($HOME)
// but falling back to os/user.Current().HomeDir (getpwuid_r) when $HOME is
// unset. Without this fallback, an empty $HOME yields relative ".loto/session"
// paths whose meaning changes with cwd — fragmenting identity across
// directories (gh#112 / loto-3axo).
func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return "/tmp" // both lookups failed; /tmp keeps paths absolute
}

// lotoHome returns the root directory for loto's per-user identity state:
// ~/.loto normally, or LOTO_BASE when set.
//
// LOTO_BASE already redirects the lock store (StateDir, internal/cli/paths.go)
// for tests and isolated runs; before sd-kx5 it did NOT reach identity, so a
// test setting LOTO_BASE still wrote session files under the real ~/.loto.
// One knob now covers the whole state dir: the lock store lands in LOTO_BASE
// directly (loto.db); the session dir lands in LOTO_BASE/session.
func lotoHome() string {
	if v := os.Getenv("LOTO_BASE"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".loto")
}

func sessionDir() string {
	return filepath.Join(lotoHome(), "session")
}

// sessionPath validates sid (must not contain path separators or '..')
// and returns the absolute path of its record file. Rejecting traversal here
// keeps callers tight and silences gosec G304/G703.
func sessionPath(sid string) (string, error) {
	if sid == "" || strings.ContainsAny(sid, `/\`) || strings.Contains(sid, "..") {
		return "", errInvalidSessionID
	}
	return filepath.Join(sessionDir(), sid+".json"), nil
}

// syncDir flushes a directory's metadata to stable storage so that a rename
// or O_EXCL create performed inside it survives power loss. Call after the
// file itself has been fsync'd. (Duplicated in internal/cli rather than shared
// via a helper package: identity must import no internal package — see
// .go-arch-lint.yml. The helper is small enough to fall under jscpd limits.)
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// mkdirAllSync is os.MkdirAll(dir, 0o700) plus an fsync of every newly-created
// level's parent, so each new directory entry survives power loss (loto-4n65,
// same durability class as loto-cq6). A pre-existing directory is a no-op — no
// extra fsync. On a fresh home MkdirAll creates more than one level (e.g.
// ~/.loto then ~/.loto/session); fsyncing only the immediate parent would leave
// the higher entries unflushed, so we walk from dir up to the first existing
// ancestor and fsync each created level's parent. A path that exists as a
// non-directory falls through to MkdirAll, which surfaces the real "not a
// directory" error rather than being masked. 0o700 is fixed: every identity
// dir under ~/.loto is user-private.
func mkdirAllSync(dir string) error {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	// Levels that don't yet exist, deepest first, up to the first existing
	// ancestor (or the filesystem root). Each level's parent gets fsync'd
	// after MkdirAll so the new directory entry is durable.
	var created []string
	for p := dir; ; {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			break
		}
		created = append(created, p)
		parent := filepath.Dir(p)
		if parent == p { // filesystem root; no further ancestors
			break
		}
		p = parent
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Fsync top-down (shallowest parent first): a level's entry must be durable
	// in its parent before that level's own contents are flushed, else a crash
	// mid-walk can leave a directory whose contents are persisted but whose link
	// from the parent is not — an orphaned inode. created is deepest-first, so
	// walk it in reverse.
	for _, p := range slices.Backward(created) {
		if err := syncDir(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}
