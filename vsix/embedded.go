// Package vsix carries the promptster-vscode extension artifact that `start`
// installs into the candidate's editor.
//
// The artifact is embedded rather than downloaded. An assessment must not
// depend on a release host being reachable at the moment a candidate begins,
// and a candidate's editor must not be asked to install a file whose contents
// depend on when the download happened.
//
// SHA256 is pinned here and asserted against the embedded bytes by a test, so
// the checksum is a fact about this commit rather than a claim about it. The
// artifact is built reproducibly (promptster-vscode/scripts/build-vsix.sh), so
// anyone can check out the extension tag below, rebuild, and get these exact
// bytes.
package vsix

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

//go:embed promptster-0.3.0.vsix
var artifact []byte

const (
	// Version of the embedded extension. Must match the filename above and the
	// version in the extension's package.json at the tag.
	Version = "0.3.0"

	// Tag in pa-arth/promptster-vscode this artifact was built from.
	SourceTag = "v0.3.0"

	// SHA256 is the pinned checksum of the embedded artifact, hex-encoded.
	// Recorded on the session when the extension is installed, so a reviewer
	// can tell which build produced a candidate's attention events.
	SHA256 = "c1a95f09c5d5472c09f12e269740b6b0f528baec08e882c92a69c6906ac201cc"
)

// Bytes returns the embedded .vsix.
func Bytes() []byte {
	return artifact
}

// Filename is the name the artifact is written under when installing.
func Filename() string {
	return "promptster-" + Version + ".vsix"
}

// ActualSHA256 computes the checksum of the embedded bytes. Used by the test
// that keeps SHA256 honest; also used at install time so the value recorded on
// the session is measured, not copied from a constant.
func ActualSHA256() string {
	sum := sha256.Sum256(artifact)
	return hex.EncodeToString(sum[:])
}
