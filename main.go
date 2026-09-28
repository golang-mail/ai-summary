// ai-summary buffers emails seen during a filter run and posts a rolled-up
// Slack digest. Three trigger modes — per_run (flush on batch_complete),
// interval (scheduled_tick at N minutes), and cron (scheduled_tick at a
// wall-clock expression). Modes B/C pull mailbox state via host.* RPC and
// only fire when the mailbox actually changed since the last digest.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type envelope struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"` // "request" | "response" | "event"
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type initRequest struct {
	HostVersion string          `json:"host_version"`
	Config      json.RawMessage `json:"config,omitempty"`
}

// pluginConfig is decoded from the host's init payload (the operator's
// `config:` block in plugins.yml, re-serialized as JSON).
type pluginConfig struct {
	Backend          string            `json:"backend"`        // "claude" | "gemini" | "opencode"
	ClaudeCommand    []string          `json:"claude_command"` // argv passed to exec
	GeminiCommand    []string          `json:"gemini_command"`
	OpenCodeCommand  []string          `json:"opencode_command"`
	AITimeoutStr     string            `json:"ai_timeout"`   // duration string; falls back to 45s
	Mode             string            `json:"mode"`         // "per_run" | "interval" | "cron"
	Schedule         string            `json:"schedule"`     // duration or cron — required for interval/cron
	Instructions     string            `json:"instructions"` // custom digest structure/style; default prompt when empty
	Mailboxes        []mailboxRef      `json:"mailboxes"`    // configured target mailboxes for scheduled modes
	IncludeBodyChars int               `json:"include_body_chars"`
	IncludeLabels    bool              `json:"include_labels"`
	MinBatch         int               `json:"min_batch"` // floor for AI call in per_run mode; default 1
	MaxBatch         int               `json:"max_batch"` // safety cap; default 100
	Webhooks         map[string]string `json:"webhooks"`
	DefaultWebhook   string            `json:"default_webhook"`
	Username         string            `json:"username"`
	IconURL          string            `json:"icon_url"`
	Channel          string            `json:"channel"`
	Verbose          bool              `json:"verbose"`

	aiTimeout time.Duration // resolved at init
}

type mailboxRef struct {
	Account string `json:"account"`
	Mailbox string `json:"mailbox"`
}

// reqIDCounter generates unique request IDs for host.* calls. Each plugin
// process is its own ID space — host doesn't care about the format, only
// that responses round-trip the same id back.
var reqIDCounter atomic.Uint64

func newReqID() string {
	return fmt.Sprintf("ai-summary-%d", reqIDCounter.Add(1))
}

func main() {
	log.SetFlags(0)
	path := os.Getenv("GO_MAIL_PLUGIN_SOCKET")
	if path == "" {
		log.Fatal("GO_MAIL_PLUGIN_SOCKET not set")
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		log.Fatalf("listen %s: %v", path, err)
	}
	_ = os.Chmod(path, 0o600)
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go newServer(conn).readLoop()
	}
}

// server owns one host↔plugin connection. Long-lived (single connection
// per supervisor run); the readLoop demuxes incoming envelopes by type.
//
// writeMu serializes envelope writes since both readLoop (responses to host
// requests) and outbound host calls (host.* RPC) can write concurrently.
//
// inflight tracks pending host.* RPC requests by ID; the reader sees a
// "response" envelope and routes it to the waiting channel.
type server struct {
	conn       net.Conn
	out        *json.Encoder
	writeMu    sync.Mutex
	inflightMu sync.Mutex
	inflight   map[string]chan responseResult

	cfg   pluginConfig
	state *digestState
}

type responseResult struct {
	payload json.RawMessage
	err     error
}

func newServer(conn net.Conn) *server {
	return &server{
		conn:     conn,
		out:      json.NewEncoder(conn),
		inflight: make(map[string]chan responseResult),
		state:    newDigestState(),
	}
}

func (s *server) readLoop() {
	defer s.conn.Close()
	in := bufio.NewScanner(s.conn)
	in.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for in.Scan() {
		var msg envelope
		if err := json.Unmarshal(in.Bytes(), &msg); err != nil {
			log.Printf("bad envelope: %v", err)
			continue
		}
		switch msg.Type {
		case "request":
			s.handleRequest(msg)
		case "response":
			s.routeResponse(msg)
		case "event":
			// Off the readLoop: event handlers make host RPC calls whose
			// responses are delivered here — running inline would deadlock.
			go s.handleEvent(msg)
		}
	}
	// Reader exit: cancel any in-flight host calls so callers don't block.
	s.inflightMu.Lock()
	for id, ch := range s.inflight {
		ch <- responseResult{err: errors.New("connection closed")}
		delete(s.inflight, id)
	}
	s.inflightMu.Unlock()
}

func (s *server) handleRequest(msg envelope) {
	switch msg.Method {
	case "init":
		s.handleInit(msg)
	case "evaluate":
		s.handleEvaluate(msg)
	case "shutdown":
		s.handleShutdown(msg)
	default:
		s.respond(msg.ID, msg.Method, map[string]any{"error": "unknown method: " + msg.Method})
	}
}

func (s *server) handleEvent(msg envelope) {
	s.logf("info", "event received method=%q payload=%s", msg.Method, string(msg.Payload))
	switch msg.Method {
	case "batch_complete":
		s.handleBatchComplete(msg)
	case "scheduled_tick":
		s.handleScheduledTick(msg)
	default:
		s.logf("debug", "ignoring event method=%q", msg.Method)
	}
}

