// Package configstore reads and safely rewrites sites.yaml and node.yaml for
// the configuration API: validation, size limits, optimistic concurrency
// (If-Match on the file hash), timestamped backups and atomic replacement.
package configstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"sitemonitor/internal/config"
)

var (
	ErrConflict  = errors.New("file changed since you read it (If-Match does not match the current hash); re-read and retry")
	ErrTooLarge  = errors.New("configuration exceeds limits.max_config_size_kb")
	ErrNotFound  = errors.New("site not found")
	ErrImmutable = errors.New("node.id, node.region, peer.id, peer.region and tls.* cannot be changed through the API; edit the file on the server")
)

// Diff summarises a sites.yaml change for the audit event.
type Diff struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

func (d Diff) Empty() bool { return len(d.Added)+len(d.Removed)+len(d.Changed) == 0 }

func (d Diff) String() string {
	var parts []string
	if len(d.Added) > 0 {
		parts = append(parts, "added "+strings.Join(d.Added, ","))
	}
	if len(d.Removed) > 0 {
		parts = append(parts, "removed "+strings.Join(d.Removed, ","))
	}
	if len(d.Changed) > 0 {
		parts = append(parts, "changed "+strings.Join(d.Changed, ","))
	}
	if len(parts) == 0 {
		return "defaults/version only"
	}
	return strings.Join(parts, "; ")
}

type Store struct {
	nodePath, sitesPath string
	node                *config.Node // running config (for limits and immutable fields)
	apply               func(*config.Sites)

	mu sync.Mutex
}

func New(nodePath string, node *config.Node, apply func(*config.Sites)) *Store {
	return &Store{nodePath: nodePath, sitesPath: node.Monitor.SitesFile, node: node, apply: apply}
}

func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Store) maxSize() int { return int(s.node.Limits.MaxConfigSizeKB << 10) }

// ---------- sites.yaml ----------

func (s *Store) ReadSites() (content []byte, hash string, modTime time.Time, err error) {
	content, err = os.ReadFile(s.sitesPath)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	fi, err := os.Stat(s.sitesPath)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	return content, Hash(content), fi.ModTime().UTC(), nil
}

// ValidateSites parses content and applies this node's limits.
func (s *Store) ValidateSites(content []byte) (*config.Sites, error) {
	if len(content) > s.maxSize() {
		return nil, ErrTooLarge
	}
	sites, err := config.ParseSites(content)
	if err != nil {
		return nil, err
	}
	return sites, sites.CheckLimits(s.node.Limits)
}

// WriteSites validates and atomically replaces sites.yaml, then applies it.
// ifMatch (optional) must equal the current hash. modTime (optional) is set
// on the file so both servers agree on which copy is newer.
func (s *Store) WriteSites(content []byte, ifMatch string, modTime time.Time) (*config.Sites, Diff, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeSitesLocked(content, ifMatch, modTime)
}

func (s *Store) writeSitesLocked(content []byte, ifMatch string, modTime time.Time) (*config.Sites, Diff, bool, error) {
	cur, err := os.ReadFile(s.sitesPath)
	if err != nil {
		return nil, Diff{}, false, err
	}
	if ifMatch != "" && strings.Trim(ifMatch, `"`) != Hash(cur) {
		return nil, Diff{}, false, ErrConflict
	}
	next, err := s.ValidateSites(content)
	if err != nil {
		return nil, Diff{}, false, err
	}
	if bytes.Equal(cur, content) {
		if !modTime.IsZero() {
			_ = os.Chtimes(s.sitesPath, modTime, modTime)
		}
		return next, Diff{}, false, nil // nothing to do
	}
	prev, _ := config.ParseSites(cur)
	if err := s.replace(s.sitesPath, cur, content, modTime); err != nil {
		return nil, Diff{}, false, err
	}
	s.apply(next)
	return next, diff(prev, next), true, nil
}

func diff(prev, next *config.Sites) Diff {
	var d Diff
	old := map[string]config.Site{}
	if prev != nil {
		for _, st := range prev.Sites {
			old[st.Name] = st
		}
	}
	seen := map[string]bool{}
	for _, st := range next.Sites {
		seen[st.Name] = true
		if o, ok := old[st.Name]; !ok {
			d.Added = append(d.Added, st.Name)
		} else if !reflect.DeepEqual(o, st) {
			d.Changed = append(d.Changed, st.Name)
		}
	}
	for name := range old {
		if !seen[name] {
			d.Removed = append(d.Removed, name)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Changed)
	return d
}

// ---------- per-site edits (comments elsewhere in the file are preserved) ----------

func sitesSeq(doc *yaml.Node) (*yaml.Node, *yaml.Node, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("sites.yaml: top level must be a mapping")
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "sites" {
			return root, root.Content[i+1], nil
		}
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "sites"}, seq)
	return root, seq, nil
}

func siteName(item *yaml.Node) string {
	for i := 0; i+1 < len(item.Content); i += 2 {
		if item.Content[i].Value == "name" {
			return item.Content[i+1].Value
		}
	}
	return ""
}

func setVersion(root *yaml.Node, v string) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "version" {
			root.Content[i+1].Value = v
			root.Content[i+1].Style = yaml.DoubleQuotedStyle
			return
		}
	}
	root.Content = append([]*yaml.Node{{Kind: yaml.ScalarNode, Value: "version"},
		{Kind: yaml.ScalarNode, Value: v, Style: yaml.DoubleQuotedStyle}}, root.Content...)
}

