#include "ctap.h"
#include <libproc.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>

/*
 * goKeyDown is //export-ed from tap.go.
 * eventPtr is a CFRetained CGEventRef cast to uintptr_t; ownership transfers
 * to Go, which must call forwardEvent or dropEvent to release it.
 * isRepeat is non-zero for auto-repeat hardware events.
 */
extern void goKeyDown(unsigned short keyCode, unsigned long long flags,
                      uintptr_t eventPtr, int isRepeat);

static CFMachPortRef gTap;

/* ── Process filter ────────────────────────────────────────────────────── */

#define MAX_FILTER 32

static char            filterProcs[MAX_FILTER][256];
static int             nFilter    = 0;
static int             nListeners = 0;
static pthread_mutex_t filterMu   = PTHREAD_MUTEX_INITIALIZER;

void
setListenerCount(int n)
{
	pthread_mutex_lock(&filterMu);
	nListeners = n;
	pthread_mutex_unlock(&filterMu);
}

void
clearFilter(void)
{
	pthread_mutex_lock(&filterMu);
	nFilter = 0;
	pthread_mutex_unlock(&filterMu);
}

void
addFilterProc(const char *name)
{
	pthread_mutex_lock(&filterMu);
	if (nFilter < MAX_FILTER) {
		strncpy(filterProcs[nFilter], name, 255);
		filterProcs[nFilter][255] = '\0';
		nFilter++;
	}
	pthread_mutex_unlock(&filterMu);
}

/*
 * PASSTHROUGH_MAGIC is stamped onto every event we re-inject so the tap
 * recognises and immediately passes them through, preventing re-entry loops.
 */
#define PASSTHROUGH_MAGIC 0xACE50001LL

/*
 * shouldInterceptEvent — returns 1 when there is at least one active reader
 * and the event's kernel-annotated target process matches the filter.
 * At kCGSessionEventTap kCGEventTargetUnixProcessID is already populated by
 * the session router, giving us the exact receiving process without any
 * NSWorkspace poll (which is stale on the tap thread).
 */
static int
shouldInterceptEvent(CGEventRef event)
{
	pthread_mutex_lock(&filterMu);
	int nl = nListeners;
	int nf = nFilter;
	pthread_mutex_unlock(&filterMu);

	if (nl == 0)
		return 0;
	if (nf == 0)
		return 1;

	pid_t pid = (pid_t)CGEventGetIntegerValueField(event,
	    kCGEventTargetUnixProcessID);
	char name[256];
	name[0] = '\0';
	proc_name((int)pid, name, (uint32_t)sizeof(name));

	pthread_mutex_lock(&filterMu);
	int ok = 0;
	for (int i = 0; i < nFilter; i++) {
		if (strcmp(name, filterProcs[i]) == 0) { ok = 1; break; }
	}
	pthread_mutex_unlock(&filterMu);
	return ok;
}

/* ── Per-keycode intercept state ───────────────────────────────────────── */

/*
 * intercepted[kc] — set when we CFRetain+swallow a keyDown and hand it to
 *   Go; cleared by forwardEvent/dropEvent once the ack arrives.
 *
 * pendingKeyUp[kc] — if the hardware keyUp arrives while intercepted[kc] is
 *   still set (i.e. before the ack), we CFRetain it here so forwardEvent can
 *   post it afterwards.  dropEvent releases it silently.
 *
 * Both arrays are accessed from two threads (CFRunLoop tap thread and the Go
 * goroutine calling forwardEvent/dropEvent), but the access pattern is
 * race-free in practice: the tap thread writes intercepted=1 and Go clears
 * it; the tap thread writes pendingKeyUp and Go clears it.  The window where
 * a race could cause a missed suppression is smaller than any human key
 * press duration.
 */
static char       intercepted[256];
static CGEventRef pendingKeyUp[256];

static int
isModifier(CGKeyCode kc)
{
	switch (kc) {
	case 0x37: case 0x36: /* Command L/R    */
	case 0x38: case 0x3C: /* Shift L/R      */
	case 0x3A: case 0x3D: /* Option L/R     */
	case 0x3B: case 0x3E: /* Control L/R    */
	case 0x39:             /* Caps Lock      */
	case 0x3F:             /* fn             */
		return 1;
	}
	return 0;
}

/* ── Event disposition (called from Go) ────────────────────────────────── */

/*
 * forwardEvent — cawk has decided to pass this key through.
 * Post the original keyDown (with PASSTHROUGH_MAGIC) and, if the hardware
 * keyUp already arrived, post that too.
 */
