package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

// Host RPC payloads. These mirror plugin/host_api.go on the host side — kept
// as locals so the plugin stays a standalone module (no shared import).

type listLabelsResp struct {
	Labels []labelInfo `json:"labels"`
	Error  string      `json:"error,omitempty"`
}

type labelInfo struct {
	Name    string `json:"name"`
	Hidden  bool   `json:"hidden,omitempty"`
	Storage string `json:"storage,omitempty"`
	Color   string `json:"color,omitempty"`
}

type listEmailsReq struct {
	Account  string    `json:"account"`
	Mailbox  string    `json:"mailbox"`
	Page     int       `json:"page,omitempty"`
	PageSize int       `json:"page_size,omitempty"`
	Unread   bool      `json:"unread,omitempty"`
	Since    time.Time `json:"since,omitempty"`
}

type listEmailsResp struct {
	Emails   []emailRef `json:"emails"`
	Total    int        `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Error    string     `json:"error,omitempty"`
}

type emailRef struct {
	UID      uint32           `json:"uid"`
	Account  string           `json:"account"`
	Mailbox  string           `json:"mailbox"`
	Envelope *envelopeSummary `json:"envelope,omitempty"`
	Flags    []string         `json:"flags,omitempty"`
	Labels   []string         `json:"labels,omitempty"`
	Date     time.Time        `json:"date"`
}

type envelopeSummary struct {
	Date    time.Time  `json:"Date"`
	Subject string     `json:"Subject"`
	From    []*address `json:"From"`
	To      []*address `json:"To"`
	Cc      []*address `json:"Cc"`
}

type address struct {
	PersonalName string `json:"PersonalName"`
	MailboxName  string `json:"MailboxName"`
	HostName     string `json:"HostName"`
}

type getEmailReq struct {
	Account string   `json:"account"`
	Mailbox string   `json:"mailbox"`
	UID     uint32   `json:"uid"`
	Parts   []string `json:"parts,omitempty"`
}

type getEmailResp struct {
	UID         uint32            `json:"uid"`
	Account     string            `json:"account"`
	Mailbox     string            `json:"mailbox"`
	Envelope    *envelopeSummary  `json:"envelope,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Flags       []string          `json:"flags,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	Date        time.Time         `json:"date"`
	Error       string            `json:"error,omitempty"`
}

// hostLogRequest mirrors plugin/protocol.go HostLogRequest. Sent fire-and-forget
// over the socket so leveled logs reach go-mail even when this plugin is
// external/dialed (no stderr path back to the host).
type hostLogRequest struct {
	Level  string            `json:"level,omitempty"`
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

// hostLog ships one log line to the host via host.log. Fire-and-forget: it does
// not register an inflight entry, so the host's response is harmlessly dropped
// by routeResponse. Returns false if the line couldn't be written (no
// connection yet / encode error) so callers can fall back to stderr.
func (s *server) hostLog(level, msg string, fields map[string]string) bool {
	if s == nil || s.out == nil {
		return false
	}
	payload, err := json.Marshal(hostLogRequest{Level: level, Msg: msg, Fields: fields})
	if err != nil {
		return false
	}
	s.writeMu.Lock()
	err = s.out.Encode(envelope{ID: newReqID(), Type: "request", Method: "host.log", Payload: payload})
	s.writeMu.Unlock()
	return err == nil
}

// logf formats a message and ships it to the host at the given level, falling
// back to stderr when the host channel isn't available (pre-connection/fatal).
func (s *server) logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !s.hostLog(level, msg, nil) {
		log.Printf("[%s] %s", level, msg)
	}
}

// fetchVisibleLabels returns the names of every label whose definition is NOT
// hidden — i.e. the set the UI surfaces. Used both as the deny-list for
// fingerprinting/prompts (hidden labels filtered out) and at init time.
func (s *server) fetchVisibleLabels(ctx context.Context) (hidden []string, err error) {
	raw, err := s.callHost(ctx, "host.list_labels", map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp listLabelsResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode list_labels: %w", err)
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	for _, l := range resp.Labels {
		if l.Hidden {
			hidden = append(hidden, l.Name)
		}
	}
	return hidden, nil
}

// refreshHiddenLabels primes the deny-list so subsequent prompt-building and
// fingerprinting drop hidden labels. Called at init and on each scheduled_tick.
func (s *server) refreshHiddenLabels(ctx context.Context) error {
	hidden, err := s.fetchVisibleLabels(ctx)
	if err != nil {
		return err
	}
	s.state.setHidden(hidden)
	if s.cfg.Verbose {
		log.Printf("hidden labels refreshed: %d entries", len(hidden))
	}
	return nil
}

// listEmails wraps host.list_emails. Caller passes the request payload as-is;
// the helper just handles the round-trip + error shape.
func (s *server) listEmails(ctx context.Context, req listEmailsReq) (*listEmailsResp, error) {
	raw, err := s.callHost(ctx, "host.list_emails", req)
	if err != nil {
		return nil, err
	}
	var resp listEmailsResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode list_emails: %w", err)
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return &resp, nil
}

// getEmail wraps host.get_email. Used to pull body for the small slice of
// emails we want to feed into the AI prompt; the bulk list comes from
// listEmails which is body-less.
func (s *server) getEmail(ctx context.Context, req getEmailReq) (*getEmailResp, error) {
	raw, err := s.callHost(ctx, "host.get_email", req)
	if err != nil {
		return nil, err
	}
	var resp getEmailResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode get_email: %w", err)
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return &resp, nil
}
