#ifndef SITEMONITOR_STARTUP_HOOK_H
#define SITEMONITOR_STARTUP_HOOK_H

/*
 * Called once when the monitor server comes up (after it knows which regions
 * it owns). Return 0 on success; any other value is logged as a failure and
 * raised as a STARTUP event. Must return quickly (it runs before scheduling).
 */
int sitemonitor_on_startup(const char *node, const char *region);

#endif
