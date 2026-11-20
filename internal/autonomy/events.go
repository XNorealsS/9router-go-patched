package autonomy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Category identifies the subsystem that triggered an autonomous action.
type Category string

const (
	CategoryOAuth       Category = "oauth"
	CategoryCatalog     Category = "catalog"
	CategoryProject     Category = "project"
	CategoryRouting     Category = "routing"
	CategoryRecovery    Category = "recovery"
	CategorySession     Category = "session"
	CategoryDeprecation Category = "deprecation"
)

// Outcome describes the result of an autonomous detection/recovery step.
type Outcome string

const (
	OutcomeOK         Outcome = "ok"
	OutcomeDegraded   Outcome = "degraded"
	OutcomeFailed     Outcome = "failed"
	OutcomeNeedsAdmin Outcome = "needs_admin"
)

// Event records a single traceable autonomous observation and action.
type Event struct {
	ID           string   `json:"id"`
	Timestamp    string   `json:"timestamp"`
	Category     Category `json:"category"`
	Provider     string   `json:"provider,omitempty"`
	ConnectionID string   `json:"connectionId,omitempty"`
	Model        string   `json:"model,omitempty"`
	Detected     string   `json:"detected"`
	Action       string   `json:"action"`
	Reason       string   `json:"reason"`
	Outcome      Outcome  `json:"outcome"`
	NeedsAdmin   bool     `json:"needsAdmin"`
	Detail       string   `json:"detail,omitempty"`
}

// SessionInfo tracks redacted continuity diagnostics for coding-agent turns.
type SessionInfo struct {
	SessionKey        string `json:"sessionKey"`
	Provider          string `json:"provider"`
	ConnectionID      string `json:"connectionId,omitempty"`
	Model             string `json:"model"`
	Turns             int    `json:"turns"`
	ToolCalls         int    `json:"toolCalls"`
	ToolResults       int    `json:"toolResults"`
	HasThoughtSig     bool   `json:"hasThoughtSig"`
	Status            string `json:"status"`
	StartedAt         string `json:"startedAt"`
	LastSeenAt        string `json:"lastSeenAt"`
	lastSeenUnixMilli int64
}

const (
	maxEventHistory   = 200
	maxTrackedSession = 64
	sessionTTL        = 30 * time.Minute
)

var eventSeq atomic.Uint64

// Recorder stores recent automation events and active coding-agent session affinity.
type Recorder struct {
	mu       sync.RWMutex
	events   []Event
	sessions map[string]*SessionInfo
}

// NewRecorder allocates a fresh thread-safe Recorder.
func NewRecorder() *Recorder {
	return &Recorder{
		sessions: make(map[string]*SessionInfo),
	}
}

// Record appends an automation event to the bounded history ring.
func (r *Recorder) Record(ev Event) Event {
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("evt_%d", eventSeq.Add(1))
	}
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if ev.Outcome == OutcomeNeedsAdmin {
		ev.NeedsAdmin = true
	}

	r.mu.Lock()
	r.events = append(r.events, ev)
	if len(r.events) > maxEventHistory {
		r.events = r.events[len(r.events)-maxEventHistory:]
	}
	r.mu.Unlock()
	return ev
}

// RecentEvents returns newest-first automation events, optionally filtered by category.
func (r *Recorder) RecentEvents(limit int, category string) []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}
	catFilter := strings.ToLower(strings.TrimSpace(category))
	out := make([]Event, 0, min(limit, len(r.events)))
	for i := len(r.events) - 1; i >= 0 && len(out) < limit; i-- {
		ev := r.events[i]
		if catFilter != "" && strings.ToLower(string(ev.Category)) != catFilter {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// PendingAdminEvents returns unique recent unresolved conditions that require admin attention.
func (r *Recorder) PendingAdminEvents() []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()

	latestByScope := make(map[string]Event)
	for _, ev := range r.events {
		scope := fmt.Sprintf("%s|%s|%s", ev.Category, ev.Provider, ev.ConnectionID)
		if !ev.NeedsAdmin && ev.Outcome == OutcomeOK {
			// A subsequent healthy event in the same scope clears the pending admin condition.
			delete(latestByScope, scope)
			continue
		}
		if ev.NeedsAdmin {
			latestByScope[scope] = ev
		}
	}

	out := make([]Event, 0, len(latestByScope))
	for _, ev := range latestByScope {
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })
	return out
}

// DeriveSessionKey hashes redacted client context so prompts are never stored in memory or logs.
func DeriveSessionKey(clientID, provider, seed string) string {
	h := sha256.Sum256([]byte(clientID + "|" + provider + "|" + seed))
	return "sess_" + hex.EncodeToString(h[:6])
}

// TrackSession updates diagnostics for a coding-agent conversation turn.
func (r *Recorder) TrackSession(sessionKey, provider, connectionID, model string, toolCalls, toolResults int, hasThoughtSig bool, status string) {
	if sessionKey == "" {
		return
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	r.mu.Lock()
	defer r.mu.Unlock()

	// Prune expired sessions before inserting.
	cutoff := now.Add(-sessionTTL).UnixMilli()
	for k, s := range r.sessions {
		if s.lastSeenUnixMilli < cutoff {
			delete(r.sessions, k)
		}
	}

	s, ok := r.sessions[sessionKey]
	if !ok {
		if len(r.sessions) >= maxTrackedSession {
			var oldestKey string
			var oldestTS int64
			for k, cand := range r.sessions {
				if oldestKey == "" || cand.lastSeenUnixMilli < oldestTS {
					oldestKey = k
					oldestTS = cand.lastSeenUnixMilli
				}
			}
			if oldestKey != "" {
				delete(r.sessions, oldestKey)
			}
		}
		s = &SessionInfo{
			SessionKey: sessionKey,
			StartedAt:  nowStr,
		}
		r.sessions[sessionKey] = s
	}

	s.Provider = provider
	if connectionID != "" {
		s.ConnectionID = connectionID
	}
	s.Model = model
	s.Turns++
	s.ToolCalls += toolCalls
	s.ToolResults += toolResults
	if hasThoughtSig {
		s.HasThoughtSig = true
	}
	if status != "" {
		s.Status = status
	}
	s.LastSeenAt = nowStr
	s.lastSeenUnixMilli = now.UnixMilli()
}

// GetSessionConnection returns the sticky connection ID for an active session if known.
func (r *Recorder) GetSessionConnection(sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.sessions[sessionKey]; ok {
		return s.ConnectionID
	}
	return ""
}

// ActiveSessions returns active coding-agent sessions ordered most recently seen first.
func (r *Recorder) ActiveSessions(limit int) []SessionInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cutoff := time.Now().UTC().Add(-sessionTTL).UnixMilli()
	out := make([]SessionInfo, 0, len(r.sessions))
	for _, s := range r.sessions {
		if s.lastSeenUnixMilli < cutoff {
			continue
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lastSeenUnixMilli > out[j].lastSeenUnixMilli })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
