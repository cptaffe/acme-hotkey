#include "ctap.h"
#include <libproc.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>

/* Exported by CGo (//export in tap.go). */
extern void goKeyDown(unsigned short keyCode, unsigned long long flags);
extern void goKeyUp(unsigned short keyCode, unsigned long long flags);
extern void goFlagsChanged(unsigned long long flags);

static CFMachPortRef gTap;

/* ── Process filter ────────────────────────────────────────────────────── */

#define MAX_FILTER 32

static char            filterProcs[MAX_FILTER][256];
static int             nFilter = 0;
static pthread_mutex_t filterMu = PTHREAD_MUTEX_INITIALIZER;

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
 * frontmostProcName returns the BSD process name of the currently focused
 * application by querying the Accessibility API (same permission that the
 * CGEventTap already requires).
 */
static const char *
frontmostProcName(void)
{
	static char name[256];
	pid_t pid = 0;

	AXUIElementRef sysWide = AXUIElementCreateSystemWide();
	AXUIElementRef app = NULL;

	if (AXUIElementCopyAttributeValue(sysWide,
	    kAXFocusedApplicationAttribute, (CFTypeRef *)&app) == kAXErrorSuccess
	    && app != NULL) {
		AXUIElementGetPid(app, &pid);
		CFRelease(app);
	}
	CFRelease(sysWide);

	name[0] = '\0';
	if (pid > 0)
		proc_name((int)pid, name, sizeof(name));
	return name;
}

/*
 * shouldIntercept returns 1 if the current frontmost process is in the
 * filter list, or if no filter is set (intercept everything).
 */
static int
shouldIntercept(void)
{
	pthread_mutex_lock(&filterMu);
	int n = nFilter;
	pthread_mutex_unlock(&filterMu);

	if (n == 0)
		return 1;

	const char *front = frontmostProcName();

	pthread_mutex_lock(&filterMu);
	int ok = 0;
	for (int i = 0; i < nFilter; i++) {
		if (strcmp(front, filterProcs[i]) == 0) {
			ok = 1;
			break;
		}
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

	flags = (unsigned long long)CGEventGetFlags(event);

	if (type == kCGEventFlagsChanged) {
		/* Modifier events always pass through; just inform Go. */
		goFlagsChanged(flags);
		return event;
	}

	kc = (CGKeyCode)CGEventGetIntegerValueField(event,
	    kCGKeyboardEventKeycode);

	if (type == kCGEventKeyDown) {
		if (isModifier(kc))
			return event;

		if (CGEventGetIntegerValueField(event,
		    kCGKeyboardEventAutorepeat)) {
			/*
			 * Auto-repeat event.  Three cases:
			 *
			 *  1. We never swallowed this key (e.g. focus changed
			 *     between keyDown and the first repeat, or the key
			 *     was originally passed through): let it flow.
			 *
			 *  2. We swallowed it and cawk re-injected it
			 *     (passthrough): re-inject this repeat too so the
			 *     user gets normal key-repeat behaviour.
			 *
			 *  3. We swallowed it as a hotkey (no re-injection):
			 *     keep suppressing — commands should not repeat.
			 */
			if (!suppressed[kc & 0xFF])
				return event;	/* case 1 */

			if (passthrough[kc & 0xFF]) {
				/* case 2: re-inject as an auto-repeat keyDown */
				CGEventRef rep =
				    CGEventCreateKeyboardEvent(NULL,
				    (CGKeyCode)kc, true);
				CGEventSetIntegerValueField(rep,
				    kCGKeyboardEventAutorepeat, 1);
				CGEventSetFlags(rep, (CGEventFlags)flags);
				CGEventPost(kCGAnnotatedSessionEventTap, rep);
				CFRelease(rep);
			}
			return NULL;		/* case 2+3: drop original */
		}

		if (!shouldIntercept())
			return event;

		suppressed[kc & 0xFF] = 1;
		passthrough[kc & 0xFF] = 0;	/* reset on each fresh keyDown */
		goKeyDown((unsigned short)kc, flags);
		return NULL;
	}

	if (type == kCGEventKeyUp) {
		if (suppressed[kc & 0xFF]) {
			suppressed[kc & 0xFF] = 0;
			passthrough[kc & 0xFF] = 0;
			goKeyUp((unsigned short)kc, flags);
			return NULL;
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

	gTap = CGEventTapCreate(
		kCGHIDEventTap,
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
	CGEventRef dn, up;

	/*
	 * Mark as pass-through so the auto-repeat handler knows to forward
	 * subsequent repeat events for this key code.
	 */
	passthrough[kc & 0xFF] = 1;

	dn = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)kc, true);
	CGEventSetFlags(dn, (CGEventFlags)flags);
	CGEventPost(kCGAnnotatedSessionEventTap, dn);
	CFRelease(dn);

	up = CGEventCreateKeyboardEvent(NULL, (CGKeyCode)kc, false);
	CGEventSetFlags(up, (CGEventFlags)flags);
	CGEventPost(kCGAnnotatedSessionEventTap, up);
	CFRelease(up);
}
