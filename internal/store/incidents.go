package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Incident groups every event about one problem (one Key). The first event
// and its time are kept; later duplicates only update LastSeen/Occurrences.
// Clearing (Severity OK) and re-appearing are recorded as transitions.
type Incident struct {
	Key         string       `json:"key"`
	Kind        string       `json:"kind"`
	Site        string       `json:"site,omitempty"`
	Check       string       `json:"check,omitempty"`
	Status      string       `json:"status"`   // OPEN | CLEARED
	Severity    string       `json:"severity"` // current (or last, when cleared) severity
	Message     string       `json:"message"`  // latest message
	FirstSeen   time.Time    `json:"first_seen"`
	LastSeen    time.Time    `json:"last_seen"`
	ClearedAt   time.Time    `json:"cleared_at,omitzero"`
	Occurrences int          `json:"occurrences"` // times the problem was reported, duplicates included
	Cleared     int          `json:"cleared"`     // times it cleared
	Reappeared  int          `json:"reappeared"`  // times it came back after clearing
	SeenBy      []string     `json:"seen_by"`     // servers that reported it
	History     []Transition `json:"history"`
}

// Transition is one change in an incident's life.
type Transition struct {
	Time     time.Time `json:"time"`
	State    string    `json:"state"` // RAISED | SEVERITY | CLEARED | REAPPEARED
	Severity string    `json:"severity"`
	Node     string    `json:"node"`
	Message  string    `json:"message"`
}

// Incident states.
const (
	IncidentOpen    = "OPEN"
	IncidentCleared = "CLEARED"
)

const (
	maxIncidents       = 2000
	maxIncidentHistory = 50
)

