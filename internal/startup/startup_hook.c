//go:build sitemonitor_cgo
// (Compiled only with -tags sitemonitor_cgo and CGO_ENABLED=1; the default
//  pure-Go static build ignores this file.)

/*
 * PLACEHOLDER for the native startup program. Replace the body with the real
 * implementation; keep the signature from startup_hook.h.
 */
#include <stdio.h>
#include "startup_hook.h"

int sitemonitor_on_startup(const char *node, const char *region) {
    fprintf(stderr, "sitemonitor startup hook placeholder: node=%s region=%s\n", node, region);
    return 0;
}
