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
// it forever with no output. Proves the bound exists and is honoured.
func TestInstalledExtensionVersionIsBounded(t *testing.T) {
	if listExtensionsTimeout <= 0 || listExtensionsTimeout > installExtensionTimeout {
		t.Fatalf("listExtensionsTimeout %s must be positive and no larger than installExtensionTimeout %s",
			listExtensionsTimeout, installExtensionTimeout)
	}

	// A fake editor CLI that never returns, reached the same way the real one
	// is: a `cursor` on PATH inside a Cursor.app bundle.
	root := t.TempDir()
	bin := filepath.Join(root, "Applications", "Cursor.app", "Contents", "Resources", "app", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	hang := filepath.Join(bin, "cursor")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
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

	// Shrink the wait: the point is that the deadline fires and the call
	// returns, not how long the production constant is.
	done := make(chan error, 1)
	go func() {
		_, err := installedExtensionVersion(cursor)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hanging editor CLI must surface an error, not a version")
		}
	case <-time.After(listExtensionsTimeout + 20*time.Second):
		t.Fatal("installedExtensionVersion did not return: the call is still unbounded")
	}
}
