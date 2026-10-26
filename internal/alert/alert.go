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
	fails int
	level string // OK | WARN | CRIT | LOCAL
}

// Evaluator applies flap control: CRIT only after N consecutive failures,
// and only state *changes* produce events.
type Evaluator struct {
	node            string
	failuresToAlert int
	emit            func(store.Event)
	verify          VerifyFunc

	mu    sync.Mutex
	state map[string]*checkState
}

func NewEvaluator(node string, failuresToAlert int, emit func(store.Event), verify VerifyFunc) *Evaluator {
	return &Evaluator{node: node, failuresToAlert: failuresToAlert, emit: emit, verify: verify, state: map[string]*checkState{}}
}

func (e *Evaluator) Process(r checks.Result) {
	e.mu.Lock()
	k := r.Site + "|" + r.Check
	st := e.state[k]
	if st == nil {
		st = &checkState{level: checks.StatusOK}
		e.state[k] = st
	}
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

	switch level {
	case checks.StatusOK:
		e.emit(NewEvent(e.node, "RECOVERY", checks.StatusOK, r.Site, r.Check, "recovered: "+r.Message))
	case "LOCAL":
		e.emit(NewEvent(e.node, "ALERT", checks.StatusWarn, r.Site, r.Check, msg))
	default:
		e.emit(NewEvent(e.node, "ALERT", level, r.Site, r.Check, msg))
	}
}
