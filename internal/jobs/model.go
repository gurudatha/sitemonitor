// Package jobs lets users and admins run their own programs periodically:
// a binary from the shared binaries directory (Python, Perl, PowerShell,
// shell or any executable), with arguments, on a fixed interval or once when
// the server comes up, either locally or on a remote Linux / Windows (Cygwin)
// server over SSH.
//
// Everything is one abstraction, Job:
//
//	scope   user  -> added by an operator; runs immediately, for a limited
//	                 window (max jobs.user_max_run_for, 24h)
//	        admin -> the admin's own periodic activities; no window limit
//	merge   a user asks for their job to become an admin activity; only this
//	        needs admin approval (merge_request: pending -> approved | rejected).
//	        While the request is pending the job keeps running as a user job.
//	status  approved <-> disabled  (pending / rejected only when the optional
//	        jobs.require_approval is on)
//	target  nil = run on the monitor server; otherwise host/port/os/credential
//
// Each run is stored as a check result (site "job:<name>", check "job"), so
// runs get counters, uptime, incidents, metrics and replication for free.
package jobs

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Scopes and statuses.
const (
	ScopeUser  = "user"
	ScopeAdmin = "admin"

	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusDisabled = "disabled"

	TypeInterval = "interval" // every N minutes/hours
	TypeStartup  = "startup"  // once, when the server comes up

	OSLinux         = "linux"
	OSWindowsCygwin = "windows-cygwin"
)

// Intervals users can choose from.
var Intervals = []string{"1m", "2m", "5m", "10m", "15m", "20m", "30m", "60m", "1h", "2h", "4h", "6h"}

// RunForChoices are the windows offered in the UI for user jobs (max 24h).
var RunForChoices = []string{"1h", "2h", "4h", "8h", "12h", "24h"}

// Schedule says when a job runs.
type Schedule struct {
	Type    string    `json:"type"`              // interval | startup
	Every   string    `json:"every,omitempty"`   // one of Intervals (type interval)
	RunFor  string    `json:"run_for,omitempty"` // window length; required for user jobs, max 24h
	StartAt time.Time `json:"start_at,omitzero"` // window start; default = approval time
	EndsAt  time.Time `json:"ends_at,omitzero"`  // set on approval: StartAt + RunFor
}

// Target is a remote server reached over SSH. Credentials are referenced by
// name (node.yaml jobs.remote.credentials) and never pass through the API.
type Target struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	OS         string `json:"os"` // linux | windows-cygwin
	Credential string `json:"credential"`
}

// MergeRequest asks an admin to turn a user job into an admin periodic activity.
type MergeRequest struct {
	Status      string    `json:"status"` // pending | approved | rejected
	RequestedBy string    `json:"requested_by"`
	RequestedAt time.Time `json:"requested_at"`
	Note        string    `json:"note,omitempty"`
	DecidedBy   string    `json:"decided_by,omitempty"`
	DecidedAt   time.Time `json:"decided_at,omitzero"`
	Reason      string    `json:"reason,omitempty"`
}

// Job is one periodic (or startup) activity.
type Job struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"` // immutable; results are stored as site "job:<name>"
	Description  string        `json:"description,omitempty"`
	Scope        string        `json:"scope"`
	Status       string        `json:"status"`
	Owner        string        `json:"owner"`  // who requested / created it
	Region       string        `json:"region"` // runs on the server that currently owns this region (failover aware)
	Binary       string        `json:"binary"` // file name in the binaries directory
	Args         []string      `json:"args"`
	Schedule     Schedule      `json:"schedule"`
	Timeout      string        `json:"timeout,omitempty"`
	Target       *Target       `json:"target,omitempty"`
	ApprovedBy   string        `json:"approved_by,omitempty"`
	ApprovedAt   time.Time     `json:"approved_at,omitzero"`
	BinarySHA    string        `json:"binary_sha256,omitempty"` // pinned at approval; a changed binary does not run
	MergedBy     string        `json:"merged_by,omitempty"`     // admin who merged a user job into admin activities
	MergedAt     time.Time     `json:"merged_at,omitzero"`
	MergeRequest *MergeRequest `json:"merge_request,omitempty"`
	Reason       string        `json:"reason,omitempty"` // rejection / disable reason
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
	UpdatedBy    string        `json:"updated_by"`
	Version      int64         `json:"version"` // last-writer-wins between the two servers
	Deleted      bool          `json:"deleted,omitempty"`
}

