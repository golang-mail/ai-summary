package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// summarizeChanges asks the configured AI CLI to digest the buffered batch.
// Returns (summary, fallback). fallback=true means the AI call failed or
// timed out and the caller should render a deterministic listing instead.
func (s *server) summarizeChanges(ctx context.Context, emails []bufferedEmail) (string, bool) {
	if len(emails) == 0 {
		return "", true
	}
	prompt := buildChangesPrompt(emails, s.cfg.IncludeLabels, s.cfg.Instructions)
	return s.runAI(ctx, prompt)
}

// summarizeWholeMailbox asks the AI to digest the current state of the
// configured mailbox(es). Body excerpts aren't included in scheduled-mode
// prompts to keep the context bounded (subject + from + visible labels only).
func (s *server) summarizeWholeMailbox(ctx context.Context, emails []bufferedEmail) (string, bool) {
	if len(emails) == 0 {
		return "_(mailbox empty)_", false
	}
	prompt := buildWholeMailboxPrompt(emails, s.cfg.IncludeLabels, s.cfg.Instructions)
	return s.runAI(ctx, prompt)
}

// backendCommand picks the argv for the configured backend. Empty means
// claude; an unknown backend yields nil so the caller falls back.
func (c *pluginConfig) backendCommand() []string {
	switch strings.ToLower(c.Backend) {
	case "", "claude":
		return c.ClaudeCommand
	case "gemini":
		return c.GeminiCommand
	case "opencode":
		return c.OpenCodeCommand
	}
	return nil
}

// runAI shells out to the configured AI CLI. Honors ai_timeout.
// Returns (summary, fallback). Stdout is the summary; stderr is logged.
func (s *server) runAI(ctx context.Context, prompt string) (string, bool) {
	argv := s.cfg.backendCommand()
	if len(argv) == 0 {
		s.logf("warn", "ai backend %q has no command configured, falling back", s.cfg.Backend)
		return "", true
	}
	opencode := strings.EqualFold(s.cfg.Backend, "opencode")
	if opencode {
		argv = openCodeArgv(argv)
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.aiTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Detach from any parent Claude Code session: an inherited CLAUDECODE +
	// shared CLAUDE_CODE_TMPDIR makes a nested `claude -p` hang until timeout.
	// Running in an empty dir keeps the CLI from loading project instructions.
	tmpDir, err := os.MkdirTemp("", "ai-summary-")
	if err == nil {
		defer os.RemoveAll(tmpDir)
		cmd.Env = aiCommandEnv(tmpDir)
		cmd.Dir = tmpDir
	}
	start := time.Now()
	err = cmd.Run()
	dur := time.Since(start)
	if err != nil {
		s.logf("error", "ai backend=%s failed in %s: %v (stderr: %s)", s.cfg.Backend, dur, err, strings.TrimSpace(stderr.String()))
		return "", true
	}
	out := strings.TrimSpace(stdout.String())
	if opencode {
		if text, ok := parseOpenCodeEvents(out); ok {
			out = text
		}
	}
	if out == "" {
		s.logf("warn", "ai backend=%s returned empty output in %s, falling back (stderr: %s)", s.cfg.Backend, dur, strings.TrimSpace(stderr.String()))
		return "", true
	}
	if s.cfg.Verbose {
		s.logf("info", "ai backend=%s ok in %s (%d bytes out)", s.cfg.Backend, dur, len(out))
	}
	return out, false
}

// aiCommandEnv returns the parent environment with Claude Code and opencode
// session markers stripped and a fresh CLAUDE_CODE_TMPDIR, so the spawned CLI
// runs as a top-level instance rather than a conflicting nested one.
func aiCommandEnv(tmpDir string) []string {
	parent := os.Environ()
	out := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CLAUDE_CODE_TMPDIR=") || strings.HasPrefix(kv, "OPENCODE_INTERACTIVE=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CLAUDE_CODE_TMPDIR="+tmpDir)
}

// slackFormatRules constrains the model to Slack mrkdwn (not Markdown), since
// the digest body is rendered verbatim in a section block of type mrkdwn.
const slackFormatRules = "Format using Slack mrkdwn ONLY, not Markdown: " +
	"*bold* (single asterisks, never **), _italic_, ~strike~, `code`, > quote. " +
	"Bullets start with \"• \". Links as <https://url|label>. " +
	"Never use #/##/### headings, [label](url) links, or --- rules. " +
	"Make each group a *bold* one-line title followed by its bullets, " +
	"and separate groups with one blank line so the digest is scannable. " +
	"Output only the digest body, no preamble.\n"

// buildChangesPrompt renders the per_run prompt. Inputs are the buffered
// emails from the last filter pass. Non-empty instructions replace the
// default structure guidance; mrkdwn rules always apply.
func buildChangesPrompt(emails []bufferedEmail, includeLabels bool, instructions string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summarize this batch of %d new emails as a Slack digest.\n", len(emails))
	if instructions != "" {
		b.WriteString(strings.TrimSpace(instructions))
		b.WriteString("\n")
	} else {
		b.WriteString("Group by sender or topic. Be terse. One bullet per group, max 8 bullets.\n")
		b.WriteString("Flag anything that looks time-sensitive.\n")
	}
	b.WriteString(slackFormatRules)
	b.WriteString("\n---\n")
	writeEmailList(&b, emails, includeLabels, true)
	return b.String()
}

// buildWholeMailboxPrompt renders the interval/cron prompt. Skips body
// excerpts (subject + from + labels only) to keep the AI context tight.
// Non-empty instructions replace the default structure guidance; mrkdwn
// rules always apply.
func buildWholeMailboxPrompt(emails []bufferedEmail, includeLabels bool, instructions string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summarize the current state of these %d emails as a Slack digest.\n", len(emails))
	if instructions != "" {
		b.WriteString(strings.TrimSpace(instructions))
		b.WriteString("\n")
	} else {
		b.WriteString("Group by sender or topic. Highlight unread and time-sensitive items.\n")
		b.WriteString("Max 10 bullets.\n")
	}
	b.WriteString(slackFormatRules)
	b.WriteString("\n---\n")
	writeEmailList(&b, emails, includeLabels, false)
	return b.String()
}

// writeEmailList serializes one bullet per email into the prompt. includeBody
// switches between the per_run shape (body excerpt) and the snapshot shape
// (subject + from + labels only).
func writeEmailList(b *strings.Builder, emails []bufferedEmail, includeLabels, includeBody bool) {
	for i, e := range emails {
		subject := e.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		fmt.Fprintf(b, "[%d] From: %s — %q", i+1, e.From, subject)
		if e.Unread {
			b.WriteString("  (unread)")
		}
		b.WriteString("\n")
		date := ""
		if !e.Date.IsZero() {
			date = e.Date.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(b, "    %s/%s  %s", e.Account, e.Mailbox, date)
		if includeLabels && len(e.Labels) > 0 {
			fmt.Fprintf(b, "   labels: [%s]", strings.Join(e.Labels, ", "))
		}
		b.WriteString("\n")
		if includeBody && e.Body != "" {
			// Indent the body so the AI knows where each entry's content ends.
			for _, line := range strings.Split(e.Body, "\n") {
				b.WriteString("    ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}
}

// renderFallbackListing produces a deterministic plain-text digest used when
// the AI call fails. Caller wraps it in the Slack message body.
func renderFallbackListing(emails []bufferedEmail) string {
	var b strings.Builder
	for i, e := range emails {
		subject := e.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, e.From, subject)
		if i >= 19 && len(emails) > 20 {
			fmt.Fprintf(&b, "…and %d more\n", len(emails)-20)
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
