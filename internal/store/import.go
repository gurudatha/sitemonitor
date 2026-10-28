package store

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"sitemonitor/internal/checks"
)

// legacySnapshot is the results.json format used before the database.
type legacySnapshot struct {
	History   map[string][]checks.Result `json:"history"`
	Events    []Event                    `json:"events"`
	Incidents map[string]*Incident       `json:"incidents"`
}

// importLegacy loads <dataDir>/results.json once (results through the normal
// path so counters are built, events and incidents as they were), then
// renames it to results.json.imported.
func (s *Store) importLegacy(dataDir string) error {
	path := filepath.Join(dataDir, "results.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var snap legacySnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		slog.Warn("results.json is not readable; skipping import", "err", err)
		return os.Rename(path, path+".unreadable")
	}
	var results []checks.Result
	for _, h := range snap.History {
		results = append(results, h...)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Time.Before(results[j].Time) })
	for _, r := range results {
		s.AddResult(r)
	}
	err = s.write(func(tx *sql.Tx) error {
		for _, inc := range snap.Incidents {
			if err := saveIncident(tx, inc); err != nil {
				return err
			}
		}
		for _, e := range snap.Events {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO events (id, time_ms, node, kind, severity, site, check_name, key, message)
				VALUES (?,?,?,?,?,?,?,?,?)`, e.ID, ms(e.Time), e.Node, e.Kind, e.Severity, e.Site, e.Check, e.Key, e.Message); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	slog.Info("imported results.json into the database", "results", len(results), "events", len(snap.Events), "incidents", len(snap.Incidents))
	return os.Rename(path, path+".imported")
}
