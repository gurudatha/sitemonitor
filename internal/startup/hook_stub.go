//go:build !sitemonitor_cgo

package startup

// Run is the no-op used by the default (pure Go, static) build.
func Run(Info) (int, error) { return 0, ErrNotCompiled }

// Compiled reports whether the native hook is part of this binary.
func Compiled() bool { return false }
