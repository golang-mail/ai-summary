package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// digestState bundles all mutable per-plugin runtime state behind one mutex.
// Lock order: state.mu before any per-keyState access.
type digestState struct {
	mu       sync.Mutex
	keys     map[string]*keyState
	hidden   map[string]struct{} // visible-label deny-list, refreshed via host.list_labels
	hiddenMu sync.RWMutex
}

// keyState holds per-webhook-key runtime data. Per_run mode uses buffer +
// lastFire; interval/cron modes use lastFingerprint + lastFire.
type keyState struct {
	buffer          []bufferedEmail
	lastFingerprint string
	lastFire        time.Time
}

// bufferedEmail is the snapshot we keep from each evaluate call. We hold
// values, not pointers into the host's payload — the host may free them
// after responding.
type bufferedEmail struct {
	UID     uint32
	Account string
	Mailbox string
	Subject string
	From    string
	Date    time.Time
	Body    string
	Labels  []string
	Unread  bool
}

// isUnread reports whether the IMAP flags lack \Seen.
func isUnread(flags []string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, "\\Seen") {
			return false
		}
	}
	return true
}

func newDigestState() *digestState {
	return &digestState{
		keys:   map[string]*keyState{},
		hidden: map[string]struct{}{},
	}
}

func (s *digestState) getOrCreate(key string) *keyState {
	s.mu.Lock()
	defer s.mu.Unlock()
	ks, ok := s.keys[key]
	if !ok {
		ks = &keyState{}
		s.keys[key] = ks
	}
	return ks
}

// appendBuffer adds an email to a key's per_run buffer. Returns the new size.
// Capped by max so an extreme backlog doesn't grow without bound — the cap is
// soft: oldest entries get dropped, the digest still describes "first N".
func (s *digestState) appendBuffer(key string, email bufferedEmail, max int) int {
	ks := s.getOrCreate(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	ks.buffer = append(ks.buffer, email)
	if max > 0 && len(ks.buffer) > max {
		ks.buffer = ks.buffer[len(ks.buffer)-max:]
	}
	return len(ks.buffer)
}

// drainBuffer returns the current buffer and clears it atomically.
func (s *digestState) drainBuffer(key string) []bufferedEmail {
	ks := s.getOrCreate(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ks.buffer
	ks.buffer = nil
	return out
}

// keysWithBufferedEmails returns the keys whose per_run buffer is non-empty.
// Used at flush time to decide which webhooks to post to.
func (s *digestState) keysWithBufferedEmails() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.keys))
	for k, v := range s.keys {
		if len(v.buffer) > 0 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// markFired records that the digest fired for `key` at `fired`, updating the
// fingerprint so the next tick can skip when nothing changed.
func (s *digestState) markFired(key, fingerprint string, fired time.Time) {
	ks := s.getOrCreate(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	ks.lastFingerprint = fingerprint
	ks.lastFire = fired
}

func (s *digestState) lastFingerprint(key string) string {
	ks := s.getOrCreate(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	return ks.lastFingerprint
}

// setHidden replaces the deny-list. Concurrent-safe.
func (s *digestState) setHidden(names []string) {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[strings.ToLower(n)] = struct{}{}
	}
	s.hiddenMu.Lock()
	s.hidden = set
	s.hiddenMu.Unlock()
}

// visibleLabels filters out any label whose lowercased name is in the deny-list.
// Returns labels in their original case — only the comparison is case-insensitive.
func (s *digestState) visibleLabels(labels []string) []string {
	s.hiddenMu.RLock()
	defer s.hiddenMu.RUnlock()
	if len(s.hidden) == 0 {
		return labels
	}
	out := labels[:0:0] // never share backing storage with the input
	for _, l := range labels {
		if _, hidden := s.hidden[strings.ToLower(l)]; hidden {
			continue
		}
		out = append(out, l)
	}
	return out
}

// fingerprintMailbox computes a stable hash of the (uid, flags, visible-labels)
// set for one mailbox snapshot. Used by interval/cron modes to detect "did
// anything I care about change since last fire?"
//
// Hidden labels are filtered out by the caller before passing labels in, so a
// label flip the UI doesn't show doesn't trigger a spurious digest.
func fingerprintMailbox(emails []fingerprintInput) string {
	// Sort by UID for determinism — the host may return arbitrary order.
	sort.Slice(emails, func(i, j int) bool { return emails[i].UID < emails[j].UID })

	h := sha256.New()
	for _, e := range emails {
		// Encode each email as `uid|flag1,flag2|label1,label2|`. Sorting flags
		// + labels first means flag-reordering by the IMAP server doesn't make
		// the fingerprint flap.
		fields := []string{e.UID2dec()}
		flags := append([]string(nil), e.Flags...)
		sort.Strings(flags)
		fields = append(fields, strings.Join(flags, ","))
		labels := append([]string(nil), e.VisibleLabels...)
		sort.Strings(labels)
		fields = append(fields, strings.Join(labels, ","))
		h.Write([]byte(strings.Join(fields, "|") + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fingerprintInput is the slice of one email that contributes to the
// dirty fingerprint. Kept separate from bufferedEmail so the caller can build
// it from either evaluate payloads or host.list_emails responses.
type fingerprintInput struct {
	UID           uint32
	Flags         []string
	VisibleLabels []string
}

// UID2dec returns the UID as a decimal string. Lifted out so the fingerprint
// builder doesn't import strconv just for one call.
func (f fingerprintInput) UID2dec() string {
	// Hand-roll since strconv import is undesirable in a tiny helper. UIDs
	// are uint32, so at most 10 digits.
	const digits = "0123456789"
	n := f.UID
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append(buf, digits[n%10])
		n /= 10
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
