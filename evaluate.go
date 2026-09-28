package main

import (
	"encoding/json"
	"log"
	"strings"
	"time"
)

// evalRequest is the host-side EvaluateRequest seen from the plugin wire.
// We mirror only the fields ai-summary actually uses; extra fields are
// ignored by JSON decode.
type evalRequest struct {
	Email        *evalEmail `json:"email"`
	FilterConfig string     `json:"filter_config,omitempty"`
}

type evalEmail struct {
	UID         uint32           `json:"uid"`
	Account     string           `json:"account"`
	Mailbox     string           `json:"mailbox"`
	Envelope    *envelopeSummary `json:"envelope,omitempty"`
	Body        string           `json:"body,omitempty"`
	ContentType string           `json:"content_type,omitempty"`
	Flags       []string         `json:"flags,omitempty"`
	Labels      []string         `json:"labels,omitempty"`
	Date        time.Time        `json:"date"`
}

// handleEvaluate buffers the email when this is the per_run mode and returns
// {} so the filter pipeline moves on. For interval/cron modes, evaluate is
// a no-op — those modes summarize from host.list_emails at tick time.
//
// FilterConfig carries the webhook key the filter routed to (the `value:` of
// the matching `plugin:ai-summary` condition). Empty key falls back to
// default_webhook so a single filter without an explicit key still works.
func (s *server) handleEvaluate(msg envelope) {
	var r evalRequest
	if err := json.Unmarshal(msg.Payload, &r); err != nil {
		s.respond(msg.ID, "evaluate", map[string]any{"error": "decode evaluate: " + err.Error()})
		return
	}
	if r.Email == nil {
		s.respond(msg.ID, "evaluate", map[string]any{})
		return
	}

	// Modes B/C don't buffer per-email; they pull state at tick time.
	if s.cfg.Mode != "per_run" && s.cfg.Mode != "" {
		s.respond(msg.ID, "evaluate", map[string]any{})
		return
	}

	key := s.resolveWebhookKey(r.FilterConfig)
	if key == "" {
		if s.cfg.Verbose {
			log.Printf("evaluate uid=%d: no webhook key resolved (filter_config=%q), skipping",
				r.Email.UID, r.FilterConfig)
		}
		s.respond(msg.ID, "evaluate", map[string]any{})
		return
	}

	be := bufferedEmail{
		UID:     r.Email.UID,
		Account: r.Email.Account,
		Mailbox: r.Email.Mailbox,
		Body:    truncateBody(r.Email.Body, s.cfg.IncludeBodyChars),
		Labels:  s.state.visibleLabels(r.Email.Labels),
		Unread:  isUnread(r.Email.Flags),
	}
	if r.Email.Envelope != nil {
		be.Subject = strings.TrimSpace(r.Email.Envelope.Subject)
		be.From = formatAddresses(r.Email.Envelope.From)
		be.Date = r.Email.Envelope.Date
	}

	size := s.state.appendBuffer(key, be, s.cfg.MaxBatch)
	if s.cfg.Verbose {
		log.Printf("evaluate uid=%d -> key=%s (buffer size=%d)", r.Email.UID, key, size)
	}

	s.respond(msg.ID, "evaluate", map[string]any{})
}

// resolveWebhookKey picks the webhook key from the filter_config value, falling
// back to default_webhook when the filter didn't specify one. Returns "" if
// the key isn't configured.
func (s *server) resolveWebhookKey(value string) string {
	if value != "" {
		if _, ok := s.cfg.Webhooks[value]; ok {
			return value
		}
	}
	if s.cfg.DefaultWebhook != "" {
		if _, ok := s.cfg.Webhooks[s.cfg.DefaultWebhook]; ok {
			return s.cfg.DefaultWebhook
		}
	}
	return ""
}

func truncateBody(body string, max int) string {
	if max <= 0 || len(body) <= max {
		return body
	}
	return body[:max] + "\n…[truncated]"
}