void
forwardEvent(uintptr_t eventPtr, unsigned short kc)
{
	CGEventRef dn = (CGEventRef)eventPtr;
	int idx = kc & 0xFF;

	intercepted[idx] = 0;

	CGEventSetIntegerValueField(dn, kCGEventSourceUserData, PASSTHROUGH_MAGIC);
	CGEventPost(kCGAnnotatedSessionEventTap, dn);
	CFRelease(dn);

	if (pendingKeyUp[idx]) {
		CGEventRef up = pendingKeyUp[idx];
		pendingKeyUp[idx] = NULL;
		CGEventSetIntegerValueField(up, kCGEventSourceUserData,
		    PASSTHROUGH_MAGIC);
		CGEventPost(kCGAnnotatedSessionEventTap, up);
		CFRelease(up);
	}
}

/*
 * dropEvent — cawk consumed this key as a hotkey.
 * Release the retained keyDown and any buffered keyUp without posting either.
 */
void
dropEvent(uintptr_t eventPtr, unsigned short kc)
{
	int idx = kc & 0xFF;

	intercepted[idx] = 0;
	CFRelease((CGEventRef)eventPtr);

	if (pendingKeyUp[idx]) {
		CFRelease(pendingKeyUp[idx]);
		pendingKeyUp[idx] = NULL;
	}
}

/* ── CGEventTap callback ───────────────────────────────────────────────── */

static CGEventRef
tapCallback(CGEventTapProxy proxy, CGEventType type,
            CGEventRef event, void *refcon)
{
	(void)proxy; (void)refcon;

	if (type == kCGEventTapDisabledByTimeout ||
	    type == kCGEventTapDisabledByUserInput) {
		CGEventTapEnable(gTap, true);
		return event;
	}

	/* Our own re-injected events carry PASSTHROUGH_MAGIC — let them fly. */
	if (CGEventGetIntegerValueField(event, kCGEventSourceUserData) ==
	    PASSTHROUGH_MAGIC)
		return event;

	if (type == kCGEventFlagsChanged)
		return event;

	CGKeyCode          kc    = (CGKeyCode)CGEventGetIntegerValueField(
	    event, kCGKeyboardEventKeycode);
	unsigned long long flags = (unsigned long long)CGEventGetFlags(event);

	if (type == kCGEventKeyUp) {
		int idx = kc & 0xFF;
		if (intercepted[idx]) {
			/*
			 * The ack has not arrived yet.  Buffer this keyUp so
			 * forwardEvent can post it at the right time.
			 */
			CFRetain(event);
			pendingKeyUp[idx] = event;
			return NULL;
		}
		return event;
	}

	if (type == kCGEventKeyDown) {
		if (isModifier(kc))
			return event;
		if (!shouldInterceptEvent(event))
			return event;

		int isRepeat = (int)CGEventGetIntegerValueField(event,
		    kCGKeyboardEventAutorepeat);

		intercepted[kc & 0xFF] = 1;
		CFRetain(event);
		goKeyDown((unsigned short)kc, flags, (uintptr_t)event, isRepeat);
		return NULL;
	}

	return event;
}

/* ── Tap lifecycle ─────────────────────────────────────────────────────── */

void
startEventTap(void)
{
	memset(intercepted,  0, sizeof intercepted);
	memset(pendingKeyUp, 0, sizeof pendingKeyUp);

	CGEventMask mask =
	    CGEventMaskBit(kCGEventKeyDown) |
	    CGEventMaskBit(kCGEventKeyUp)   |
	    CGEventMaskBit(kCGEventFlagsChanged);

	gTap = CGEventTapCreate(
		kCGSessionEventTap,
		kCGHeadInsertEventTap,
		kCGEventTapOptionDefault,
		mask,
		tapCallback,
		NULL);

	if (!gTap) {
		fprintf(stderr,
		    "acme-hotkey: CGEventTapCreate failed\n"
		    "  Grant Accessibility access:\n"
		    "  System Settings → Privacy & Security → Accessibility\n");
		return;
	}

	CFRunLoopSourceRef src =
	    CFMachPortCreateRunLoopSource(kCFAllocatorDefault, gTap, 0);
	CFRunLoopAddSource(CFRunLoopGetCurrent(), src, kCFRunLoopCommonModes);
	CGEventTapEnable(gTap, true);
	CFRunLoopRun();
}
