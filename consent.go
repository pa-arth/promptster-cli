package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// fallbackTosURL and fallbackDisclosure mirror the API's canonical consent
// disclosure (apps/api/src/lib/consent-disclosure.ts). They are used ONLY when
// the GET /v1/candidate/consent fetch fails, so the consent screen still works
// if the API is briefly unreachable. Keep them in sync with the API constant.
//
// "Keep in sync" is not a nicety here. The server stamps its CURRENT version
// and hash onto the candidate's record regardless of which text this binary
// rendered, so a stale fallback means holding a sha256 proof of assent to a
// document nobody displayed — worse than holding no proof at all. Two things
// keep that honest: this copy is refreshed with every version bump, and a
// consent taken from this fallback attests NO hash (see runConsent), so the
// server records it the way it records any older client rather than treating
// our text as its own.
const fallbackTosURL = "https://promptster.ai/legal/terms"

var fallbackDisclosure = ConsentDisclosure{
	Version: "v4",
	Captures: []string{
		"Prompts you send to AI coding tools (Claude Code, Codex)",
		"Which files you open in your editor and for how long — via a Promptster extension installed into VS Code or Cursor with your permission, which reads no file contents and can be paused at any time from the editor command palette",
		"How you have set up your AI tools in this workspace — CLAUDE.md, and any skills, agents, commands, or settings under .claude/",
		"AI tool calls and responses",
		"Code changes and file edits in the assessment workspace (unified diffs)",
		"Terminal commands and exit codes run in the workspace",
		"Which files you read or search",
		"How AI suggestions were accepted, revised, or rejected",
		"Timing between prompts and commands (only if you enable cadence checks)",
		"Anonymized device info for session continuity",
	},
	DoesNotCapture: []string{
		"Keystrokes, clipboard contents, or screen recordings",
		"File contents you never open or edit",
		"Environment variables and secrets — .env files, .npmrc, SSH and cloud credentials, and private keys are excluded by name before anything is stored",
		"Browser history, personal tabs, webcam, or microphone",
		"Anything outside the assessment workspace",
		"Anything before you start or after you end the session",
	},
	Evaluated: []string{
		"Whether your solution passed the assessment tests",
		"How you decomposed the problem using AI tools",
		"Your exploration and verification habits",
		"How you set up your tools for the work — and setting nothing up is a legitimate answer, not a deduction. We are looking at whether your setup fits the task, not at how much of it there is.",
		"Key moments in your process (not a score)",
		"How you explain your key decisions",
	},
	DataUse: []string{
		"In de-identified form — with your name, email, and account identifiers removed — your assessment may be used to improve and train Promptster’s evaluation models.",
		"This never affects your assessment result, and you can decline this specific use at any time by contacting privacy@promptster.ai.",
	},
	TosURL: fallbackTosURL,
}

// ConsentResult captures both the ToS decision and optional integrity opt-in.
type ConsentResult struct {
	Accepted          bool
	Note              string
	IntegrityAccepted bool
	// sha256 of the disclosure that was ACTUALLY RENDERED, or empty. Empty is a
	// meaningful value, not a missing one: it says we showed the embedded
	// fallback, so we are not in a position to attest anything about the
	// server's current text. apiConfirmConsent sends the field only when set.
	DisclosureHash string
}

