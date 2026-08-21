package vsix

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

// The pinned checksum is the only thing that makes "which extension is on the
// candidate's machine" answerable. A constant that drifts from the bytes beside
// it answers it wrongly, which is worse than not recording it.
func TestPinnedSHA256MatchesEmbeddedBytes(t *testing.T) {
	if got := ActualSHA256(); got != SHA256 {
		t.Fatalf("pinned SHA256 does not match the embedded artifact:\n  pinned %s\n  actual %s\n\n"+
			"If you replaced the .vsix, update SHA256 (and Version, SourceTag, and the go:embed filename).",
			SHA256, got)
	}
}

func TestArtifactIsNotEmpty(t *testing.T) {
	if len(artifact) < 10_000 {
		t.Fatalf("embedded artifact is %d bytes — that is not a packaged extension", len(artifact))
	}
}

// A .vsix is a zip with an extension manifest. Check the embedded bytes really
// are one, and that the version inside agrees with the version we advertise —
// the exact skew this package exists to stop (the repo previously shipped a
// 0.1.0 artifact beside a 0.2.0 source).
func TestArtifactManifestVersionMatches(t *testing.T) {
	zr, err := zip.NewReader(bytes.NewReader(artifact), int64(len(artifact)))
	if err != nil {
		t.Fatalf("embedded artifact is not a zip: %v", err)
	}

	var manifest []byte
	for _, f := range zr.File {
		if f.Name == "extension.vsixmanifest" {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open manifest: %v", err)
			}
			buf := new(bytes.Buffer)
			if _, err := buf.ReadFrom(rc); err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			rc.Close()
			manifest = buf.Bytes()
			break
		}
	}
	if manifest == nil {
		t.Fatal("embedded artifact has no extension.vsixmanifest — not a .vsix")
	}

	var parsed struct {
		Metadata struct {
			Identity struct {
				ID      string `xml:"Id,attr"`
				Version string `xml:"Version,attr"`
			} `xml:"Identity"`
		} `xml:"Metadata"`
	}
	if err := xml.Unmarshal(manifest, &parsed); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	if parsed.Metadata.Identity.Version != Version {
		t.Errorf("artifact manifest says version %q but this package advertises %q",
			parsed.Metadata.Identity.Version, Version)
	}
	if !strings.EqualFold(parsed.Metadata.Identity.ID, "promptster") {
		t.Errorf("artifact identity is %q, expected promptster", parsed.Metadata.Identity.ID)
	}
	if !strings.Contains(Filename(), Version) {
		t.Errorf("Filename() %q does not carry the version", Filename())
	}
}
