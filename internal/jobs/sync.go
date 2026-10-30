package jobs

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"sitemonitor/internal/store"
)

// SyncState is everything the two servers keep identical: job definitions,
// binary metadata (contents are fetched separately) and API tokens.
// Records carry a version; the newer one wins. Deletions are tombstones.
type SyncState struct {
	Jobs     []Job            `json:"jobs"`
	Binaries []BinaryMeta     `json:"binaries"`
	Tokens   []store.APIToken `json:"tokens"`
}

// State returns the local state, including tombstones and token hashes.
func (s *Service) State() (SyncState, error) {
	jobs, err := s.repo.list(true)
	if err != nil {
		return SyncState{}, err
	}
	bins, err := s.repo.binaries(true)
	if err != nil {
		return SyncState{}, err
	}
	return SyncState{Jobs: jobs, Binaries: bins, Tokens: s.st.Tokens(true, true)}, nil
}

// Hash summarises the state; it is exchanged in every heartbeat.
func (s *Service) Hash() string {
	st, err := s.State()
	if err != nil {
		return ""
	}
	var keys []string
	for _, j := range st.Jobs {
		keys = append(keys, fmt.Sprintf("j|%s|%d", j.ID, j.Version))
	}
	for _, b := range st.Binaries {
		keys = append(keys, fmt.Sprintf("b|%s|%d", b.Name, b.Version))
	}
	for _, t := range st.Tokens {
		keys = append(keys, fmt.Sprintf("t|%s|%d", t.ID, t.Version))
	}
	sort.Strings(keys)
	h := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(h[:])
}

// Merge applies the peer's state. fetch downloads a binary's content from the
// peer; it is checked against the expected sha256 before it is stored.
func (s *Service) Merge(remote SyncState, fetch func(name string) ([]byte, error)) (changed int, err error) {
	local, err := s.State()
	if err != nil {
		return 0, err
	}
	have := map[string]BinaryMeta{}
	for _, b := range local.Binaries {
		have[b.Name] = b
	}
	for _, b := range remote.Binaries {
		if !BinaryRe.MatchString(b.Name) {
			continue
		}
		if l, ok := have[b.Name]; ok && l.Version >= b.Version {
			continue
		}
		path, _ := s.binPath(b.Name)
		if b.Deleted {
			os.Remove(path)
		} else {
			if cur, err := fileSHA(path); err != nil || cur != b.SHA256 {
				content, err := fetch(b.Name)
				if err != nil {
					return changed, fmt.Errorf("fetch binary %s: %w", b.Name, err)
				}
				sum := sha256.Sum256(content)
				if hex.EncodeToString(sum[:]) != b.SHA256 {
					return changed, fmt.Errorf("binary %s from peer does not match its sha256", b.Name)
				}
				if _, _, err := s.writeBinary(b.Name, bytes.NewReader(content), int64(len(content))); err != nil {
					return changed, err
				}
			}
		}
		b := b
		if err := s.st.Write(func(tx *sql.Tx) error { return putBinary(tx, &b) }); err != nil {
			return changed, err
		}
		changed++
	}

	versions := map[string]int64{}
	for _, j := range local.Jobs {
		versions[j.ID] = j.Version
	}
	err = s.st.Write(func(tx *sql.Tx) error {
		for i := range remote.Jobs {
			j := &remote.Jobs[i]
			if j.ID == "" || j.Version <= versions[j.ID] {
				continue
			}
			if err := putJob(tx, j); err != nil {
				return err
			}
			changed++
		}
		return nil
	})
	if err != nil {
		return changed, err
	}

	tv := map[string]int64{}
	for _, t := range local.Tokens {
		tv[t.ID] = t.Version
	}
	var newer []store.APIToken
	for _, t := range remote.Tokens {
		if t.Version > tv[t.ID] {
			newer = append(newer, t)
		}
	}
	if len(newer) > 0 {
		if err := s.st.MergeTokens(newer); err != nil {
			return changed, err
		}
		changed += len(newer)
	}
	if changed > 0 {
		s.Reconcile() // new / changed / deleted jobs from the peer
	}
	return changed, nil
}

// BinaryContent returns a stored binary's bytes (peer sync).
func (s *Service) BinaryContent(name string) ([]byte, error) {
	path, err := s.binPath(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
