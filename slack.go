package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// slackText / slackBlock / slackMessage mirror Slack's block-kit payload shape.
// Lifted from plugins/slack-notify — the two plugins intentionally stay
// standalone (no shared module), so we re-declare instead of importing.
type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type slackBlock struct {
	Type     string       `json:"type"`
	Text     *slackText   `json:"text,omitempty"`
	Elements []*slackText `json:"elements,omitempty"`
}

type slackMessage struct {
	Channel  string       `json:"channel,omitempty"`
	Username string       `json:"username,omitempty"`
	IconURL  string       `json:"icon_url,omitempty"`
	Text     string       `json:"text,omitempty"` // fallback for screen readers
	Blocks   []slackBlock `json:"blocks,omitempty"`
}

// digestMessage is what buildDigestMessage takes — small struct so the call
// sites are readable and we don't pass a half-dozen positional strings.
type digestMessage struct {
	Cfg        *pluginConfig
	Header     string
	Context    string
	Body       string
	ViaBackend string
}

func buildDigestMessage(d digestMessage) slackMessage {
	const maxBody = 2800 // Slack section text limit is 3000; leave room for formatting
	body := normalizeToMrkdwn(d.Body)
	if body == "" {
		body = "_(empty)_"
	}
	if len(body) > maxBody {
		body = body[:maxBody] + "\n…[truncated]"
	}
	blocks := []slackBlock{
		{Type: "section", Text: &slackText{Type: "mrkdwn", Text: "*" + escapeMrkdwn(d.Header) + "*"}},
	}
	if d.Context != "" {
		blocks = append(blocks, slackBlock{
			Type:     "context",
			Elements: []*slackText{{Type: "mrkdwn", Text: escapeMrkdwn(d.Context)}},
		})
	}
	blocks = append(blocks, slackBlock{
		Type: "section",
		Text: &slackText{Type: "mrkdwn", Text: body},
	})
	if d.ViaBackend != "" {
		blocks = append(blocks, slackBlock{
			Type:     "context",
			Elements: []*slackText{{Type: "mrkdwn", Text: "via " + escapeMrkdwn(d.ViaBackend)}},
		})
	}
	return slackMessage{
		Channel:  d.Cfg.Channel,
		Username: d.Cfg.Username,
		IconURL:  d.Cfg.IconURL,
		Text:     d.Header,
		Blocks:   blocks,
	}
}

// escapeMrkdwn escapes the three characters that have meaning in Slack's
// mrkdwn. Simpler than slack-notify's variant because the digest body comes
// from either the AI (prompted via slackFormatRules to emit clean mrkdwn) or
// the fallback listing (plain text). No HTML stripping needed.
//
// Note: only applied to header/context/"via" chrome, not the body — the body
// is the AI's mrkdwn and escaping it would defeat slackFormatRules.
func escapeMrkdwn(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// Markdown patterns the AI leaks despite slackFormatRules, compiled once.
var (
	mdHeading   = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`) // "## Title" -> "*Title*"
	mdBold      = regexp.MustCompile(`\*\*(.+?)\*\*`)                       // "**x**" -> "*x*"
	mdBoldUnder = regexp.MustCompile(`__(.+?)__`)                           // "__x__" -> "*x*"
	mdLink      = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+)\)`)  // "[a](url)" -> "<url|a>"
	mdBullet    = regexp.MustCompile(`(?m)^(\s*)[-*]\s+`)                   // "- " / "* " -> "• "
	mdRule      = regexp.MustCompile(`(?m)^\s*(?:-{3,}|\*{3,}|_{3,})\s*$`)  // "---"/"***" rule -> drop
	blankRuns   = regexp.MustCompile(`\n{3,}`)                              // collapse 3+ blank lines
)

// normalizeToMrkdwn converts Markdown the model leaks into Slack mrkdwn, since
// the body renders verbatim in a section block. slackFormatRules asks for
// mrkdwn but the model often disobeys (**bold**, ## headings, [a](url)); this
// is the reliable backstop. Order matters: links before bold so "[**a**](url)"
// survives, headings before bullets so a "# - item" line isn't double-touched.
func normalizeToMrkdwn(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = mdLink.ReplaceAllString(s, "<$2|$1>")
	s = mdRule.ReplaceAllString(s, "") // before bullets so "***" isn't read as a bullet
	s = mdHeading.ReplaceAllString(s, "*$1*")
	s = mdBold.ReplaceAllString(s, "*$1*")
	s = mdBoldUnder.ReplaceAllString(s, "*$1*")
	s = mdBullet.ReplaceAllString(s, "$1• ")
	s = blankRuns.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func formatAddresses(addrs []*address) string {
	if len(addrs) == 0 {
		return "(unknown sender)"
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a == nil {
			continue
		}
		email := a.MailboxName + "@" + a.HostName
		if a.PersonalName != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", a.PersonalName, email))
		} else {
			parts = append(parts, email)
		}
	}
	return strings.Join(parts, ", ")
}

func postSlack(url string, msg slackMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	return nil
}
