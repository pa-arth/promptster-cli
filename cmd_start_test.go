package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFilesEqualSameSizeDifferentContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(src, []byte("abcdef"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("abcxyz"), 0o755); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	if filesEqual(src, dst) {
		t.Fatal("filesEqual returned true for different same-size content")
	}
}

func TestInstallBinaryOverwritesStaleSameSizeBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	destDir := filepath.Join(dir, "bin")
	dest := filepath.Join(destDir, "promptster")

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("mkdir destDir: %v", err)
	}
	if err := os.WriteFile(src, []byte("fresh!"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dest, []byte("stale!"), 0o755); err != nil {
		t.Fatalf("write dest: %v", err)
	}

	if err := installBinary(src, dest); err != nil {
		t.Fatalf("installBinary: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "fresh!" {
		t.Fatalf("dest contents = %q, want %q", string(got), "fresh!")
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("dest mode = %o, want 755", info.Mode().Perm())
	}
}
