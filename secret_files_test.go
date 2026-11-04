package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Copied verbatim from promptster-hire-backend
// packages/shared/src/fileFilters.ts (EXCLUDED_FILE_NAMES,
// EXCLUDED_SECRET_EXTENSIONS, EXCLUDED_SECRET_DIR_PREFIXES, ENV_TEMPLATE_SUFFIXES)
// as of origin/main 2026-11-04. When the server list changes, update this
// fixture AND fileFilter.go; this test fails until both agree.
var (
	serverSecretFileNames = []string{
		".npmrc", ".yarnrc", ".netrc", "_netrc", ".pgpass", ".htpasswd", ".dockercfg",
		".mcp.json", "settings.local.json", "credentials", "credentials.json",
		"service-account.json", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
		"secrets.yml", "secrets.yaml", "secrets.json",
	}
	serverSecretExtensions    = []string{"pem", "key", "p12", "pfx", "jks", "keystore", "ppk", "asc", "gpg", "kdbx"}
	serverSecretDirPrefixes   = []string{".ssh/", ".gnupg/", ".aws/", ".gcloud/"}
	serverEnvTemplateSuffixes = []string{"example", "sample", "template", "dist", "defaults"}
)

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameSet(t *testing.T, name string, got, want []string) {
	t.Helper()
	g := append([]string{}, got...)
	w := append([]string{}, want...)
	sort.Strings(g)
	sort.Strings(w)
	if strings.Join(g, ",") != strings.Join(w, ",") {
		t.Errorf("%s drifted from server:\n cli    %v\n server %v", name, g, w)
	}
}

func TestSecretFileListsMatchServer(t *testing.T) {
	sameSet(t, "file names", sortedKeys(excludedSecretFileNames),
		append(append([]string{}, serverSecretFileNames...), cliOnlySecretFileNames...))
	sameSet(t, "extensions", sortedKeys(excludedSecretExtensions), serverSecretExtensions)
	sameSet(t, "dir prefixes", excludedSecretDirPrefixes, serverSecretDirPrefixes)
	sameSet(t, "env template suffixes", envTemplateSuffixes, serverEnvTemplateSuffixes)
}

func TestIsSecretFile(t *testing.T) {
	secret := []string{
		".npmrc", "web/.npmrc", ".mcp.json", ".claude/settings.local.json",
		"certs/server.PEM", "tls.key", ".ssh/config", "home/.ssh/known_hosts",
		".aws/config", ".gnupg/x", "id_ed25519", "credentials", ".git-credentials",
		"id_rsa.bak", "keys/id_ed25519.old", "ID_DSA_backup", "id_ecdsa-2024",
		".env", ".env.local", ".env.production.local", "local.env", "secrets.yaml",
	}
	kept := []string{
		"main.go", ".env.example", ".env.sample.local", "example.env", ".claude/settings.json",
		"key.go", "keys/readme.md", "id_rsa.pub", "id_ed25519.old.pub", "my_id_rsa.go", "src/sshd/main.go", ".pem",
	}
	for _, p := range secret {
		if !isSecretFile(p) {
			t.Errorf("isSecretFile(%q) = false, want true", p)
		}
	}
	for _, p := range kept {
		if isSecretFile(p) {
			t.Errorf("isSecretFile(%q) = true, want false", p)
		}
	}
}

// Candidate-created secrets never reach the index or the bundle; secrets the
// problem repo already tracks keep flowing (the server's tests may need them).
func TestSecretFilesNeverStagedOrBundled(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write(".gitignore", "ignored/\n")
	write("app.go", "before\n")
	write("testdata/fixture.pem", "upstream cert\n") // tracked by the problem repo
	git("add", ".")
	git("-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "base")
	base := git("rev-parse", "HEAD")

	write("app.go", "after\n")
	write("testdata/fixture.pem", "upstream cert, edited\n")
	write(".env.example", "KEY=\n")
	for _, p := range []string{".npmrc", "web/.mcp.json", ".claude/settings.local.json",
		"certs/dev.key", ".ssh/id_ed25519", ".env", "ignored/.npmrc"} {
		write(p, "secret\n")
	}
	write("staged.pem", "candidate staged this themselves\n")
	git("add", "staged.pem")

	git(append([]string{"add", "-A"}, gitAddExcludePathspecs(root)...)...)
	// staged.pem was staged by the candidate before we ran, so only the bundle filter can drop it.
	want := ".env.example\n.gitignore\napp.go\nstaged.pem\ntestdata/fixture.pem"
	if got := git("ls-files"); got != want {
		t.Fatalf("index = %q, want %q", got, want)
	}

	// A secret the candidate committed is just as out of reach of git add.
	write("committed.key", "candidate committed this\n")
	git("add", "committed.key")
	git("-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "candidate")

	// The uploaded diff drops added secrets but keeps the edited fixture.
	diffFiles := git(append([]string{"diff", "--name-only", base}, gitDiffPathspecs(root, base, base)...)...)
	if diffFiles != ".env.example\napp.go\ntestdata/fixture.pem" {
		t.Fatalf("diff files = %q", diffFiles)
	}

	res, err := bundleWorkspace(root, base)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(res.Path)
	if got := strings.Join(res.FileTree, "\n"); got != ".env.example\n.gitignore\napp.go\ntestdata/fixture.pem" {
		t.Fatalf("bundle = %q", got)
	}

	// Git-watcher shape: tree-to-tree diff where a committed secret is then
	// deleted — the deletion hunk would carry its body.
	git("rm", "-q", "committed.key")
	git("-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "delete")
	if got := git(append([]string{"diff", "--name-only", "HEAD~1", "HEAD"}, gitDiffPathspecs(root, base, "HEAD~1", "HEAD")...)...); got != "" {
		t.Fatalf("watcher diff leaked %q", got)
	}
}
