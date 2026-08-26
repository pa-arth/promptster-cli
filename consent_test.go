package main

import (
	"strings"
	"testing"
)

// The consent screen's job is to DISPLAY what the server will attest to. These
// tests exist because it silently stopped doing that: `dataUse` had no field on
// the Go struct, so the CLI decoded the disclosure, dropped the training-use
// clause, and let the server stamp a corpus-eligible version and a sha256 onto
// the candidate's record anyway.

func TestFallbackDisclosureIsCurrent(t *testing.T) {
	d := fallbackDisclosure

	if d.Version != "v4" {
		t.Errorf("fallback version = %q, want v4 — a stale fallback renders one document while the server stamps another", d.Version)
	}

	captures := strings.Join(d.Captures, "\n")
	// Cursor was retired as an instrumented agent; naming it as one told
	// candidates we capture a rail we do not run.
	aiTools := ""
	for _, c := range d.Captures {
		if strings.Contains(c, "Prompts you send") {
			aiTools = c
		}
	}
	if aiTools == "" {
		t.Fatal("fallback disclosure does not say prompts are captured")
	}
	if !strings.Contains(aiTools, "Codex") {
		t.Errorf("AI-tools line omits Codex, the rail this CLI instruments: %q", aiTools)
	}
	if strings.Contains(aiTools, "Cursor") {
		t.Errorf("AI-tools line still names Cursor, a retired agent rail: %q", aiTools)
	}

	// v3 added workspace AI-tool configuration; the fallback never caught up.
	if !strings.Contains(captures, ".claude/") || !strings.Contains(captures, "CLAUDE.md") {
		t.Error("fallback is missing the v3 AI-tool configuration capture line")
	}
	// v4 added the editor-extension install.
	if !strings.Contains(captures, "extension installed into") {
		t.Error("fallback does not disclose that an editor extension is installed")
	}

	// v3 reworded the secrets line to name the mechanism instead of asserting
	// an outcome that was not true before backend #783.
	if !strings.Contains(strings.Join(d.DoesNotCapture, "\n"), "excluded by name") {
		t.Error("fallback carries the pre-#783 secrets wording")
	}

	if len(d.DataUse) == 0 {
		t.Fatal("fallback has no data-use clause — the one omission that made every prior consent a false proof")
	}
	dataUse := strings.Join(d.DataUse, "\n")
	for _, want := range []string{"de-identified", "train", "decline"} {
		if !strings.Contains(strings.ToLower(dataUse), want) {
			t.Errorf("data-use clause missing %q: %q", want, dataUse)
		}
	}
}

// TestConsentResultCarriesHashOnlyWhenServerTextWasShown is the property that
// keeps the attestation honest. Attesting a hash for text we did not render
// would manufacture exactly the false proof the hash exists to prevent.
func TestConsentResultCarriesHashOnlyWhenServerTextWasShown(t *testing.T) {
	server := ConsentDisclosure{
		Version:  "v4",
		Captures: []string{"something the server said"},
		DataUse:  []string{"training clause"},
		TosURL:   fallbackTosURL,
	}

	t.Run("server disclosure attests its hash", func(t *testing.T) {
		got := runConsent(server, "abc123", false, true) // --accept-tos: no stdin needed
		if !got.Accepted {
			t.Fatal("--accept-tos should accept")
		}
		if got.DisclosureHash != "abc123" {
			t.Errorf("DisclosureHash = %q, want abc123", got.DisclosureHash)
		}
	})

	t.Run("fallback attests nothing", func(t *testing.T) {
		// An empty Captures list is what triggers the fallback substitution.
		got := runConsent(ConsentDisclosure{}, "abc123", false, true)
		if !got.Accepted {
			t.Fatal("--accept-tos should accept")
		}
		if got.DisclosureHash != "" {
			t.Errorf("DisclosureHash = %q, want empty: the fallback text was rendered, not the server's", got.DisclosureHash)
		}
	})
}
