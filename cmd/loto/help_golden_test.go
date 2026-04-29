package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "update golden files")

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

func writeGolden(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write golden %s: %v", path, err)
	}
}

func captureUsage(t *testing.T) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	usage()
	_ = w.Close()
	os.Stderr = orig
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	return buf.String()
}

func TestHelpGolden(t *testing.T) {
	help := normalizeHelp(captureUsage(t))
	goldenPath := filepath.Join("testdata", "help", "root.golden")

	if *update {
		writeGolden(t, goldenPath, help)
	}

	want := normalizeHelp(readGolden(t, goldenPath))
	if help != want {
		t.Fatalf("help output mismatch for root\n--- got ---\n%s\n--- want ---\n%s", help, want)
	}
}
