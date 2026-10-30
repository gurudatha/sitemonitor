package server

import (
	"net/http"
	"strconv"
	"time"
)

// activity serves the dashboard charts from the stored hourly counters:
//
//	?days=30&tz=330          one bucket per local day, the last N days (max 400)
//	?day=2026-10-30&tz=330   one bucket per local hour of that day
//
// tz is the viewer's offset from UTC in minutes (east positive).
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tz, _ := strconv.Atoi(q.Get("tz"))
	if tz < -14*60 || tz > 14*60 {
		tz = 0
	}
	loc := time.FixedZone("viewer", tz*60)
	if d := q.Get("day"); d != "" {
		day, err := time.ParseInLocation("2006-01-02", d, loc)
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "day must be YYYY-MM-DD"})
			return
		}
		writeJSON(w, map[string]any{"unit": "hour", "day": d, "buckets": s.Store.Activity(day, time.Hour, 24)})
		return
	}
	days, _ := strconv.Atoi(q.Get("days"))
	if days <= 0 || days > 400 {
		days = 30
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start := today.AddDate(0, 0, -(days - 1))
	writeJSON(w, map[string]any{"unit": "day", "days": days, "buckets": s.Store.Activity(start, 24*time.Hour, days)})
}
