package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// snapshotIntervalSeconds is the minimum time between periodic workspace
// snapshots. A hook firing more often than this will no-op. Kept low so
// that server-side auto-submit on time-limit expiry almost always has a
// recent candidate snapshot to test against.
const snapshotIntervalSeconds = 30

// snapshotMarkerPath returns the file used to throttle periodic snapshots.
func snapshotMarkerPath() string {
	return filepath.Join(stateDir(), "last-snapshot")
}

// shouldSnapshotNow returns true if the throttle window has elapsed.
func shouldSnapshotNow() bool {
	info, err := os.Stat(snapshotMarkerPath())
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) >= snapshotIntervalSeconds*time.Second
}

// markSnapshotAttempted updates the throttle marker whether the upload
// succeeded or not — we don't want to retry-spam a broken endpoint.
func markSnapshotAttempted() {
	p := snapshotMarkerPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

// maybeSnapshotWorkspace pushes a tarball of the candidate's workspace to the
// server so that server-side auto-submit (on time-limit expiry) has something
// to test against. Silent and non-blocking — hooks must never disrupt the IDE.
//
// Unlike submitWorkspaceCode (called by `promptster done`), this version:
//   - Does NOT create a git commit
//   - Prints no user-visible output on success or failure
//   - Throttles itself so repeated hook fires don't hammer the API
func maybeSnapshotWorkspace(session Session) {
	if !shouldSnapshotNow() {
		return
	}
	snapshotWorkspace(session)
}

// forceSnapshotWorkspace pushes a snapshot regardless of the throttle. Used
// after file edits so the server has the latest code within seconds of a
// change, which matters when the time limit hits mid-edit.
func forceSnapshotWorkspace(session Session) {
	snapshotWorkspace(session)
}

func snapshotWorkspace(session Session) {
	if session.SessionID == "" || session.SessionToken == "" || session.TaskRoot == "" {
		return
	}
	markSnapshotAttempted()

	if !isGitRepository(session.TaskRoot) {
		return
	}

	taskRoot := session.TaskRoot
	baseSha := diffBaseFor(session)

	// Stage with -N (intent-to-add) so untracked files appear in `git diff`
	// without actually committing or polluting the index. Pathspec excludes
	// keep CLI-generated files (TASK.md, .promptster/) out of the snapshot.
	addArgs := append([]string{"add", "-N"}, gitAddExcludePathspecs(taskRoot)...)
	if _, err := runCommand(taskRoot, "git", addArgs...); err != nil {
		hookDebugf("snapshot: git add -N failed: %v", err)
		return
	}

	var diff string
	if baseSha != "" {
		args := append([]string{"diff", baseSha}, gitExcludePathspecs(taskRoot)...)
		if out, err := runCommand(taskRoot, "git", args...); err == nil {
			diff = string(out)
		}
	}
	if diff == "" {
		args := append([]string{"diff", "HEAD"}, gitExcludePathspecs(taskRoot)...)
		if out, err := runCommand(taskRoot, "git", args...); err == nil {
			diff = string(out)
		}
	}

	bundle, err := bundleWorkspace(taskRoot, baseSha)
	if err != nil {
		hookDebugf("snapshot: bundle failed: %v", err)
		return
	}
	defer os.Remove(bundle.Path)

	urlResp, err := apiGetUploadURL(session.SessionToken, session.SessionID)
	if err != nil {
		hookDebugf("snapshot: get upload URL failed: %v", err)
		return
	}
	if err := uploadBundle(urlResp.UploadURL, bundle.Path, bundle.Size); err != nil {
		hookDebugf("snapshot: upload failed: %v", err)
		return
	}

	payload := submitCodePayload{
		SessionID: session.SessionID,
		// Same scrub as the `promptster done` path — snapshot diffs feed the
		// identical submit-code endpoint and fire far more often.
		Diff:         string(redactBytes([]byte(diff))),
		FileTree:     bundle.FileTree,
		RepoURL:      session.RepoURL,
		BaseSha:      baseSha,
		BundleKey:    urlResp.ObjectKey,
		BundleSize:   bundle.Size,
		BundleSHA256: bundle.SHA256Hex,
	}

	if err := apiSubmitCode(session.SessionToken, payload); err != nil {
		hookDebugf("snapshot: submit-code failed: %v", err)
		return
	}
	hookDebugAppend(fmt.Sprintf("%s snapshot uploaded files=%d size=%d", time.Now().UTC().Format(time.RFC3339Nano), bundle.FileCount, bundle.Size))
}
