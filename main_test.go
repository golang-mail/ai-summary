package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIsUnread(t *testing.T) {
	if isUnread([]string{"\\Seen", "Flagged"}) {
		t.Error("\\Seen flag means read")
	}
	if isUnread([]string{"\\seen"}) {
		t.Error("flag match must be case-insensitive")
	}
	if !isUnread([]string{"Flagged"}) || !isUnread(nil) {
		t.Error("absence of \\Seen means unread")
	}
}

func TestWriteEmailList_MarksUnread(t *testing.T) {
	emails := []bufferedEmail{
		{Subject: "needs attention", From: "a@x", Unread: true},
		{Subject: "already read", From: "b@x"},
	}
	p := buildWholeMailboxPrompt(emails, false, "")
	if strings.Count(p, "(unread)") != 1 {
		t.Fatalf("prompt must mark exactly the one unread email; got %d markers in:\n%s",
			strings.Count(p, "(unread)"), p)
	}
	if !strings.Contains(p, "needs attention") {
		t.Fatalf("prompt missing email listing:\n%s", p)
	}
}

func TestRefsToBuffered_CarriesUnread(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	refs := []emailRef{
		{UID: 1, Flags: []string{"\\Seen"}, Date: t0.Add(time.Hour)},
		{UID: 2, Date: t0},
	}
	out := refsToBuffered(refs, newDigestState(), 0)
	if len(out) != 2 {
		t.Fatalf("got %d emails, want 2", len(out))
	}
	// newest first: UID 1 (read), then UID 2 (unread)
	if out[0].Unread {
		t.Error("UID 1 has \\Seen, must not be unread")
	}
	if !out[1].Unread {
		t.Error("UID 2 has no \\Seen, must be unread")
	}
}

// TestFingerprintMailbox_Stable confirms the fingerprint depends only on the
// (uid, flags, visible-labels) tuple, not on input ordering — flag reorder or
// row reorder both produce the same hash.
func TestFingerprintMailbox_Stable(t *testing.T) {
	a := []fingerprintInput{
		{UID: 2, Flags: []string{"\\Seen", "Flagged"}, VisibleLabels: []string{"work"}},
		{UID: 1, Flags: []string{"Flagged"}, VisibleLabels: []string{"urgent", "ci"}},
	}
	b := []fingerprintInput{
		// reordered rows + reordered flags + reordered labels
		{UID: 1, Flags: []string{"Flagged"}, VisibleLabels: []string{"ci", "urgent"}},
		{UID: 2, Flags: []string{"Flagged", "\\Seen"}, VisibleLabels: []string{"work"}},
	}
	if fingerprintMailbox(a) != fingerprintMailbox(b) {
		t.Error("fingerprint should not depend on input ordering")
	}
}

// TestFingerprintMailbox_DetectsLabelFlip confirms a label change shifts the
// hash — without that, scheduled modes can't tell when to refire.
func TestFingerprintMailbox_DetectsLabelFlip(t *testing.T) {
	before := []fingerprintInput{{UID: 1, VisibleLabels: []string{"ci"}}}
	after := []fingerprintInput{{UID: 1, VisibleLabels: []string{"ci", "success"}}}
	if fingerprintMailbox(before) == fingerprintMailbox(after) {
		t.Error("adding a visible label must change the fingerprint")
	}
}

