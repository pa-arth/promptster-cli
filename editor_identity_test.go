package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// The bug: `detectEditors` matched on command name alone. Cursor's own
// "Install 'code' command in PATH" writes /usr/local/bin/code pointing into
// Cursor.app, so on a machine with only Cursor we reported BOTH editors,
// installed the same .vsix into Cursor twice, told the candidate we had
// installed into "VS Code and Cursor", and recorded an editor that does not
// exist on the machine. Editor attribution was wrong on every machine where
// `code` is a Cursor shim.

func TestAppBundleFindsTheOwningApp(t *testing.T) {
	cases := []struct{ in, want string }{
		// The real shim target. Note the plain "app" directory on the way down:
		// it must not be mistaken for a bundle.
		{"/Applications/Cursor.app/Contents/Resources/app/bin/code", "/Applications/Cursor.app"},
		{"/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code", "/Applications/Visual Studio Code.app"},
		{"/Applications/Cursor.app", "/Applications/Cursor.app"},
		// Not in a bundle at all — a Linux install, or a user-built binary.
		{"/usr/local/bin/code", ""},
		{"/home/candidate/.local/bin/cursor", ""},
		{"code", ""},
	}
	for _, c := range cases {
		if got := appBundle(c.in); got != c.want {
			t.Errorf("appBundle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEditorIdentityDistinguishesTheApps(t *testing.T) {
	cursorShim := "/Applications/Cursor.app/Contents/Resources/app/bin/code"
	if got := editorIdentity(cursorShim); got != "Cursor.app" {
		t.Fatalf("a `code` shim inside Cursor.app must identify as Cursor.app, got %q", got)
	}
	var vscode, cursor supportedEditor
	for _, e := range supportedEditors() {
		switch e.key {
		case "vscode":
			vscode = e
		case "cursor":
			cursor = e
		}
	}
	if editorIdentity(vscode.bundlePath) == editorIdentity(cursor.bundlePath) {
		t.Fatal("the two supported editors must not share a bundle identity")
	}
	// This is the comparison detectEditors makes, and the one that was missing.
	if editorIdentity(cursorShim) == editorIdentity(vscode.bundlePath) {
		t.Fatal("a Cursor-owned `code` shim must not satisfy the VS Code identity")
	}
}

// End to end through detectEditors: a machine with ONLY Cursor, whose `code`
// command is a symlink into Cursor.app, must report exactly one editor.
func TestDetectEditorsRejectsACursorOwnedCodeShim(t *testing.T) {
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/Applications/Visual Studio Code.app"); err == nil {
			t.Skip("real VS Code is installed; the bundlePath fallback would legitimately find it")
		}
	}
	root := t.TempDir()
	// A fake Cursor.app carrying both of its shipped commands.
	bin := filepath.Join(root, "Applications", "Cursor.app", "Contents", "Resources", "app", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cursor", "code"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// PATH holds symlinks to them, the way Cursor's installer writes them.
	pathDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cursor", "code"} {
		if err := os.Symlink(filepath.Join(bin, name), filepath.Join(pathDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", pathDir)

	found := detectEditors()
	if len(found) != 1 {
		keys := make([]string, 0, len(found))
		for _, e := range found {
			keys = append(keys, e.key)
		}
		t.Fatalf("a Cursor-only machine must report one editor, got %d: %v", len(found), keys)
	}
	if found[0].key != "cursor" {
		t.Fatalf("the one editor must be cursor, got %q", found[0].key)
	}
}

// doctor is the first thing a confused candidate runs. It used to shell out to
// the editor CLI with no deadline at all, so an editor that blocked would hang
// it forever with no output.
//
// The fake CLI backgrounds a child that INHERITS the stdout pipe and outlives
// its parent. That is the case CommandContext alone does not cover: it kills
// the editor process, but Output goes on waiting for the pipe the survivor is
// still holding. Without WaitDelay this test hangs.
func TestInstalledExtensionVersionIsBounded(t *testing.T) {
	if listExtensionsTimeout <= 0 || listExtensionsTimeout > installExtensionTimeout {
		t.Fatalf("listExtensionsTimeout %s must be positive and no larger than installExtensionTimeout %s",
			listExtensionsTimeout, installExtensionTimeout)
	}
	// Inject a short deadline: the point is that it FIRES, not how long the
	// production value is. Restored by t.Cleanup via the saved value.
	saved := listExtensionsTimeout
	t.Cleanup(func() { listExtensionsTimeout = saved })
	listExtensionsTimeout = 1 * time.Second

	root := t.TempDir()
	bin := filepath.Join(root, "Applications", "Cursor.app", "Contents", "Resources", "app", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	hang := filepath.Join(bin, "cursor")
	// The shell EXITS IMMEDIATELY, leaving a backgrounded child holding the
	// inherited stdout pipe. This is the shape CommandContext cannot cover: the
	// editor process is already gone, so there is nothing for the deadline to
	// kill, and Output goes on waiting for the pipe the survivor still holds.
	// Measured without WaitDelay: returns only when the child finishes, with a
	// nil error. Only WaitDelay bounds it.
	// /bin/sleep by absolute path: this test narrows PATH to its own temp dir,
	// so a bare `sleep` would not be found and the fake CLI would exit at once
	// — passing for the wrong reason.
	if err := os.WriteFile(hang, []byte("#!/bin/sh\n/bin/sleep 120 &\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hang, filepath.Join(pathDir, "cursor")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	var cursor supportedEditor
	for _, e := range supportedEditors() {
		if e.key == "cursor" {
			cursor = e
		}
	}

	done := make(chan error, 1)
	t0 := time.Now()
	go func() {
		_, err := installedExtensionVersion(cursor)
		done <- err
	}()
	select {
	case err := <-done:
		// Without WaitDelay this call returns only when the survivor finishes,
		// and returns a NIL error while doing it — measured at 30.2s against a
		// 30s sleep, versus 2.2s with it. So the error is the signal that the
		// bound fired; a nil error here means nothing bounded anything.
		if err == nil {
			t.Fatal("bounded call returned no error: the pipe survivor was not cut off")
		}
		if elapsed := time.Since(t0); elapsed > 20*time.Second {
			t.Fatalf("returned only after %s: the bound is not being honoured", elapsed.Round(time.Millisecond))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("installedExtensionVersion did not return: a survivor holding the pipe still hangs it")
	}
}

// Greptile caught this as a regression in the first cut of the identity fix:
// rejecting a mismatched PATH shim without falling back to the known bundle
// DROPPED a genuinely installed editor. Both CAN be installed at once with
// `code` pointing into Cursor.
//
// Uses a synthetic editor whose bundle lives in a temp dir, so it verifies the
// fallback on any machine rather than skipping wherever VS Code is absent — a
// skipped test for a regression is no test at all.
func TestBundleFallbackWhenPathCommandBelongsToAnotherEditor(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the bundle fallback is macOS-only by construction")
	}
	root := t.TempDir()

	// A real VS Code install, in the temp tree.
	vscodeBin := filepath.Join(root, "Applications", "Visual Studio Code.app", "Contents", "Resources", "app", "bin")
	if err := os.MkdirAll(vscodeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	vscodeCLI := filepath.Join(vscodeBin, "code")
	if err := os.WriteFile(vscodeCLI, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Cursor is also installed, and owns `code` on PATH.
	cursorBin := filepath.Join(root, "Applications", "Cursor.app", "Contents", "Resources", "app", "bin")
	if err := os.MkdirAll(cursorBin, 0o755); err != nil {
		t.Fatal(err)
	}
	cursorShim := filepath.Join(cursorBin, "code")
	if err := os.WriteFile(cursorShim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cursorShim, filepath.Join(pathDir, "code")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	vscode := supportedEditor{key: "vscode", name: "VS Code", command: "code", bundlePath: vscodeCLI}

	// The PATH entry is Cursor's, so it must be rejected — but VS Code is
	// genuinely installed, so we must fall back to its bundle, not drop it.
	if got := resolveEditorCLI(vscode); got != vscodeCLI {
		t.Fatalf("VS Code must resolve to its own bundle %q, got %q", vscodeCLI, got)
	}
	// And the Cursor-owned shim must not be mistaken for it.
	if ownedBy(cursorShim, vscode) {
		t.Fatal("a Cursor-owned `code` must not be reported as owned by VS Code")
	}
}
