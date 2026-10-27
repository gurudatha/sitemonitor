package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

var seq int

func ev(node, sev string, at time.Duration) Event {
	seq++
	kind := "ALERT"
	if sev == "OK" {
		kind = "RECOVERY"
	}
	return Event{ID: fmt.Sprintf("%s-%d", node, seq), Time: t0.Add(at), Node: node, Kind: kind, Severity: sev,
		Site: "shop", Check: "availability", Key: "site:shop|availability", Message: "DOWN: HTTP 503"}
}

func TestIncidentLifecycle(t *testing.T) {
	s := New(t.TempDir(), 10, 100)

	if !s.AddEvent(ev("idc", "CRIT", 0)) {
		t.Fatal("first alert must be stored")
	}
	// same problem again (restart / other server / takeover): not stored, oldest date kept
	if s.AddEvent(ev("idc", "CRIT", 5*time.Minute)) || s.AddEvent(ev("fc", "CRIT", 6*time.Minute)) {
		t.Fatal("duplicate of an open incident must be suppressed")
	}
	// a late replicated report that is even older moves first_seen back
	if s.AddEvent(ev("fc", "CRIT", -2*time.Minute)) {
		t.Fatal("older duplicate must be suppressed too")
	}
	inc := s.Incidents(10)[0]
	if inc.Status != IncidentOpen || !inc.FirstSeen.Equal(t0.Add(-2*time.Minute)) || inc.Occurrences != 4 || len(inc.SeenBy) != 2 {
		t.Fatalf("open incident wrong: %+v", inc)
	}
	if s.IncidentLevel("site:shop|availability") != "CRIT" {
		t.Fatal("IncidentLevel must report the open severity")
	}

	// clears: stored and annotated with the original date
	rec := ev("idc", "OK", 10*time.Minute)
	if !s.AddEvent(rec) {
		t.Fatal("clear must be stored")
	}
	if s.AddEvent(ev("fc", "OK", 11*time.Minute)) {
		t.Fatal("second clear of a cleared incident must be suppressed")
	}
	// reappears: stored, marked, keeps first_seen
	if !s.AddEvent(ev("idc", "CRIT", 20*time.Minute)) {
		t.Fatal("re-appearance must be stored")
	}
	events := s.Events(10)
	if !strings.HasPrefix(events[0].Message, "REAPPEARED:") || !strings.Contains(events[0].Message, "first seen 2026-10-27 08:58:00Z") {
		t.Fatalf("reappear message: %q", events[0].Message)
	}
	if !strings.Contains(events[1].Message, "first seen 2026-10-27 08:58:00Z, open for 12m0s") {
		t.Fatalf("clear message: %q", events[1].Message)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 stored events (raised, cleared, reappeared), got %d", len(events))
	}

	inc = s.Incidents(10)[0]
	states := []string{}
	for _, tr := range inc.History {
		states = append(states, tr.State)
	}
	if got := strings.Join(states, ","); got != "RAISED,CLEARED,REAPPEARED" || inc.Cleared != 1 || inc.Reappeared != 1 {
		t.Fatalf("history %s, cleared %d, reappeared %d", got, inc.Cleared, inc.Reappeared)
	}

	// severity change while open is recorded, not suppressed
	if !s.AddEvent(ev("idc", "WARN", 25*time.Minute)) || s.Incidents(10)[0].History[3].State != "SEVERITY" {
		t.Fatal("severity change must be stored as a transition")
	}

	// incidents survive a restart (snapshot)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2 := New(strings.TrimSuffix(s.path, "/results.json"), 10, 100)
	if got := s2.Incidents(10); len(got) != 1 || got[0].Reappeared != 1 || s2.IncidentLevel("site:shop|availability") != "WARN" {
		t.Fatalf("incident not restored: %+v", got)
	}
	if s2.AddEvent(ev("idc", "WARN", 30*time.Minute)) {
		t.Fatal("after restart, the still-open problem must not be raised again")
	}
}

func TestUnkeyedEventsUnchanged(t *testing.T) {
	s := New(t.TempDir(), 10, 100)
	e := Event{ID: "a", Time: t0, Kind: "INFO", Severity: "OK", Message: "started"}
	if !s.AddEvent(e) || s.AddEvent(e) {
		t.Fatal("unkeyed events: stored once, de-duplicated by ID only")
	}
	e.ID = "b"
	if !s.AddEvent(e) || len(s.Incidents(10)) != 0 {
		t.Fatal("unkeyed events are not grouped into incidents")
	}
}