// State is the effective state shown to users (derived from status + window).
func (j *Job) State(now time.Time) string {
	switch {
	case j.Deleted:
		return "deleted"
	case j.Status != StatusApproved:
		return j.Status
	case !j.Schedule.StartAt.IsZero() && now.Before(j.Schedule.StartAt):
		return "scheduled"
	case !j.Schedule.EndsAt.IsZero() && !now.Before(j.Schedule.EndsAt):
		return "expired"
	}
	return "active"
}

// Active reports whether the job may run at now.
func (j *Job) Active(now time.Time) bool { return j.State(now) == "active" }

// Site is the results key for this job's runs.
func (j *Job) Site() string { return "job:" + j.Name }

// Interval is the parsed Every.
func (j *Job) Interval() time.Duration {
	d, _ := time.ParseDuration(j.Schedule.Every)
	return d
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	BinaryRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})$`)
)

// Input is what clients send to create or update a job.
type Input struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Region      string   `json:"region"`
	Binary      string   `json:"binary"`
	Args        []string `json:"args"`
	Schedule    Schedule `json:"schedule"`
	Timeout     string   `json:"timeout"`
	Target      *Target  `json:"target"`
	Scope       string   `json:"scope"` // admins may create "admin" jobs directly; ignored for others
}

// Limits used by validation (from node.yaml jobs).
type Limits struct {
	UserMaxRunFor  time.Duration
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	Credentials    []string // names configured in node.yaml
}

// ValidationError is returned for bad input (HTTP 422).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{fmt.Sprintf(format, a...)} }

// IsValidation reports whether err is a ValidationError.
func IsValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

// validate checks a job definition. scope decides the window rules.
func validate(in *Input, scope string, lim Limits) error {
	if !nameRe.MatchString(in.Name) {
		return invalid("name must be 1-63 characters: lower-case letters, digits, '.', '_' or '-'")
	}
	if len(in.Description) > 500 {
		return invalid("description is longer than 500 characters")
	}
	if in.Region != "IDC" && in.Region != "FC" {
		return invalid("region must be IDC or FC")
	}
	if !BinaryRe.MatchString(in.Binary) {
		return invalid("binary must be the name of an uploaded file")
	}
	if len(in.Args) > 32 {
		return invalid("at most 32 arguments")
	}
	for _, a := range in.Args {
		if len(a) > 1024 || strings.ContainsRune(a, 0) {
			return invalid("arguments must be at most 1024 characters, without NUL bytes")
		}
	}

	sc := &in.Schedule
	switch sc.Type {
	case TypeInterval:
		if !slices.Contains(Intervals, sc.Every) {
			return invalid("schedule.every must be one of %s", strings.Join(Intervals, ", "))
		}
	case TypeStartup:
		sc.Every = ""
	default:
		return invalid("schedule.type must be %q or %q", TypeInterval, TypeStartup)
	}
	if sc.RunFor != "" {
		d, err := time.ParseDuration(sc.RunFor)
		if err != nil || d <= 0 {
			return invalid("schedule.run_for must be a duration such as 8h")
		}
		if scope == ScopeUser && d > lim.UserMaxRunFor {
			return invalid("user jobs can run for at most %s", lim.UserMaxRunFor)
		}
	} else if scope == ScopeUser {
		sc.RunFor = lim.UserMaxRunFor.String() // default: the maximum window
	}
	sc.EndsAt = time.Time{} // set on approval

	timeout := lim.DefaultTimeout
	if in.Timeout != "" {
		d, err := time.ParseDuration(in.Timeout)
		if err != nil || d <= 0 {
			return invalid("timeout must be a duration such as 2m")
		}
		timeout = d
	}
	if timeout > lim.MaxTimeout {
		return invalid("timeout may be at most %s", lim.MaxTimeout)
	}
	if sc.Type == TypeInterval {
		if every, _ := time.ParseDuration(sc.Every); timeout > every {
			timeout = every // a run never overlaps the next one
		}
	}
	in.Timeout = timeout.String()

	if t := in.Target; t != nil {
		if net.ParseIP(t.Host) == nil && !hostRe.MatchString(t.Host) {
			return invalid("target.host must be an IP address or host name")
		}
		if t.Port == 0 {
			t.Port = 22
		}
		if t.Port < 1 || t.Port > 65535 {
			return invalid("target.port must be 1-65535")
		}
		if t.OS != OSLinux && t.OS != OSWindowsCygwin {
			return invalid("target.os must be %q or %q", OSLinux, OSWindowsCygwin)
		}
		if !slices.Contains(lim.Credentials, t.Credential) {
			return invalid("target.credential must be one of the credentials configured by the admin: %s", strings.Join(lim.Credentials, ", "))
		}
	}
	return nil
}
