package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testAnthropicKey is a CRAFTED FIXTURE (the Titus anthropic rule's own
// documented example), not a live credential. It must match the production
// format exactly — the rule is length-exact and entropy-aware, so a visibly
// fake value would not exercise the scrubber.
const testAnthropicKey = "sk-ant-api03-jSq6OMjv1syXaEUE0bvOckLe_GtCKy8lvZdko3eOJgV8TH-f2iyzRekyZNSby5d9ScikGYuqQhsrxML3X3N3rQ-XwQaQAAA"

// initTestRepo creates a git repo with a committed base state and returns
// (dir, baseSha). Base state: main.go (source), .env.test (tracked secret-ish
// fixture, stands in for upstream repos that legitimately commit one).
func initTestRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.name", "test")
	git("config", "user.email", "test@test.local")

	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n\nfunc main() {}\n")
	write(".env.test", "FIXTURE_FLAG=upstream-tracked-value\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	return dir, git("rev-parse", "HEAD")
}

func runAddAll(t *testing.T, dir string) {
	t.Helper()
	args := append([]string{"-C", dir, "add", "-A"}, gitAddExcludePathspecs(dir)...)
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git add with exclude pathspecs: %v\n%s", err, out)
	}
}

func lsFiles(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "ls-files").Output()
	if err != nil {
		t.Fatal(err)
	}
	set := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			set[line] = true
		}
	}
	return set
}

// Candidate-created credential files must never enter the index — at any
// depth — while normal candidate files and upstream-tracked fixtures do.
func TestSecretAddExcludePathspecs(t *testing.T) {
	dir, _ := initTestRepo(t)

	files := map[string]string{
		".env":             "ANTHROPIC_API_KEY=" + testAnthropicKey + "\n",
		"apps/api/.env":    "DB_PASSWORD=hunter2\n",
		".env.local":       "SECRET=low-entropy-value\n",
		"config/prod.env":  "TOKEN=abc\n",
		"id_rsa":           "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n",
		"deploy/.netrc":    "machine github.com login x password y\n",
		".git-credentials": "https://user:pass@github.com\n",
		"fix.go":           "package main // candidate's actual fix\n",
		"docs/notes.md":    "approach notes\n",
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runAddAll(t, dir)
	tracked := lsFiles(t, dir)

	for _, mustExclude := range []string{
		".env", "apps/api/.env", ".env.local", "config/prod.env",
		"id_rsa", "deploy/.netrc", ".git-credentials",
	} {
		if tracked[mustExclude] {
			t.Errorf("secret file %q was staged despite exclude pathspecs", mustExclude)
		}
	}
	for _, mustInclude := range []string{"fix.go", "docs/notes.md", "main.go", ".env.test"} {
		if !tracked[mustInclude] {
			t.Errorf("legitimate file %q missing from index", mustInclude)
		}
	}
}

func extractBundle(t *testing.T, bundlePath string) map[string][]byte {
	t.Helper()
	f, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = data
	}
	return out
}

// Changed files get content-scrubbed in the bundle; unchanged upstream files
// pass through byte-identical (they're public problem-repo content and may be
// load-bearing for server-side test runs).
func TestBundleScrubsChangedFilesOnly(t *testing.T) {
	dir, baseSha := initTestRepo(t)

	// Candidate hardcodes a real key into a new file and edits main.go cleanly.
	if err := os.WriteFile(filepath.Join(dir, "client.go"),
		[]byte("package main\n\nconst apiKey = \""+testAnthropicKey+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() { println(\"fixed\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runAddAll(t, dir)

	bundle, err := bundleWorkspace(dir, baseSha)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(bundle.Path)
	contents := extractBundle(t, bundle.Path)

	if c, ok := contents["client.go"]; !ok {
		t.Fatal("client.go missing from bundle")
	} else if strings.Contains(string(c), "jSq6OMjv1syX") {
		t.Errorf("hardcoded key survived into bundle: %s", c)
	} else if !strings.Contains(string(c), "const apiKey") {
		t.Errorf("non-secret code damaged: %s", c)
	}
	// Upstream-tracked fixture is unchanged → must be byte-identical even
	// though its content pattern-matches a KEY=value secret shape.
	if c := contents[".env.test"]; string(c) != "FIXTURE_FLAG=upstream-tracked-value\n" {
		t.Errorf("unchanged upstream fixture was modified: %q", c)
	}
	if c := contents["main.go"]; !strings.Contains(string(c), "fixed") {
		t.Errorf("changed main.go content wrong: %q", c)
	}
}

// With no determinable base, every file gets scrubbed (fail-safe).
func TestBundleScrubsEverythingWithoutBase(t *testing.T) {
	dir, _ := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "leak.txt"),
		[]byte("key: "+testAnthropicKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runAddAll(t, dir)

	// Single commit, no parent, empty baseSha → changedFileSet returns nil.
	bundle, err := bundleWorkspace(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(bundle.Path)
	contents := extractBundle(t, bundle.Path)
	if strings.Contains(string(contents["leak.txt"]), "jSq6OMjv1syX") {
		t.Errorf("secret survived no-base bundle: %s", contents["leak.txt"])
	}
}

// A git error must yield nil (scrub everything), never a partial set —
// a partial set would let unlisted changed files ship verbatim.
func TestChangedFileSetGitErrorFailsSafe(t *testing.T) {
	dir, _ := initTestRepo(t)
	if set := changedFileSet(dir, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); set != nil {
		t.Errorf("expected nil on git error, got %v", set)
	}
}

// Binary content in the changed set must pass through unmodified — regex
// splicing inside binaries corrupts them.
func TestBundleLeavesBinaryUntouched(t *testing.T) {
	dir, baseSha := initTestRepo(t)
	binary := append([]byte{0x00, 0x01, 0xFF, 0x00}, []byte(testAnthropicKey)...)
	if err := os.WriteFile(filepath.Join(dir, "fixture.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	runAddAll(t, dir)

	bundle, err := bundleWorkspace(dir, baseSha)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(bundle.Path)
	contents := extractBundle(t, bundle.Path)
	if got := contents["fixture.bin"]; string(got) != string(binary) {
		t.Errorf("binary file modified: %v", got)
	}
}