// runConsent handles the Terms of Service check and returns the result.
//
// disclosure is the canonical capture text (server-provided; pass a zero value
// to fall back to the embedded list). The three branches:
//  1. acceptTosFlag (--accept-tos): auto-accept, but STILL print the disclosure so
//     even scripted/CI runs leave a displayed record. Integrity defaults on.
//  2. alreadyConfirmed (consent already given via the web flow): skip re-showing
//     the full disclosure, but STILL ask the one cadence question so the opt-in
//     stays explicit. Non-interactive → integrity defaults off.
//  3. Otherwise: full interactive flow — disclosure + accept prompt + cadence prompt.
func runConsent(disclosure ConsentDisclosure, disclosureHash string, alreadyConfirmed, acceptTosFlag bool) ConsentResult {
	// Falling back drops the attestation with the text. Attesting a hash we did
	// not render would produce exactly the false proof the hash exists to
	// prevent — and the server already handles an absent attestation as an
	// older client, which is a fair description of what we are in that moment.
	if len(disclosure.Captures) == 0 {
		disclosure = fallbackDisclosure
		disclosureHash = ""
	}

	// [1] --accept-tos: print disclosure (record), no prompt.
	if acceptTosFlag {
		printDisclosure(disclosure)
		dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		fmt.Println(dim.Render("  Terms accepted via --accept-tos."))
		fmt.Println()
		return ConsentResult{Accepted: true, Note: "(--accept-tos)", IntegrityAccepted: true, DisclosureHash: disclosureHash}
	}

	scanner := bufio.NewScanner(os.Stdin)

	// [2] Already accepted on the web — don't re-show the disclosure, but still
	// ask the cadence question.
	if alreadyConfirmed {
		heading := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
		check := lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("✓")
		fmt.Println()
		fmt.Printf("  %s  %s\n", check, heading.Render("Terms already accepted on the web"))
		fmt.Println()
		integrity := askCadenceOptIn(scanner)
		return ConsentResult{Accepted: true, Note: "accepted (web)", IntegrityAccepted: integrity, DisclosureHash: disclosureHash}
	}

	// [3] Full interactive flow.
	printDisclosure(disclosure)
	fmt.Print("  Accept and continue? [yes/no]: ")
	if !scanner.Scan() {
		return ConsentResult{}
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	if answer != "yes" && answer != "y" {
		return ConsentResult{}
	}

	integrity := askCadenceOptIn(scanner)
	return ConsentResult{Accepted: true, Note: "accepted", IntegrityAccepted: integrity, DisclosureHash: disclosureHash}
}

// printDisclosure renders the canonical capture/non-capture/evaluated lists with
// the CLI's terminal styling. The API supplies plain strings; the styling is
// applied here.
func printDisclosure(d ConsentDisclosure) {
	heading := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	check := lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("✓")
	cross := lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("✗")
	bullet := lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render("•")
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	link := lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Underline(true)

	tos := d.TosURL
	if tos == "" {
		tos = fallbackTosURL
	}

	fmt.Println()
	fmt.Printf("  %s  %s\n", heading.Render("Terms of Service"), link.Render(tos))
	fmt.Println()

	fmt.Println(heading.Render("  CAPTURED during your session:"))
	fmt.Println()
	for _, item := range d.Captures {
		fmt.Printf("    %s  %s\n", check, item)
	}

	fmt.Println()
	fmt.Println(heading.Render("  NOT CAPTURED:"))
	fmt.Println()
	for _, item := range d.DoesNotCapture {
		fmt.Printf("    %s  %s\n", cross, item)
	}

	if len(d.Evaluated) > 0 {
		fmt.Println()
		fmt.Println(heading.Render("  WHAT'S EVALUATED:"))
		fmt.Println()
		for _, item := range d.Evaluated {
			fmt.Printf("    %s  %s\n", bullet, item)
		}
	}

	// The clause that decides whether this session can ever be reused for
	// training. It has been in the canonical disclosure since v2 and was
	// rendered by nothing: the Go struct had no field for it, so this function
	// could not have printed it if it wanted to. Every consent this CLI took
	// before v4 is a proof of assent to text the candidate never saw.
	//
	// Placed before the accept prompt, deliberately. A data-use clause shown
	// after the decision is not a disclosure, it is a receipt.
	if len(d.DataUse) > 0 {
		fmt.Println()
		fmt.Println(heading.Render("  HOW YOUR DATA MAY BE USED:"))
		fmt.Println()
		for _, item := range d.DataUse {
			fmt.Printf("    %s  %s\n", bullet, item)
		}
	}

	fmt.Println()
	fmt.Println("  You'll receive a link to view your own session replay after submission.")
	fmt.Println()
	fmt.Println(dim.Render("  Your session data is retained per the hiring team's policy — 30 days by"))
	fmt.Println(dim.Render("  default, never longer than their plan allows. Deletion: privacy@promptster.ai"))
	fmt.Println()
}

// askCadenceOptIn prompts for the optional cadence-based authorship check and
// returns the candidate's choice. Non-interactive (no stdin) → false.
func askCadenceOptIn(scanner *bufio.Scanner) bool {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	fmt.Println("  Optional: allow cadence-based authorship checks?")
	fmt.Println(dim.Render("  Records timing between your prompts and commands so reviewers can"))
	fmt.Println(dim.Render("  distinguish typed work from pasted work. No keystrokes are captured."))
	fmt.Print("  Enable? [yes/no]: ")

	if scanner.Scan() {
		ans := strings.TrimSpace(strings.ToLower(scanner.Text()))
		return ans == "yes" || ans == "y"
	}
	return false
}
