// Package startup calls an optional native (cgo) hook when the server comes up.
//
// The hook is a placeholder: the C program will be written later. It is only
// compiled in with the build tag "sitemonitor_cgo" (and CGO_ENABLED=1); the
// default static build uses the stub in hook_stub.go and reports that no hook
// is compiled in. See docs/JOBS.md, "Startup hook".
package startup

import "errors"

// ErrNotCompiled is returned when the binary was built without the cgo hook.
var ErrNotCompiled = errors.New("startup hook not compiled in (build with CGO_ENABLED=1 -tags sitemonitor_cgo)")

// Info is passed to the hook.
type Info struct {
	Node   string
	Region string
}