func (s *server) routeResponse(msg envelope) {
	s.inflightMu.Lock()
	ch, ok := s.inflight[msg.ID]
	delete(s.inflight, msg.ID)
	s.inflightMu.Unlock()
	if !ok {
		return
	}
	// Host wraps errors as {"error": "..."} in the payload, not as a wire-level
	// failure. The caller is responsible for inspecting the response shape.
	ch <- responseResult{payload: msg.Payload}
}

func (s *server) handleInit(msg envelope) {
	var req initRequest
	_ = json.Unmarshal(msg.Payload, &req)
	if len(req.Config) > 0 {
		if err := json.Unmarshal(req.Config, &s.cfg); err != nil {
			s.logf("error", "decode plugin config: %v", err)
		}
	}
	s.cfg.applyDefaults()

	manifest := s.buildManifest()
	raw, _ := json.Marshal(manifest)
	s.respond(msg.ID, "init", json.RawMessage(raw))
	s.logf("info", "initialized: mode=%s schedule=%q webhooks=%d backend=%s verbose=%v",
		s.cfg.Mode, s.cfg.Schedule, len(s.cfg.Webhooks), s.cfg.Backend, s.cfg.Verbose)

	// Prime the hidden-label deny-list so the first evaluate / tick sees an
	// up-to-date view; refreshed on each scheduled_tick too. Failures are
	// non-fatal — empty deny-list just means "no labels are hidden".
	// Async: this host RPC's response is delivered by readLoop, so running it
	// inline (we're on readLoop here) would deadlock.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.refreshHiddenLabels(ctx); err != nil {
			s.logf("warn", "init: refresh hidden labels: %v", err)
		}
	}()
}

func (s *server) buildManifest() map[string]any {
	wants := []string{"envelope"}
	if s.cfg.IncludeBodyChars > 0 {
		wants = append(wants, "body")
	}
	m := map[string]any{
		"name":      "ai-summary",
		"version":   "0.3.2",
		"actions":   []string{},
		"wants":     wants,
		"cacheable": true,
		"run_last":  true, // default, but explicit for clarity
		// Cleanup filters should fire first so the digest reflects the
		// post-move/delete state, not the raw arrival batch.
	}
	switch s.cfg.Mode {
	case "per_run", "":
		m["wants_batch_complete"] = true
		// Mailbox state is only worth requesting in changes scope when the
		// operator explicitly opted in (whole_mailbox scope, currently not
		// surfaced as a per-webhook config — v1 keeps changes-only).
	case "interval", "cron":
		m["wants_schedule"] = true
		m["schedule"] = s.cfg.Schedule
		m["host_methods"] = []string{"list_emails", "get_email", "list_labels"}
	}
	return m
}

func (s *server) handleShutdown(msg envelope) {
	// Drain per_run buffers synchronously so any unflushed digest still posts.
	s.flushAllBuffers(context.Background(), "shutdown")
	s.respond(msg.ID, "shutdown", map[string]any{})
	_ = s.conn.Close()
}

func (s *server) respond(id, method string, payload any) {
	var raw json.RawMessage
	switch v := payload.(type) {
	case json.RawMessage:
		raw = v
	case []byte:
		raw = v
	default:
		raw, _ = json.Marshal(v)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.out.Encode(envelope{
		ID:      id,
		Type:    "response",
		Method:  method,
		Payload: raw,
	})
}

// callHost issues a host.* RPC and blocks until the response (or ctx) lands.
// Returns the raw payload — the caller decodes (and inspects {"error":...}).
func (s *server) callHost(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	id := newReqID()
	ch := make(chan responseResult, 1)
	s.inflightMu.Lock()
	s.inflight[id] = ch
	s.inflightMu.Unlock()
	defer func() {
		s.inflightMu.Lock()
		delete(s.inflight, id)
		s.inflightMu.Unlock()
	}()

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s req: %w", method, err)
	}
	s.writeMu.Lock()
	err = s.out.Encode(envelope{ID: id, Type: "request", Method: method, Payload: raw})
	s.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("write %s req: %w", method, err)
	}

	select {
	case r := <-ch:
		return r.payload, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *pluginConfig) applyDefaults() {
	if c.Backend == "" {
		c.Backend = "claude"
	}
	if len(c.ClaudeCommand) == 0 {
		c.ClaudeCommand = []string{"claude", "-p"}
	}
	if len(c.GeminiCommand) == 0 {
		c.GeminiCommand = []string{"gemini", "-p"}
	}
	if len(c.OpenCodeCommand) == 0 {
		c.OpenCodeCommand = []string{"opencode", "run"}
	}
	if c.AITimeoutStr == "" {
		c.aiTimeout = 45 * time.Second
	} else if d, err := time.ParseDuration(c.AITimeoutStr); err == nil {
		c.aiTimeout = d
	} else {
		log.Printf("invalid ai_timeout %q, using 45s", c.AITimeoutStr)
		c.aiTimeout = 45 * time.Second
	}
	if c.Mode == "" {
		c.Mode = "per_run"
	}
	if c.MinBatch <= 0 {
		c.MinBatch = 1
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = 100
	}
	if c.Username == "" {
		c.Username = "go-mail digest"
	}
	if c.IncludeBodyChars == 0 && c.Mode == "per_run" {
		c.IncludeBodyChars = 600
	}
}
