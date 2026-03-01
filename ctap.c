#include "ctap.h"
#include <libproc.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>

/* Exported by CGo (//export in tap.go). */
extern void goKeyDown(unsigned short keyCode, unsigned long long flags);
extern void goKeyUp(unsigned short keyCode, unsigned long long flags);

static CFMachPortRef gTap;

/* ── Process filter ────────────────────────────────────────────────────── */

#define MAX_FILTER 32

static char            filterProcs[MAX_FILTER][256];
static int             nFilter = 0;
static int             nListeners = 0;
static pthread_mutex_t filterMu = PTHREAD_MUTEX_INITIALIZER;

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
 * PASSTHROUGH_MAGIC is stamped into kCGEventSourceUserData on every event
 * we re-inject.  The tap checks this field first so our own injected events
 * are returned immediately without re-processing, preventing infinite loops.
 */
#define PASSTHROUGH_MAGIC 0xACE50001LL

/*
 * shouldInterceptEvent returns 1 when there is at least one active reader
 * and the event's annotated target process matches the filter.
 *
 * At kCGAnnotatedSessionEventTap the kernel has already resolved
 * kCGEventTargetUnixProcessID to the exact receiving process — no
 * NSWorkspace poll, no stale-cache problem.
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

/* ── Key suppression / pass-through tables ─────────────────────────────── */

/*
 * suppressed[kc] — set when we swallow a keyDown; cleared on keyUp.
 * passthrough[kc] — set when reInjectKey is called for that code,
 *   meaning cawk decided the key is not a hotkey and re-injected it.
 *   Used to decide whether auto-repeat events should be forwarded.
 *
 * Both arrays are written from the CGEventTap thread AND (passthrough only)
 * from the Go goroutine that calls reInjectKey.  The race window is tiny
 * (key-repeat delay ≥ 300 ms vs. the cawk round-trip of < 10 ms) so a
 * data race here would at worst cause one missed repeat event.
 *
 * Virtual key codes on current hardware all fit in 7 bits; 256 slots covers
 * the full range of CGKeyCode values we might see.
 */
static char suppressed[256];
static char passthrough[256];

static int
isModifier(CGKeyCode kc)
{
	switch (kc) {
	case 0x37: /* Command        */
	case 0x36: /* Right Command  */
	case 0x38: /* Shift          */
	case 0x3C: /* Right Shift    */
	case 0x3A: /* Option         */
	case 0x3D: /* Right Option   */
	case 0x3B: /* Control        */
	case 0x3E: /* Right Control  */
	case 0x39: /* Caps Lock      */
	case 0x3F: /* fn             */
		return 1;
	}
	return 0;
}

/* ── CGEventTap callback ───────────────────────────────────────────────── */

static CGEventRef
tapCallback(CGEventTapProxy proxy, CGEventType type,
            CGEventRef event, void *refcon)
{
	CGKeyCode kc;
	unsigned long long flags;

	if (type == kCGEventTapDisabledByTimeout ||
	    type == kCGEventTapDisabledByUserInput) {
		CGEventTapEnable(gTap, true);
		return event;
	}

	/*
	 * Re-injected events are stamped with PASSTHROUGH_MAGIC.  Pass them
	 * straight through — no re-processing, no risk of infinite loops.
	 * At kCGAnnotatedSessionEventTap our injected events re-enter the
	 * same tap level, so this check must come first.
	 */
	if (CGEventGetIntegerValueField(event, kCGEventSourceUserData) ==
	    PASSTHROUGH_MAGIC)
		return event;

	flags = (unsigned long long)CGEventGetFlags(event);

	if (type == kCGEventFlagsChanged) {
		/* Modifier-only events always pass through unchanged. */
		return event;
	}

	kc = (CGKeyCode)CGEventGetIntegerValueField(event,
	    kCGKeyboardEventKeycode);

	if (type == kCGEventKeyDown) {
		if (isModifier(kc))
			return event;

		/*
		 * Only intercept when Command is held or the key is Escape.
		 * Plain typing and Shift-only combos pass through immediately —
		 * no round-trip through cawk, no latency, no debounce damage.
		 */
#define kVK_Escape 0x35
		if (!(flags & kCGEventFlagMaskCommand) && kc != kVK_Escape)
			return event;

		if (CGEventGetIntegerValueField(event,
		    kCGKeyboardEventAutorepeat)) {
			/*
			 * Auto-repeat event.  Three cases:
			 *
			 *  1. We never swallowed this key: let it flow.
			 *  2. We swallowed it and cawk re-injected (passthrough):
			 *     let the repeat flow too.
			 *  3. We swallowed it as a hotkey: keep suppressing.
			 */
			if (!suppressed[kc & 0xFF])
				return event;	/* case 1 */
			if (passthrough[kc & 0xFF])
				return event;	/* case 2 */
			return NULL;		/* case 3 */
		}

		if (!shouldInterceptEvent(event))
			return event;

		suppressed[kc & 0xFF] = 1;
		passthrough[kc & 0xFF] = 0;
		goKeyDown((unsigned short)kc, flags);
		return NULL;
	}

	if (type == kCGEventKeyUp) {
		if (suppressed[kc & 0xFF]) {
			int wasPT = passthrough[kc & 0xFF];
			suppressed[kc & 0xFF]  = 0;
			passthrough[kc & 0xFF] = 0;
			goKeyUp((unsigned short)kc, flags);
			/*
			 * For passthrough keys we only injected a keyDown;
			 * the hardware keyUp must reach the application too.
			 * For hotkeys we never sent a keyDown to the app, so
			 * suppress the keyUp as well.
			 */
			return wasPT ? event : NULL;
		}
	}

	return event;
}

/* ── Tap lifecycle ─────────────────────────────────────────────────────── */

void
startEventTap(void)
{
	CFRunLoopSourceRef src;
	CGEventMask mask;

	memset(suppressed,   0, sizeof suppressed);
	memset(passthrough,  0, sizeof passthrough);

	mask = CGEventMaskBit(kCGEventKeyDown)      |
	       CGEventMaskBit(kCGEventKeyUp)         |
	       CGEventMaskBit(kCGEventFlagsChanged);

	/*
	 * kCGSessionEventTap: supports active (modifying) taps, runs after
	 * the session event router so kCGEventTargetUnixProcessID is already
	 * populated.  kCGAnnotatedSessionEventTap is passive-only and cannot
	 * suppress events.  kCGHIDEventTap predates routing so target PID
	 * is always 0 there.
	 */
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

	src = CFMachPortCreateRunLoopSource(kCFAllocatorDefault, gTap, 0);
	CFRunLoopAddSource(CFRunLoopGetCurrent(), src, kCFRunLoopCommonModes);
	CGEventTapEnable(gTap, true);
	CFRunLoopRun();
}

void
reInjectKey(unsigned short kc, unsigned long long flags)
{
	CGEventRef dn;

	/*
	 * Mark as pass-through so the auto-repeat and keyUp handlers know to
	 * forward subsequent events for this key code.
	 *
	 * We only inject the keyDown.  The hardware keyUp is passed through
	 * by the kCGEventKeyUp branch above (wasPT == 1), so the application
	 * receives a matched keyDown/keyUp pair without us synthesising one.
	 */
	passthrough[kc & 0xFF] = 1;

	dn = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)kc, true);
	CGEventSetFlags(dn, (CGEventFlags)flags);
	CGEventSetIntegerValueField(dn, kCGEventSourceUserData, PASSTHROUGH_MAGIC);
	CGEventPost(kCGAnnotatedSessionEventTap, dn);
	CFRelease(dn);
}
