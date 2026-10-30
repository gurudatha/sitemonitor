//go:build sitemonitor_cgo

package startup

/*
#include <stdlib.h>
#include "startup_hook.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Run calls sitemonitor_on_startup(node, region) from startup_hook.c.
// A non-zero return value is reported as an error.
func Run(info Info) (int, error) {
	node, region := C.CString(info.Node), C.CString(info.Region)
	defer C.free(unsafe.Pointer(node))
	defer C.free(unsafe.Pointer(region))
	rc := int(C.sitemonitor_on_startup(node, region))
	if rc != 0 {
		return rc, fmt.Errorf("startup hook returned %d", rc)
	}
	return 0, nil
}

// Compiled reports whether the native hook is part of this binary.
func Compiled() bool { return true }