func ts(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05Z") }

func (inc *Incident) addTransition(e Event, state string) {
	inc.History = append(inc.History, Transition{Time: e.Time, State: state, Severity: e.Severity, Node: e.Node, Message: e.Message})
	if len(inc.History) > maxIncidentHistory {
		// keep the very first transition (the original event) plus the most recent ones
		inc.History = append(inc.History[:1], inc.History[len(inc.History)-maxIncidentHistory+1:]...)
	}
}

func (inc *Incident) seenBy(node string) {
	if node != "" && !slices.Contains(inc.SeenBy, node) {
		inc.SeenBy = append(inc.SeenBy, node)
	}
}

// track folds a keyed event into its incident (inc is nil if none exists).
// It returns the incident to save (nil = nothing to save) and false when the
// event is a duplicate (same problem, still open, same severity; or a second
// "cleared" for an already cleared problem) that must not be stored or
// notified again. Otherwise it may annotate the event message with history.
func track(inc *Incident, e *Event) (*Incident, bool) {
	clear := e.Severity == "OK"

	switch {
	case inc == nil && clear:
		return nil, true // recovery for something we never saw open: keep, nothing to track

	case inc == nil:
		return &Incident{Key: e.Key, Kind: e.Kind, Site: e.Site, Check: e.Check, Status: IncidentOpen,
			Severity: e.Severity, Message: e.Message, FirstSeen: e.Time, LastSeen: e.Time, Occurrences: 1,
			SeenBy: []string{e.Node}, History: []Transition{{Time: e.Time, State: "RAISED", Severity: e.Severity, Node: e.Node, Message: e.Message}}}, true

	case inc.Status == IncidentOpen && !clear:
		inc.Occurrences++
		inc.seenBy(e.Node)
		if e.Time.Before(inc.FirstSeen) {
			inc.FirstSeen = e.Time // keep the oldest date, even if a late (replicated) report arrives
		}
		if e.Time.After(inc.LastSeen) {
			inc.LastSeen = e.Time
		}
		if e.Severity == inc.Severity {
			return inc, false // duplicate of an open problem: counters updated, no event
		}
		inc.Severity, inc.Message = e.Severity, e.Message
		inc.addTransition(*e, "SEVERITY")
		e.Message = fmt.Sprintf("%s (open since %s)", e.Message, ts(inc.FirstSeen))
		return inc, true

	case inc.Status == IncidentOpen && clear:
		inc.Status, inc.ClearedAt, inc.Message = IncidentCleared, e.Time, e.Message
		inc.Cleared++
		inc.LastSeen = e.Time
		inc.seenBy(e.Node)
		inc.addTransition(*e, "CLEARED")
		e.Message = fmt.Sprintf("%s (first seen %s, open for %s)", e.Message, ts(inc.FirstSeen), e.Time.Sub(inc.FirstSeen).Round(time.Second))
		return inc, true

	case inc.Status == IncidentCleared && !clear:
		clearedAt := inc.ClearedAt
		inc.Status, inc.Severity, inc.Message = IncidentOpen, e.Severity, e.Message
		inc.Reappeared++
		inc.Occurrences++
		inc.LastSeen = e.Time
		inc.seenBy(e.Node)
		inc.addTransition(*e, "REAPPEARED")
		e.Message = fmt.Sprintf("REAPPEARED: %s (first seen %s, cleared %s, reappearance #%d)", e.Message, ts(inc.FirstSeen), ts(clearedAt), inc.Reappeared)
		return inc, true

	default: // cleared and cleared again
		return nil, false
	}
}

// ---------- database ----------

func loadIncident(q interface {
	QueryRow(string, ...any) *sql.Row
}, key string) (*Incident, error) {
	var data string
	err := q.QueryRow(`SELECT data FROM incidents WHERE key = ?`, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var inc Incident
	return &inc, json.Unmarshal([]byte(data), &inc)
}

func saveIncident(tx *sql.Tx, inc *Incident) error {
	data, err := json.Marshal(inc)
	if err != nil {
		return err
	}
	var cleared sql.NullInt64
	if !inc.ClearedAt.IsZero() {
		cleared = sql.NullInt64{Int64: ms(inc.ClearedAt), Valid: true}
	}
	_, err = tx.Exec(`INSERT INTO incidents (key, status, severity, first_seen_ms, cleared_at_ms, data) VALUES (?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET status = excluded.status, severity = excluded.severity,
			first_seen_ms = excluded.first_seen_ms, cleared_at_ms = excluded.cleared_at_ms, data = excluded.data`,
		inc.Key, inc.Status, inc.Severity, ms(inc.FirstSeen), cleared, string(data))
	return err
}

// AddEvent stores an event. It returns false for an event already stored
// (replication) or a duplicate of an open incident; the caller then must not
// notify or replicate it.
func (s *Store) AddEvent(e Event) bool {
	stored := false
	err := s.write(func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM events WHERE id = ?`, e.ID).Scan(&one); err == nil {
			return nil // already stored
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if e.Key != "" {
			inc, err := loadIncident(tx, e.Key)
			if err != nil {
				return err
			}
			next, keep := track(inc, &e)
			if next != nil {
				if err := saveIncident(tx, next); err != nil {
					return err
				}
			}
			if !keep {
				return nil
			}
		}
		_, err := tx.Exec(`INSERT INTO events (id, time_ms, node, kind, severity, site, check_name, key, message)
			VALUES (?,?,?,?,?,?,?,?,?)`, e.ID, ms(e.Time), e.Node, e.Kind, e.Severity, e.Site, e.Check, e.Key, e.Message)
		stored = err == nil
		return err
	})
	if err != nil {
		logErr("store event", err)
		return false
	}
	return stored
}

// Events returns the newest n events, newest first.
func (s *Store) Events(n int) []Event {
	rows, err := s.db.Query(`SELECT id, time_ms, node, kind, severity, site, check_name, key, message
		FROM events ORDER BY time_ms DESC LIMIT ?`, n)
	if err != nil {
		logErr("events", err)
		return nil
	}
	defer rows.Close()
	out := make([]Event, 0, n)
	for rows.Next() {
		var e Event
		var t int64
		var node, kind, sev, site, chk, key, msg sql.NullString
		if err := rows.Scan(&e.ID, &t, &node, &kind, &sev, &site, &chk, &key, &msg); err != nil {
			continue
		}
		e.Time, e.Node, e.Kind, e.Severity, e.Site, e.Check, e.Key, e.Message =
			fromMs(t), node.String, kind.String, sev.String, site.String, chk.String, key.String, msg.String
		out = append(out, e)
	}
	return out
}

// Incidents returns open incidents (oldest first) followed by cleared ones
// (most recently cleared first), at most n in total.
func (s *Store) Incidents(n int) []Incident {
	rows, err := s.db.Query(`SELECT data FROM incidents
		ORDER BY status = 'OPEN' DESC,
		         CASE WHEN status = 'OPEN' THEN first_seen_ms END ASC,
		         cleared_at_ms DESC
		LIMIT ?`, n)
	if err != nil {
		logErr("incidents", err)
		return nil
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		var data string
		var inc Incident
		if rows.Scan(&data) == nil && json.Unmarshal([]byte(data), &inc) == nil {
			out = append(out, inc)
		}
	}
	return out
}

// IncidentLevel returns the severity of the open incident for key, or "OK".
// The evaluator uses it to continue an incident after a restart or takeover
// instead of raising it again.
func (s *Store) IncidentLevel(key string) string {
	var sev string
	err := s.db.QueryRow(`SELECT severity FROM incidents WHERE key = ? AND status = 'OPEN'`, key).Scan(&sev)
	if err != nil {
		return "OK"
	}
	return sev
}

// OpenIncidentCounts returns the number of open incidents per severity.
func (s *Store) OpenIncidentCounts() map[string]int64 {
	out := map[string]int64{}
	rows, err := s.db.Query(`SELECT severity, COUNT(*) FROM incidents WHERE status = 'OPEN' GROUP BY severity`)
	if err != nil {
		logErr("incident counts", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var sev string
		var n int64
		if rows.Scan(&sev, &n) == nil {
			out[sev] = n
		}
	}
	return out
}
