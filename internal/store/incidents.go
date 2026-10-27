package store

import (
	"fmt"
	"slices"
	"sort"
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

// trackLocked folds a keyed event into its incident. It returns false when
// the event is a duplicate (same problem, still open, same severity; or a
// second "cleared" for an already cleared problem) and must not be stored or
// notified again. Otherwise it may annotate the message with the history.
func (s *Store) trackLocked(e *Event) bool {
	clear := e.Severity == "OK"
	inc := s.incidents[e.Key]

	switch {
	case inc == nil && clear:
		return true // recovery for something we never saw open: keep, nothing to track

	case inc == nil:
		s.incidents[e.Key] = &Incident{Key: e.Key, Kind: e.Kind, Site: e.Site, Check: e.Check, Status: IncidentOpen,
			Severity: e.Severity, Message: e.Message, FirstSeen: e.Time, LastSeen: e.Time, Occurrences: 1,
			SeenBy: []string{e.Node}, History: []Transition{{Time: e.Time, State: "RAISED", Severity: e.Severity, Node: e.Node, Message: e.Message}}}
		s.evictIncidentsLocked()
		return true

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
			return false // duplicate of an open problem
		}
		inc.Severity, inc.Message = e.Severity, e.Message
		inc.addTransition(*e, "SEVERITY")
		e.Message = fmt.Sprintf("%s (open since %s)", e.Message, ts(inc.FirstSeen))
		return true

	case inc.Status == IncidentOpen && clear:
		inc.Status, inc.ClearedAt, inc.Message = IncidentCleared, e.Time, e.Message
		inc.Cleared++
		inc.LastSeen = e.Time
		inc.seenBy(e.Node)
		inc.addTransition(*e, "CLEARED")
		e.Message = fmt.Sprintf("%s (first seen %s, open for %s)", e.Message, ts(inc.FirstSeen), e.Time.Sub(inc.FirstSeen).Round(time.Second))
		return true

	case inc.Status == IncidentCleared && !clear:
		clearedAt := inc.ClearedAt
		inc.Status, inc.Severity, inc.Message = IncidentOpen, e.Severity, e.Message
		inc.Reappeared++
		inc.Occurrences++
		inc.LastSeen = e.Time
		inc.seenBy(e.Node)
		inc.addTransition(*e, "REAPPEARED")
		e.Message = fmt.Sprintf("REAPPEARED: %s (first seen %s, cleared %s, reappearance #%d)", e.Message, ts(inc.FirstSeen), ts(clearedAt), inc.Reappeared)
		return true

	default: // cleared and cleared again
		return false
	}
}

// evictIncidentsLocked bounds memory: drop the oldest cleared incidents first.
func (s *Store) evictIncidentsLocked() {
	if len(s.incidents) <= maxIncidents {
		return
	}
	var cleared []*Incident
	for _, inc := range s.incidents {
		if inc.Status == IncidentCleared {
			cleared = append(cleared, inc)
		}
	}
	sort.Slice(cleared, func(i, j int) bool { return cleared[i].ClearedAt.Before(cleared[j].ClearedAt) })
	for _, inc := range cleared {
		if len(s.incidents) <= maxIncidents {
			return
		}
		delete(s.incidents, inc.Key)
	}
}

// Incidents returns open incidents (oldest first) followed by cleared ones
// (most recently cleared first), at most n in total.
func (s *Store) Incidents(n int) []Incident {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Incident, 0, len(s.incidents))
	for _, inc := range s.incidents {
		c := *inc
		c.History = append([]Transition(nil), inc.History...)
		c.SeenBy = append([]string(nil), inc.SeenBy...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Status != b.Status {
			return a.Status == IncidentOpen
		}
		if a.Status == IncidentOpen {
			return a.FirstSeen.Before(b.FirstSeen)
		}
		return a.ClearedAt.After(b.ClearedAt)
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// IncidentLevel returns the severity of the open incident for key, or "OK".
// The evaluator uses it to continue an incident after a restart or takeover
// instead of raising it again.
func (s *Store) IncidentLevel(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if inc := s.incidents[key]; inc != nil && inc.Status == IncidentOpen {
		return inc.Severity
	}
	return "OK"
}