// TestVisibleLabels_FiltersHidden confirms the deny-list drops hidden labels
// case-insensitively but leaves the original case of visible labels intact.
func TestVisibleLabels_FiltersHidden(t *testing.T) {
	st := newDigestState()
	st.setHidden([]string{"Internal", "slack-marker"})

	got := st.visibleLabels([]string{"Urgent", "INTERNAL", "Slack-Marker", "ci"})
	want := []string{"Urgent", "ci"}
	if len(got) != len(want) {
		t.Fatalf("visibleLabels = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("visibleLabels[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestVisibleLabels_NoDenyListReturnsInput confirms the fast path doesn't
// reshape labels when nothing is hidden.
func TestVisibleLabels_NoDenyListReturnsInput(t *testing.T) {
	st := newDigestState()
	in := []string{"a", "b"}
	out := st.visibleLabels(in)
	if len(out) != 2 || out[0] != "a" || out[1] != "b" {
		t.Errorf("empty deny-list should pass labels through; got %v", out)
	}
}

func TestResolveWebhookKey(t *testing.T) {
	s := &server{cfg: pluginConfig{
		Webhooks:       map[string]string{"general": "u1", "alerts": "u2"},
		DefaultWebhook: "general",
	}}
	if got := s.resolveWebhookKey("alerts"); got != "alerts" {
		t.Errorf("explicit key got %q, want alerts", got)
	}
	if got := s.resolveWebhookKey(""); got != "general" {
		t.Errorf("empty key should fall back to default; got %q", got)
	}
	if got := s.resolveWebhookKey("unknown"); got != "general" {
		t.Errorf("unknown key should fall back to default; got %q", got)
	}
	s.cfg.DefaultWebhook = ""
	if got := s.resolveWebhookKey("unknown"); got != "" {
		t.Errorf("no default + unknown key should give empty; got %q", got)
	}
}

func TestApplyDefaults(t *testing.T) {
	c := pluginConfig{}
	c.applyDefaults()
	if c.Backend != "claude" {
		t.Errorf("default backend = %q, want claude", c.Backend)
	}
	if want := []string{"opencode", "run"}; !reflect.DeepEqual(c.OpenCodeCommand, want) {
		t.Errorf("default opencode_command = %v, want %v", c.OpenCodeCommand, want)
	}
	if c.Mode != "per_run" {
		t.Errorf("default mode = %q, want per_run", c.Mode)
	}
	if c.aiTimeout != 45*time.Second {
		t.Errorf("default ai_timeout = %s, want 45s", c.aiTimeout)
	}
	if c.MinBatch != 1 || c.MaxBatch != 100 {
		t.Errorf("default batch bounds = (%d, %d), want (1, 100)", c.MinBatch, c.MaxBatch)
	}
	if c.IncludeBodyChars != 600 {
		t.Errorf("per_run mode should default IncludeBodyChars to 600; got %d", c.IncludeBodyChars)
	}

	c2 := pluginConfig{Mode: "cron", AITimeoutStr: "10s"}
	c2.applyDefaults()
	if c2.IncludeBodyChars != 0 {
		t.Errorf("scheduled modes should leave IncludeBodyChars=0; got %d", c2.IncludeBodyChars)
	}
	if c2.aiTimeout != 10*time.Second {
		t.Errorf("ai_timeout override = %s, want 10s", c2.aiTimeout)
	}
}

func TestBackendCommand(t *testing.T) {
	c := pluginConfig{
		ClaudeCommand:   []string{"claude", "-p"},
		GeminiCommand:   []string{"gemini", "-p"},
		OpenCodeCommand: []string{"opencode", "run"},
	}
	tests := []struct {
		backend string
		want    []string
	}{
		{"claude", c.ClaudeCommand},
		{"Claude", c.ClaudeCommand},
		{"gemini", c.GeminiCommand},
		{"opencode", c.OpenCodeCommand},
		{"OpenCode", c.OpenCodeCommand},
		{"", c.ClaudeCommand},
		{"codex", nil},
	}
	for _, tt := range tests {
		c.Backend = tt.backend
		if got := c.backendCommand(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("backend %q: got %v, want %v", tt.backend, got, tt.want)
		}
	}
}

func TestTruncateBody(t *testing.T) {
	if got := truncateBody("hello", 100); got != "hello" {
		t.Errorf("short body should pass through; got %q", got)
	}
	if got := truncateBody("hello world", 5); !strings.HasPrefix(got, "hello") || !strings.Contains(got, "truncated") {
		t.Errorf("overflow body should be truncated with marker; got %q", got)
	}
	if got := truncateBody("xxx", 0); got != "xxx" {
		t.Errorf("max=0 should pass through (no limit); got %q", got)
	}
}

// TestNormalizeToMrkdwn confirms Markdown the model leaks is rewritten into
// Slack mrkdwn: ** -> *, ## headings -> bold, [a](url) -> <url|a>, - bullets
// -> •, rules dropped, blank runs collapsed.
func TestNormalizeToMrkdwn(t *testing.T) {
	in := "## HUG issues\n" +
		"**@Helene** — review\n" +
		"- [#2279](https://x/2279) thread\n" +
		"* second item\n" +
		"---\n\n\n" +
		"__done__"
	got := normalizeToMrkdwn(in)
	for _, want := range []string{
		"*HUG issues*",
		"*@Helene* — review",
		"• <https://x/2279|#2279> thread",
		"• second item",
		"*done*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "## ") || strings.Contains(got, "---") {
		t.Errorf("markdown leaked through:\n%s", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("blank runs not collapsed:\n%s", got)
	}
}

// TestBuildChangesPrompt confirms the prompt names the right batch size and
// renders one entry per email, with body excerpt included in per_run shape.
func TestBuildChangesPrompt(t *testing.T) {
	emails := []bufferedEmail{
		{UID: 1, Account: "work", Mailbox: "INBOX", Subject: "Deploy", From: "ci@x", Body: "ok"},
		{UID: 2, Account: "work", Mailbox: "INBOX", Subject: "MR", From: "gitlab@x"},
	}
	out := buildChangesPrompt(emails, true, "")
	if !strings.Contains(out, "batch of 2 new emails") {
		t.Errorf("prompt missing batch count: %q", out)
	}
	if !strings.Contains(out, `From: ci@x — "Deploy"`) {
		t.Errorf("prompt missing first email: %q", out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("per_run prompt should include body excerpt: %q", out)
	}
}

// TestBuildWholeMailboxPrompt confirms scheduled-mode prompts omit body
// excerpts (subject + from + labels only).
func TestBuildWholeMailboxPrompt(t *testing.T) {
	emails := []bufferedEmail{
		{UID: 1, Subject: "Hi", From: "a@x", Body: "should not be in prompt", Labels: []string{"work"}},
	}
	out := buildWholeMailboxPrompt(emails, true, "")
	if strings.Contains(out, "should not be in prompt") {
		t.Errorf("snapshot prompt should not include body excerpts: %q", out)
	}
	if !strings.Contains(out, "labels: [work]") {
		t.Errorf("snapshot prompt should surface visible labels: %q", out)
	}
}

// TestPrompt_CustomInstructions confirms configured instructions replace the
// default structure guidance while mrkdwn rules and the email list remain.
func TestPrompt_CustomInstructions(t *testing.T) {
	emails := []bufferedEmail{{Subject: "Hi", From: "a@x"}}
	custom := "Use exactly three sections: Urgent, FYI, Ignore."

	for name, out := range map[string]string{
		"changes":  buildChangesPrompt(emails, false, custom),
		"snapshot": buildWholeMailboxPrompt(emails, false, custom),
	} {
		if !strings.Contains(out, custom) {
			t.Errorf("%s: prompt missing custom instructions: %q", name, out)
		}
		if strings.Contains(out, "Group by sender or topic") {
			t.Errorf("%s: default guidance should be replaced: %q", name, out)
		}
		if !strings.Contains(out, "Slack mrkdwn") {
			t.Errorf("%s: mrkdwn rules must always apply: %q", name, out)
		}
		if !strings.Contains(out, `From: a@x — "Hi"`) {
			t.Errorf("%s: email listing missing: %q", name, out)
		}
	}
}

func TestRenderFallbackListing_Cap(t *testing.T) {
	emails := make([]bufferedEmail, 25)
	for i := range emails {
		emails[i] = bufferedEmail{Subject: "s", From: "f"}
	}
	out := renderFallbackListing(emails)
	if !strings.Contains(out, "and 5 more") {
		t.Errorf("listing should cap at 20 with overflow marker; got %q", out)
	}
}

// TestDigestStateBuffer confirms appendBuffer + drainBuffer round-trip and
// that max-cap drops oldest entries.
func TestDigestStateBuffer(t *testing.T) {
	st := newDigestState()
	for i := 0; i < 5; i++ {
		st.appendBuffer("k", bufferedEmail{UID: uint32(i + 1)}, 3)
	}
	emails := st.drainBuffer("k")
	if len(emails) != 3 {
		t.Fatalf("max-cap=3 should keep 3 entries; got %d", len(emails))
	}
	if emails[0].UID != 3 || emails[2].UID != 5 {
		t.Errorf("max-cap should drop oldest; got UIDs %d…%d", emails[0].UID, emails[2].UID)
	}
	if got := st.drainBuffer("k"); len(got) != 0 {
		t.Errorf("drain should leave the buffer empty; got %d", len(got))
	}
}

func TestBuildManifest_ModeSwitchesOptIns(t *testing.T) {
	s := &server{cfg: pluginConfig{Mode: "per_run", IncludeBodyChars: 600}}
	s.cfg.applyDefaults()
	m := s.buildManifest()
	if m["wants_batch_complete"] != true {
		t.Error("per_run manifest must opt into batch_complete")
	}
	if _, ok := m["wants_schedule"]; ok {
		t.Error("per_run manifest should not declare wants_schedule")
	}
	wants, _ := m["wants"].([]string)
	hasBody := false
	for _, w := range wants {
		if w == "body" {
			hasBody = true
		}
	}
	if !hasBody {
		t.Errorf("IncludeBodyChars>0 should add 'body' to wants; got %v", wants)
	}

	s2 := &server{cfg: pluginConfig{Mode: "cron", Schedule: "@daily"}}
	s2.cfg.applyDefaults()
	m2 := s2.buildManifest()
	if m2["wants_schedule"] != true || m2["schedule"] != "@daily" {
		t.Errorf("cron manifest missing schedule fields: %+v", m2)
	}
	if _, ok := m2["wants_batch_complete"]; ok {
		t.Error("cron-only manifest should not opt into batch_complete")
	}
}
