package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// cmdVerify implements `promptster verify [sessionId|PST-XXXX]`.
// It fetches the signed event log for a session, walks the Ed25519 chain
// offline, and reports whether every event verifies and whether prev_sig
// linkage is intact end-to-end.
func cmdVerify(args []string) {
	var target string
	if len(args) > 0 {
		target = strings.TrimSpace(args[0])
	} else {
		sess, err := loadSession()
		if err != nil {
			fmt.Fprintln(os.Stderr, "usage: promptster verify <sessionId|PST-XXXX-XXXX>")
			os.Exit(1)
		}
		target = sess.SessionID
	}

	sessionID, apiKey, err := resolveSessionAndKey(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	log, err := fetchSignedLog(sessionID, apiKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	dim := lipgloss.NewStyle().Foreground(cDim)
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Bold(true)
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444")).Bold(true)

	fmt.Println()
	fmt.Printf("  %s %s\n", dim.Render("session"), sessionID)

	if log.SigningPubkey == "" {
		fmt.Printf("  %s  This session was started with an older CLI and has no per-session signing key.\n",
			dim.Render("unsigned"))
		fmt.Printf("  %s  %d events recorded; signatures cannot be verified.\n",
			dim.Render("·"), len(log.Events))
		fmt.Println()
		return
	}

	pubRaw, err := base64.StdEncoding.DecodeString(log.SigningPubkey)
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		fmt.Fprintf(os.Stderr, "error: invalid signing pubkey on server\n")
		os.Exit(1)
	}
	pub := ed25519.PublicKey(pubRaw)

	total := len(log.Events)
	verified := 0
	chainIntact := true
	var firstBreakAt int = -1
	var expectedPrev string

	for i, ev := range log.Events {
		if ev.Sig == "" {
			chainIntact = false
			if firstBreakAt < 0 {
				firstBreakAt = i
			}
			continue
		}
		if ev.PrevSig != expectedPrev {
			chainIntact = false
			if firstBreakAt < 0 {
				firstBreakAt = i
			}
		}
		ok := verifyChainedEvent(pub, ev)
		if ok {
			verified++
		} else {
			chainIntact = false
			if firstBreakAt < 0 {
				firstBreakAt = i
			}
		}
		expectedPrev = ev.Sig
	}

	if verified == total && chainIntact {
		fmt.Printf("  %s  %d / %d events signed · chain intact\n",
			green.Render("✓"), verified, total)
	} else {
		fmt.Printf("  %s  %d / %d events verified · chain broken at event %d\n",
			red.Render("✗"), verified, total, firstBreakAt)
	}
	fmt.Println()
}

// SignedEventLog is the JSON body returned by GET /v1/sessions/:id/signed-log.
type SignedEventLog struct {
	SessionID     string         `json:"sessionId"`
	SigningPubkey string         `json:"signingPubkey"`
	Events        []SignedLogRow `json:"events"`
}

// SignedLogRow is a single entry. `payload` is the original event body the
// CLI sent; we re-derive the signing message from it to verify sig locally.
type SignedLogRow struct {
	ID          string                 `json:"id"`
	Ts          string                 `json:"ts"`
	Payload     map[string]interface{} `json:"payload"`
	Sig         string                 `json:"sig"`
	PrevSig     string                 `json:"prevSig"`
	EventHash   string                 `json:"eventHash"`
	SigVerified *bool                  `json:"sigVerified"`
}

func verifyChainedEvent(pub ed25519.PublicKey, row SignedLogRow) bool {
	p := row.Payload
	toStr := func(k string) string {
		if v, ok := p[k].(string); ok {
			return v
		}
		return ""
	}
	v := 1
	if n, ok := p["v"].(float64); ok {
		v = int(n)
	}
	sourceIntegration := ""
	switch s := p["source"].(type) {
	case string:
		sourceIntegration = s
	case map[string]interface{}:
		if v, ok := s["integration"].(string); ok {
			sourceIntegration = v
		} else if v, ok := s["emitter"].(string); ok {
			sourceIntegration = v
		} else if v, ok := s["channel"].(string); ok {
			sourceIntegration = v
		}
	}

	e := Event{
		ID:        toStr("id"),
		SessionID: toStr("sessionId"),
		Ts:        toStr("ts"),
		Kind:      toStr("kind"),
		Source:    sourceIntegration,
		V:         v,
		Data:      p["data"],
	}
	msg, err := buildSigningMessage(e, row.PrevSig)
	if err != nil {
		return false
	}
	sig, err := hex.DecodeString(row.Sig)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

func fetchSignedLog(sessionID, apiKey string) (*SignedEventLog, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL()+"/v1/sessions/"+sessionID+"/signed-log", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out SignedEventLog
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

// resolveSessionAndKey figures out the session ID and auth key to use.
// If `target` is a PST-XXXX-XXXX key: use it as both the auth key and
// (via the locally-saved session) learn the session id it unlocks. If
// `target` looks like a UUID, require an active local session to supply
// the auth key.
func resolveSessionAndKey(target string) (sessionID, apiKey string, err error) {
	if strings.HasPrefix(strings.ToUpper(target), "PST-") {
		sess, lerr := loadSession()
		if lerr == nil && strings.EqualFold(sess.Key, target) {
			return sess.SessionID, target, nil
		}
		return "", "", fmt.Errorf("no local session matches key %s — run verify from the workspace where the session ran, or pass the session UUID", target)
	}
	sess, lerr := loadSession()
	if lerr != nil {
		return "", "", fmt.Errorf("no active local session to provide an API key for %s", target)
	}
	return target, sess.SessionToken, nil
}
