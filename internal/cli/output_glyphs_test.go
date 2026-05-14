package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoBannedGlyphsInSource enforces design.md: only ✓ and ✗ severity glyphs
// are allowed in CLI output. ⚠ ℹ ✔ are banned. The rule is per-source because
// every output path eventually reaches a Claude consumer and the glyph
// vocabulary must stay closed.
func TestNoBannedGlyphsInSource(t *testing.T) {
	banned := []string{"⚠", "ℹ", "✔"}
	roots := []string{".", "../store", "../identity", "../domain", "../../cmd/loto"}

	for _, root := range roots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			t.Fatalf("abs %s: %v", root, err)
		}
		err = filepath.Walk(absRoot, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			scanner := bufio.NewScanner(f)
			lineNum := 0
			for scanner.Scan() {
				lineNum++
				line := scanner.Text()
				for _, g := range banned {
					if strings.Contains(line, g) {
						t.Errorf("%s:%d: banned glyph %q — see .claude/rules/design.md", path, lineNum, g)
					}
				}
			}
			return scanner.Err()
		})
		if err != nil {
			t.Fatalf("walk %s: %v", absRoot, err)
		}
	}
}
