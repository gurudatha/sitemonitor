package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// kindOf names the interpreter family of a program from its extension.
func kindOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".py":
		return "python"
	case ".pl":
		return "perl"
	case ".ps1":
		return "powershell"
	case ".sh", ".bash":
		return "shell"
	case ".bat", ".cmd":
		return "windows-batch"
	}
	return "executable"
}

// binPath is the file of a binary inside the binaries directory. The name is
// validated (no path separators, no "..") before it reaches here.
func (s *Service) binPath(name string) (string, error) {
	if !BinaryRe.MatchString(name) || strings.Contains(name, "..") {
		return "", invalid("invalid binary name %q", name)
	}
	return filepath.Join(s.cfg.BinariesDir, name), nil
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeBinary stores content atomically (temp file + rename, mode 0750) and
// returns its sha256 and size. maxBytes bounds the upload.
func (s *Service) writeBinary(name string, r io.Reader, maxBytes int64) (string, int64, error) {
	path, err := s.binPath(name)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(s.cfg.BinariesDir, 0o750); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(s.cfg.BinariesDir, ".upload-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxBytes+1))
	if err != nil {
		tmp.Close()
		return "", 0, err
	}
	if n > maxBytes {
		tmp.Close()
		return "", 0, invalid("binary exceeds jobs.max_binary_size_mb (%d MB)", maxBytes>>20)
	}
	if n == 0 {
		tmp.Close()
		return "", 0, invalid("binary is empty")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", 0, err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o750); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", 0, fmt.Errorf("store binary: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