// editSites applies fn to the parsed document and writes the result.
func (s *Store) editSites(ifMatch string, fn func(root, seq *yaml.Node) error) (*config.Sites, Diff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := os.ReadFile(s.sitesPath)
	if err != nil {
		return nil, Diff{}, err
	}
	if ifMatch != "" && strings.Trim(ifMatch, `"`) != Hash(cur) {
		return nil, Diff{}, ErrConflict
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(cur, &doc); err != nil {
		return nil, Diff{}, err
	}
	root, seq, err := sitesSeq(&doc)
	if err != nil {
		return nil, Diff{}, err
	}
	if err := fn(root, seq); err != nil {
		return nil, Diff{}, err
	}
	setVersion(root, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, Diff{}, err
	}
	next, d, _, err := s.writeSitesLocked(buf.Bytes(), "", time.Time{})
	return next, d, err
}

// ParseSite reads one site from YAML or JSON (JSON is valid YAML).
func ParseSite(body []byte) (config.Site, error) {
	var st config.Site
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&st); err != nil {
		return st, fmt.Errorf("site: %w", err)
	}
	return st, nil
}

// GetSite returns one site exactly as written in sites.yaml (no defaults filled in).
func (s *Store) GetSite(name string) ([]byte, string, error) {
	cur, err := os.ReadFile(s.sitesPath)
	if err != nil {
		return nil, "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(cur, &doc); err != nil {
		return nil, "", err
	}
	_, seq, err := sitesSeq(&doc)
	if err != nil {
		return nil, "", err
	}
	for _, item := range seq.Content {
		if siteName(item) == name {
			var buf bytes.Buffer
			enc := yaml.NewEncoder(&buf)
			enc.SetIndent(2)
			if err := enc.Encode(item); err != nil {
				return nil, "", err
			}
			return buf.Bytes(), Hash(cur), nil
		}
	}
	return nil, "", ErrNotFound
}

// UpsertSite adds or replaces the site called name.
func (s *Store) UpsertSite(name string, st config.Site, ifMatch string) (*config.Sites, Diff, error) {
	if st.Name == "" {
		st.Name = name
	}
	if st.Name != name {
		return nil, Diff{}, fmt.Errorf("site name in body (%q) does not match URL (%q)", st.Name, name)
	}
	var node yaml.Node
	if err := node.Encode(st); err != nil {
		return nil, Diff{}, err
	}
	return s.editSites(ifMatch, func(_, seq *yaml.Node) error {
		for i, item := range seq.Content {
			if siteName(item) == name {
				node.HeadComment = item.HeadComment
				seq.Content[i] = &node
				return nil
			}
		}
		seq.Content = append(seq.Content, &node)
		return nil
	})
}

func (s *Store) DeleteSite(name, ifMatch string) (*config.Sites, Diff, error) {
	return s.editSites(ifMatch, func(_, seq *yaml.Node) error {
		for i, item := range seq.Content {
			if siteName(item) == name {
				seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
}

// ---------- node.yaml ----------

func (s *Store) ReadNode() ([]byte, string, error) {
	b, err := os.ReadFile(s.nodePath)
	if err != nil {
		return nil, "", err
	}
	return b, Hash(b), nil
}

// ValidateNode parses node.yaml content and rejects changes to identity fields.
func (s *Store) ValidateNode(content []byte) (*config.Node, error) {
	if len(content) > s.maxSize() {
		return nil, ErrTooLarge
	}
	n, err := config.ParseNode(content)
	if err != nil {
		return nil, err
	}
	o := s.node
	if n.Node.ID != o.Node.ID || n.Node.Region != o.Node.Region || n.Peer.ID != o.Peer.ID ||
		n.Peer.Region != o.Peer.Region || n.TLS != o.TLS {
		return nil, ErrImmutable
	}
	return n, nil
}

// WriteNode validates and replaces node.yaml. Changes take effect on restart.
func (s *Store) WriteNode(content []byte, ifMatch string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := os.ReadFile(s.nodePath)
	if err != nil {
		return false, err
	}
	if ifMatch != "" && strings.Trim(ifMatch, `"`) != Hash(cur) {
		return false, ErrConflict
	}
	if _, err := s.ValidateNode(content); err != nil {
		return false, err
	}
	if bytes.Equal(cur, content) {
		return false, nil
	}
	return true, s.replace(s.nodePath, cur, content, time.Time{})
}

// ---------- file handling ----------

// replace backs up the current file, then writes the new one atomically.
func (s *Store) replace(path string, cur, content []byte, modTime time.Time) error {
	dir := filepath.Dir(path)
	if n := s.node.ConfigAPI.Backups; n > 0 {
		bdir := filepath.Join(dir, "backups")
		if err := os.MkdirAll(bdir, 0o750); err != nil {
			return fmt.Errorf("backup dir: %w", err)
		}
		base := filepath.Base(path)
		name := filepath.Join(bdir, base+"."+time.Now().UTC().Format("20060102T150405.000Z"))
		if err := os.WriteFile(name, cur, 0o640); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		old, _ := filepath.Glob(filepath.Join(bdir, base+".*"))
		sort.Strings(old)
		for len(old) > n {
			_ = os.Remove(old[0])
			old = old[1:]
		}
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("config directory must be writable for the config API: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if !modTime.IsZero() {
		_ = os.Chtimes(path, modTime, modTime)
	}
	return nil
}

// Backups lists saved versions of sites.yaml or node.yaml, newest first.
func (s *Store) Backups(which string) []string {
	path := s.sitesPath
	if which == "node" {
		path = s.nodePath
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "backups", filepath.Base(path)+".*"))
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, filepath.Base(f))
	}
	return out
}
