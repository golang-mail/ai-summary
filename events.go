package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"
)

type batchCompleteEvent struct {
	RunID     string    `json:"run_id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Trigger   string    `json:"trigger"`
}

type scheduledTickEvent struct {
	FiredAt  time.Time `json:"fired_at"`
	Schedule string    `json:"schedule"`
	Since    time.Time `json:"since,omitempty"`
	Force    bool      `json:"force,omitempty"`
}

// handleBatchComplete flushes all per_run buffers (one Slack post per webhook
// key with buffered emails). No-op for interval/cron modes.
func (s *server) handleBatchComplete(msg envelope) {
	if s.cfg.Mode != "per_run" && s.cfg.Mode != "" {
		return
	}
	var ev batchCompleteEvent
	_ = json.Unmarshal(msg.Payload, &ev)
	if s.cfg.Verbose {
		log.Printf("batch_complete run_id=%s trigger=%s — flushing buffers", ev.RunID, ev.Trigger)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.aiTimeout+30*time.Second)
	defer cancel()
	s.flushAllBuffers(ctx, ev.Trigger)
}

// flushAllBuffers iterates keys with buffered emails and posts a digest per
// key. Each key serializes through its own dispatch goroutine to keep Slack
// posts ordered; across keys we fan out.
func (s *server) flushAllBuffers(ctx context.Context, trigger string) {
	keys := s.state.keysWithBufferedEmails()
	for _, key := range keys {
		s.flushKey(ctx, key, trigger)
	}
}

func (s *server) flushKey(ctx context.Context, key, trigger string) {
	emails := s.state.drainBuffer(key)
	if len(emails) == 0 {
		return
	}
	url := s.cfg.Webhooks[key]
	if url == "" {
		log.Printf("flush key=%s: webhook URL not configured, dropping %d email(s)", key, len(emails))
		return
	}

	// Min-batch floor: skip the AI call for a single email and post a
	// one-line subject. Cheaper, more readable, no LLM context.
	if len(emails) < s.cfg.MinBatch {
		// MinBatch < 1 already coerced to 1 in applyDefaults, so MinBatch=1
		// means "always go through the AI path even for one email" only if
		// the operator bumped MinBatch above 1.
		s.postOneLiner(url, emails, trigger)
		return
	}

	summary, fallback := s.summarizeChanges(ctx, emails)
	body := summary
	via := s.cfg.Backend
	if fallback {
		body = renderFallbackListing(emails)
		via = s.cfg.Backend + " (fallback)"
	}

	msg := buildDigestMessage(digestMessage{
		Cfg:        &s.cfg,
		Header:     fmt.Sprintf("Inbox digest — %d new emails", len(emails)),
		Context:    contextLine(emails, trigger),
		Body:       body,
		ViaBackend: via,
	})
	if err := postSlack(url, msg); err != nil {
		log.Printf("flush key=%s: slack post: %v", key, err)
		return
	}
	if s.cfg.Verbose {
		log.Printf("flush key=%s posted (%d emails, fallback=%v)", key, len(emails), fallback)
	}
}

// postOneLiner sends a short single-email digest with no AI call.
func (s *server) postOneLiner(url string, emails []bufferedEmail, trigger string) {
	if len(emails) == 0 {
		return
	}
	e := emails[0]
	subject := e.Subject
	if subject == "" {
		subject = "(no subject)"
	}
	msg := buildDigestMessage(digestMessage{
		Cfg:        &s.cfg,
		Header:     fmt.Sprintf("1 new mail: %s", subject),
		Context:    contextLine(emails, trigger),
		Body:       fmt.Sprintf("From: %s", e.From),
		ViaBackend: "no-ai (min_batch)",
	})
	if err := postSlack(url, msg); err != nil {
		log.Printf("one-liner post: %v", err)
	}
}

// handleScheduledTick is the entry point for interval / cron modes. Refresh
// the hidden-label set, then for each configured webhook key compare the
// current mailbox state against the last fingerprint and post a snapshot
// digest when it changed. When ev.Force is set (operator-initiated trigger
// via the UI), the fingerprint check is bypassed so the operator can
// validate the side-effecting path even on an unchanged mailbox.
func (s *server) handleScheduledTick(msg envelope) {
	if s.cfg.Mode != "interval" && s.cfg.Mode != "cron" {
		return
	}
	var ev scheduledTickEvent
	_ = json.Unmarshal(msg.Payload, &ev)
	if s.cfg.Verbose || ev.Force {
		log.Printf("scheduled_tick schedule=%s since=%s force=%v", ev.Schedule, ev.Since.Format(time.RFC3339), ev.Force)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.aiTimeout+30*time.Second)
	defer cancel()

	// Refresh the deny-list each tick so label-config edits take effect
	// without a plugin restart.
	if err := s.refreshHiddenLabels(ctx); err != nil {
		log.Printf("scheduled_tick: refresh hidden labels: %v", err)
	}

	for key := range s.cfg.Webhooks {
		if err := s.runScheduledKey(ctx, key, ev); err != nil {
			log.Printf("scheduled_tick key=%s: %v", key, err)
		}
	}
}

// runScheduledKey is the per-key body of handleScheduledTick. Pulls the
// current mailbox state, fingerprints it (visible-labels only), and decides
// whether to send a digest.
func (s *server) runScheduledKey(ctx context.Context, key string, ev scheduledTickEvent) error {
	url := s.cfg.Webhooks[key]
	if url == "" {
		return fmt.Errorf("webhook url missing for key %q", key)
	}
	if len(s.cfg.Mailboxes) == 0 {
		return fmt.Errorf("no mailboxes configured for scheduled mode")
	}

	// Gather every email across configured mailboxes. With multiple mailboxes
	// we concatenate the per-mailbox EmailRef lists.
	var refs []emailRef
	for _, m := range s.cfg.Mailboxes {
		resp, err := s.listEmails(ctx, listEmailsReq{Account: m.Account, Mailbox: m.Mailbox})
		if err != nil {
			return fmt.Errorf("list_emails %s/%s: %w", m.Account, m.Mailbox, err)
		}
		refs = append(refs, resp.Emails...)
	}

	// Build the fingerprint with hidden labels filtered out so a flip-only on
	// a hidden label does not look like a change.
	fpInputs := make([]fingerprintInput, 0, len(refs))
	for _, r := range refs {
		fpInputs = append(fpInputs, fingerprintInput{
			UID:           r.UID,
			Flags:         r.Flags,
			VisibleLabels: s.state.visibleLabels(r.Labels),
		})
	}
	current := fingerprintMailbox(fpInputs)
	if !ev.Force {
		if last := s.state.lastFingerprint(key); last == current {
			if s.cfg.Verbose {
				log.Printf("scheduled_tick key=%s: no change since last fire, skipping", key)
			}
			return nil
		}
	} else {
		log.Printf("scheduled_tick key=%s: force=true, bypassing fingerprint gate", key)
	}

	// State changed — render and post.
	emails := refsToBuffered(refs, s.state, s.cfg.IncludeBodyChars)
	summary, fallback := s.summarizeWholeMailbox(ctx, emails)
	body := summary
	via := s.cfg.Backend
	if fallback {
		body = renderFallbackListing(emails)
		via = s.cfg.Backend + " (fallback)"
	}
	header := fmt.Sprintf("Inbox snapshot — %d emails", len(emails))
	if len(s.cfg.Mailboxes) == 1 {
		header = fmt.Sprintf("Inbox snapshot — %d emails in %s", len(emails), s.cfg.Mailboxes[0].Mailbox)
	}
	msg := buildDigestMessage(digestMessage{
		Cfg:        &s.cfg,
		Header:     header,
		Context:    fmt.Sprintf("trigger: %s  •  fired %s", s.cfg.Mode, ev.FiredAt.Format("Jan 02 15:04 MST")),
		Body:       body,
		ViaBackend: via,
	})
	if err := postSlack(url, msg); err != nil {
		return fmt.Errorf("slack post: %w", err)
	}
	s.state.markFired(key, current, ev.FiredAt)
	if s.cfg.Verbose {
		log.Printf("scheduled_tick key=%s posted (%d emails, fallback=%v)", key, len(emails), fallback)
	}
	return nil
}

// refsToBuffered converts host.list_emails EmailRefs into the bufferedEmail
// shape the prompt builder expects. Bodies stay empty here — the listing path
// keeps prompts light; the per-email body excerpt is only sent in per_run mode
// where evaluate already supplied it.
func refsToBuffered(refs []emailRef, st *digestState, _ int) []bufferedEmail {
	out := make([]bufferedEmail, 0, len(refs))
	// Newest first to match how the UI surfaces emails.
	sort.Slice(refs, func(i, j int) bool { return refs[i].Date.After(refs[j].Date) })
	for _, r := range refs {
		be := bufferedEmail{
			UID:     r.UID,
			Account: r.Account,
			Mailbox: r.Mailbox,
			Labels:  st.visibleLabels(r.Labels),
			Date:    r.Date,
			Unread:  isUnread(r.Flags),
		}
		if r.Envelope != nil {
			be.Subject = r.Envelope.Subject
			be.From = formatAddresses(r.Envelope.From)
			if be.Date.IsZero() {
				be.Date = r.Envelope.Date
			}
		}
		out = append(out, be)
	}
	return out
}

// contextLine builds the small grey context line that appears under the
// header in the Slack digest message. Summarizes account/mailbox span + time.
func contextLine(emails []bufferedEmail, trigger string) string {
	if len(emails) == 0 {
		return fmt.Sprintf("trigger: %s", trigger)
	}
	// Aggregate distinct account/mailbox pairs so the line stays terse.
	type ref struct{ acct, mbox string }
	uniq := map[ref]struct{}{}
	for _, e := range emails {
		uniq[ref{e.Account, e.Mailbox}] = struct{}{}
	}
	pairs := make([]string, 0, len(uniq))
	for r := range uniq {
		pairs = append(pairs, r.acct+"/"+r.mbox)
	}
	sort.Strings(pairs)
	mboxes := pairs[0]
	if len(pairs) > 1 {
		mboxes = fmt.Sprintf("%s (+%d)", pairs[0], len(pairs)-1)
	}
	var earliest, latest time.Time
	for _, e := range emails {
		if !e.Date.IsZero() && (earliest.IsZero() || e.Date.Before(earliest)) {
			earliest = e.Date
		}
		if e.Date.After(latest) {
			latest = e.Date
		}
	}
	when := ""
	if !earliest.IsZero() {
		when = earliest.Format("Jan 02 15:04") + "–" + latest.Format("15:04")
	}
	out := mboxes + "  •  trigger: " + trigger
	if when != "" {
		out += "  •  " + when
	}
	return out
}
