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

// taskRootWithinRepo is shared by the clone path and the adopt path precisely
// because they used to compute the task boundary independently — and the adopt
// path simply did not, so a hosted monorepo assessment captured, diffed and
// bundled the whole repository.
func TestTaskRootWithinRepo(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "w", "repo")
	cases := []struct {
		name   string
		subdir string
		want   string
	}{
		{"no subdir is the repo root", "", root},
		{"blank subdir is the repo root", "   ", root},
		{"dot is the repo root", ".", root},
		{"a plain subdir", "services/api", filepath.Join(root, "services", "api")},
		{"an untidy subdir is cleaned", "./services//api/", filepath.Join(root, "services", "api")},
		{"a leading separator is not an absolute path", string(filepath.Separator) + "services", filepath.Join(root, "services")},
		// The server supplies this value, but a task root outside the repository
		// would bundle the candidate's home directory. The root is the safe answer.
		{"climbing out falls back to the root", "../elsewhere", root},
		{"a bare .. falls back to the root", "..", root},
		{"a name merely starting with dots is kept", "..hidden", filepath.Join(root, "..hidden")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskRootWithinRepo(root, c.subdir); got != c.want {
				t.Fatalf("taskRootWithinRepo(%q, %q) = %q, want %q", root, c.subdir, got, c.want)
			}
		})
	}
}
