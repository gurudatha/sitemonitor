// Package alert turns raw check results into state-change alerts and delivers them.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"sitemonitor/internal/checks"
	"sitemonitor/internal/config"
	"sitemonitor/internal/store"
)

var eventSeq atomic.Int64

// SiteKey is the incident key for a site/check problem.
func SiteKey(site, check string) string { return "site:" + site + "|" + check }

// NewKeyedEvent builds an event that belongs to the incident identified by key.
func NewKeyedEvent(node, kind, severity, key, msg string) store.Event {
	e := NewEvent(node, kind, severity, "", "", msg)
	e.Key = key
	return e
}

// NewEvent builds an event with a cluster-unique ID.
func NewEvent(node, kind, severity, site, check, msg string) store.Event {
	now := time.Now().UTC()
	return store.Event{
		ID:   fmt.Sprintf("%s-%d-%d", node, now.UnixNano(), eventSeq.Add(1)),
		Time: now, Node: node, Kind: kind, Severity: severity, Site: site, Check: check, Message: msg,
	}
}

// Notifier logs every event and posts it to an optional Slack-compatible webhook.
type Notifier struct {
	Webhook string
	client  *http.Client
}

func NewNotifier(webhook string) *Notifier {
	return &Notifier{Webhook: webhook, client: &http.Client{Timeout: 10 * time.Second}}
}

func (n *Notifier) Send(e store.Event) {
	slog.Info("event", "kind", e.Kind, "severity", e.Severity, "site", e.Site, "check", e.Check, "msg", e.Message)
	if n.Webhook == "" {
		return
	}
	go func() {
		text := fmt.Sprintf("[%s] %s %s/%s: %s (from %s)", e.Severity, e.Kind, e.Site, e.Check, e.Message, e.Node)
		b, _ := json.Marshal(map[string]string{"text": text})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, n.Webhook, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := n.client.Do(req); err != nil {
			slog.Warn("webhook failed", "err", err)
		} else {
			resp.Body.Close()
		}
	}()
}

// VerifyFunc asks the peer server to run the same check (cross-region verification).
// ok=false means the peer could not be asked.
type VerifyFunc func(site, check string) (checks.Result, bool)

type checkState struct {
	fails    int
	level    string // OK | WARN | CRIT | LOCAL
	lastSeen time.Time
}

// stateTTL: local state older than this is re-read from the incident store
// (e.g. this server monitored the region before, handed it back, and has
// now taken it over again).
const stateTTL = 10 * time.Minute

// Evaluator applies flap control: CRIT only after N consecutive failures,
// and only state *changes* produce events.
type Evaluator struct {
	node            string
	failuresToAlert int
	emit            func(store.Event)
	verify          VerifyFunc
	incidentLevel   func(key string) string // current level of the open incident, "OK" if none

	mu    sync.Mutex
	state map[string]*checkState
}

func NewEvaluator(node string, failuresToAlert int, emit func(store.Event), verify VerifyFunc, incidentLevel func(string) string) *Evaluator {
	return &Evaluator{node: node, failuresToAlert: failuresToAlert, emit: emit, verify: verify,
		incidentLevel: incidentLevel, state: map[string]*checkState{}}
}

func (e *Evaluator) Process(r checks.Result) {
	e.mu.Lock()
	k := SiteKey(r.Site, r.Check)
	st := e.state[k]
	if st == nil || time.Since(st.lastSeen) > stateTTL {
		// Continue an incident that is already open (raised before a restart,
		// or by the peer before a takeover) instead of raising it again.
		lvl := checks.StatusOK
		if e.incidentLevel != nil {
			lvl = e.incidentLevel(k)
		}
		st = &checkState{level: lvl}
		if lvl == checks.StatusCrit {
			st.fails = e.failuresToAlert
		}
		e.state[k] = st
	}
	st.lastSeen = time.Now()
	level := r.Status
	if r.Status == checks.StatusCrit {
		st.fails++
		if st.fails < e.failuresToAlert {
			level = st.level // not confirmed yet; keep previous level
		}
	} else {
		st.fails = 0
	}
	prev := st.level
	e.mu.Unlock()

	if level == prev {
		return
	}

	// Before paging for "site down", ask the other region whether it sees the same.
	msg := r.Message
	if level == checks.StatusCrit && r.Check == config.CheckAvailability && e.verify != nil {
		if pr, ok := e.verify(r.Site, r.Check); ok {
			if pr.Status != checks.StatusCrit {
				level = "LOCAL"
				msg = fmt.Sprintf("unreachable from %s but UP from %s (%s): suspect local network issue", r.NodeRegion, pr.NodeRegion, pr.Message)
			} else {
				msg += fmt.Sprintf(" (confirmed DOWN from %s too)", pr.NodeRegion)
			}
		}
	}

	e.mu.Lock()
	if st.level == level { // another goroutine got here first
		e.mu.Unlock()
		return
	}
	st.level = level
	e.mu.Unlock()

	var ev store.Event
	switch level {
	case checks.StatusOK:
		ev = NewEvent(e.node, "RECOVERY", checks.StatusOK, r.Site, r.Check, "recovered: "+r.Message)
	case "LOCAL":
		ev = NewEvent(e.node, "ALERT", checks.StatusWarn, r.Site, r.Check, msg)
	default:
		ev = NewEvent(e.node, "ALERT", level, r.Site, r.Check, msg)
	}
	ev.Key = k
	e.emit(ev)
}
