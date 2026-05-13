package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "update golden files")

func executeCommand(args ...string) (stdout, stderr string, code int) {
	var outBuf, errBuf bytes.Buffer
	code = Run(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

func normalizeHelp(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	s = strings.Join(lines, "\n")
	s = strings.TrimRight(s, "\n") + "\n"
	return s
}

func readGolden(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	return string(b)
}

func writeGolden(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func helpCommandNames() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		if _, stderr, _ := executeCommand(name, "-h"); strings.Contains(stderr, "Usage of "+name+":") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func TestHelpGolden(t *testing.T) {
	type tc struct {
		name   string
		args   []string
		golden string
	}
	names := helpCommandNames()
	tests := make([]tc, 0, 1+len(names))
	tests = append(tests, tc{name: "root", args: nil, golden: "root.golden"})
	for _, name := range names {
		tests = append(tests, tc{
			name:   name,
			args:   []string{name, "-h"},
			golden: name + ".golden",
		})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, _ := executeCommand(tc.args...)
			got := normalizeHelp(stdout + stderr)
			goldenPath := filepath.Join("testdata", "help", tc.golden)
			if *update {
				if err := writeGolden(goldenPath, got); err != nil {
					t.Fatalf("write golden %s: %v", goldenPath, err)
				}
				return
			}
			want := readGolden(t, goldenPath)
			if got != want {
				t.Fatalf("help output mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", tc.name, got, want)
			}
		})
	}
}
