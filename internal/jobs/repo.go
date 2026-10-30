package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"sitemonitor/internal/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id      TEXT PRIMARY KEY,
	name    TEXT NOT NULL,
	version INTEGER NOT NULL,
	deleted INTEGER NOT NULL DEFAULT 0,
	data    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS job_binaries (
	name        TEXT PRIMARY KEY,
	sha256      TEXT NOT NULL,
	size        INTEGER NOT NULL,
	uploaded_by TEXT,
	uploaded_ms INTEGER NOT NULL,
	version     INTEGER NOT NULL,
	deleted     INTEGER NOT NULL DEFAULT 0
);`

// repo stores job definitions and binary metadata in the shared database.
type repo struct{ st *store.Store }

var errNotFound = errors.New("job not found")

func newRepo(st *store.Store) (*repo, error) {
	_, err := st.DB().Exec(schema)
	return &repo{st: st}, err
}

func (r *repo) get(id string) (*Job, error) {
	var data string
	err := r.st.DB().QueryRow(`SELECT data FROM jobs WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var j Job
	return &j, json.Unmarshal([]byte(data), &j)
}

// list returns jobs ordered by name; tombstones only if withDeleted.
func (r *repo) list(withDeleted bool) ([]Job, error) {
	q := `SELECT data FROM jobs`
	if !withDeleted {
		q += ` WHERE deleted = 0`
	}
	rows, err := r.st.DB().Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var data string
		var j Job
		if rows.Scan(&data) == nil && json.Unmarshal([]byte(data), &j) == nil {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// put writes a job if its version is newer than the stored one (LWW).
func putJob(tx *sql.Tx, j *Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO jobs (id, name, version, deleted, data) VALUES (?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, version = excluded.version,
			deleted = excluded.deleted, data = excluded.data
		WHERE excluded.version > jobs.version`, j.ID, j.Name, j.Version, j.Deleted, string(data))
	return err
}

func (r *repo) put(j *Job) error {
	return r.st.Write(func(tx *sql.Tx) error { return putJob(tx, j) })
}

// BinaryMeta describes a file in the binaries directory.
type BinaryMeta struct {
	Name       string `json:"name"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	UploadedBy string `json:"uploaded_by"`
	UploadedMs int64  `json:"uploaded_ms"`
	Version    int64  `json:"version"`
	Deleted    bool   `json:"deleted,omitempty"`
	Kind       string `json:"kind,omitempty"` // python, perl, powershell, shell, executable (derived)
}

func putBinary(tx *sql.Tx, b *BinaryMeta) error {
	_, err := tx.Exec(`INSERT INTO job_binaries (name, sha256, size, uploaded_by, uploaded_ms, version, deleted)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size,
			uploaded_by = excluded.uploaded_by, uploaded_ms = excluded.uploaded_ms,
			version = excluded.version, deleted = excluded.deleted
		WHERE excluded.version > job_binaries.version`,
		b.Name, b.SHA256, b.Size, b.UploadedBy, b.UploadedMs, b.Version, b.Deleted)
	return err
}

func (r *repo) binary(name string) (*BinaryMeta, error) {
	var b BinaryMeta
	var del int
	err := r.st.DB().QueryRow(`SELECT name, sha256, size, uploaded_by, uploaded_ms, version, deleted
		FROM job_binaries WHERE name = ?`, name).Scan(&b.Name, &b.SHA256, &b.Size, &b.UploadedBy, &b.UploadedMs, &b.Version, &del)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	b.Deleted = del == 1
	b.Kind = kindOf(b.Name)
	return &b, err
}

func (r *repo) binaries(withDeleted bool) ([]BinaryMeta, error) {
	q := `SELECT name, sha256, size, uploaded_by, uploaded_ms, version, deleted FROM job_binaries`
	if !withDeleted {
		q += ` WHERE deleted = 0`
	}
	rows, err := r.st.DB().Query(q + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BinaryMeta{}
	for rows.Next() {
		var b BinaryMeta
		var del int
		if rows.Scan(&b.Name, &b.SHA256, &b.Size, &b.UploadedBy, &b.UploadedMs, &b.Version, &del) == nil {
			b.Deleted = del == 1
			b.Kind = kindOf(b.Name)
			out = append(out, b)
		}
	}
	return out, nil
}
