package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	flag "github.com/spf13/pflag"
)

func cmdRedeem(args []string) {
	fs := flag.NewFlagSet("redeem", flag.ContinueOnError)
	acceptTos := fs.Bool("accept-tos", false, "Skip ToS prompt (for scripted/CI use)")
	fs.Parse(args) //nolint:errcheck

	remaining := fs.Args()
	if len(remaining) == 0 || remaining[0] == "" {
		fmt.Fprintln(os.Stderr, "error: key is required\n  Usage: promptster redeem PST-XXXX-XXXX")
		os.Exit(1)
	}
	key := strings.TrimSpace(remaining[0])

	// Fetch consent state + canonical disclosure. Best-effort: on error the zero
	// value falls back to the embedded disclosure and treats consent as unconfirmed.
	info, _ := apiConsentInfo(key)

	// Show consent TUI before redeeming — accept must happen first. If consent was
	// already given via the web flow, runConsent skips the disclosure but still asks
	// the cadence question.
	result := runConsent(info.Disclosure, info.AlreadyConfirmed, *acceptTos)
	if !result.Accepted {
		fmt.Println("\nAssessment declined. No data has been recorded.")
		os.Exit(0)
	}

	// Confirm consent server-side so the redeem endpoint allows it. Skip when the
	// web flow already recorded it — the POST is idempotent, but there's no need.
	if !info.AlreadyConfirmed {
		if err := apiConfirmConsent(key); err != nil {
			fmt.Fprintf(os.Stderr, "error confirming consent: %v\n", err)
			os.Exit(1)
		}
	}

	// Generate a per-session Ed25519 keypair and register its public key with
	// the server during redeem. The private key stays on disk at
	// stateDir()/session.key (0600). Every event the CLI emits will be signed
	// with this key so `promptster verify` can prove the record is unaltered.
	pubB64, err := generateSessionKeypair()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not generate signing key: %v\n", err)
		// Non-fatal: a missing key just means this session's events are unsigned.
	}

	fmt.Printf("Redeeming %s ...\n", key)
	resp, err := apiRedeem(key, "", pubB64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Persist session so `start` can use saved session data.
	session := Session{
		SessionID:         resp.SessionID,
		SessionToken:      resp.SessionToken,
		Key:               key,
		AssessmentID:      resp.AssessmentID,
		AssessmentTitle:   resp.AssessmentTitle,
		OrgName:           resp.OrgName,
		TaskBrief:         resp.TaskBrief,
		RepoURL:           resp.RepoURL,
		RepoCommit:        resp.RepoCommit,
		RepoSubdir:        resp.RepoSubdir,
		SetupInstructions: resp.SetupInstructions,
		TimeLimitMinutes:  resp.TimeLimitMinutes,
		IssueID:           resp.IssueID,
		AllowedTools:      resp.AllowedTools,
		ConsentAccepted:   true,
		StartedAt:         time.Now().UTC(),
		ExpiresAt:         parseExpiresAt(resp.ExpiresAt),
	}
	if err := saveSession(session); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save session: %v\n", err)
	}

	// Display confirmation
	orgDisplay := resp.OrgName
	if orgDisplay == "" {
		orgDisplay = "—"
	}
	titleDisplay := resp.AssessmentTitle
	if titleDisplay == "" {
		titleDisplay = resp.TaskBrief
		if len(titleDisplay) > 60 {
			titleDisplay = titleDisplay[:57] + "..."
		}
	}
	durationDisplay := "Not specified"
	if resp.TimeLimitMinutes > 0 {
		durationDisplay = fmt.Sprintf("%d minutes", resp.TimeLimitMinutes)
	}

	fmt.Println()
	fmt.Println("\033[32m✓\033[0m Key redeemed")
	fmt.Println()
	fmt.Printf("  Organization: %s\n", orgDisplay)
	fmt.Printf("  Assessment:   %s\n", titleDisplay)
	fmt.Printf("  Duration:     %s\n", durationDisplay)
	fmt.Printf("  Capturing:    prompts, tool calls, file diffs\n")
	fmt.Println()
	fmt.Println("  Tip: Use \033[1mpromptster explain\033[0m during your session to document key decisions.")
	fmt.Println()
	fmt.Println("  Run \033[1mpromptster start\033[0m when ready.")
	fmt.Println()
}
