#include "ctap.h"
#include <libproc.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>

/* Exported by CGo (//export in tap.go). */
extern void goKeyDown(unsigned short keyCode, unsigned long long flags);

static CFMachPortRef gTap;

/* ── Process filter ────────────────────────────────────────────────────── */

#define MAX_FILTER 32

static char            filterProcs[MAX_FILTER][256];
static int             nFilter    = 0;
static int             nListeners = 0;
static pthread_mutex_t filterMu   = PTHREAD_MUTEX_INITIALIZER;

void setListenerCount(int n) {
	pthread_mutex_lock(&filterMu);
	nListeners = n;
	pthread_mutex_unlock(&filterMu);
}
void clearFilter(void) {
	pthread_mutex_lock(&filterMu);
	nFilter = 0;
	pthread_mutex_unlock(&filterMu);
}
void addFilterProc(const char *name) {
	pthread_mutex_lock(&filterMu);
	if (nFilter < MAX_FILTER) {
		strncpy(filterProcs[nFilter], name, 255);
		filterProcs[nFilter][255] = '\0';
		nFilter++;
	}
	pthread_mutex_unlock(&filterMu);
}

#define PASSTHROUGH_MAGIC 0xACE50001LL

static int
shouldInterceptEvent(CGEventRef event)
{
	pthread_mutex_lock(&filterMu);
	int nl = nListeners, nf = nFilter;
	pthread_mutex_unlock(&filterMu);

	if (nl == 0) return 0;
	if (nf == 0) return 1;

	pid_t pid = (pid_t)CGEventGetIntegerValueField(event,
	    kCGEventTargetUnixProcessID);
	char name[256]; name[0] = '\0';
	proc_name((int)pid, name, (uint32_t)sizeof(name));

	pthread_mutex_lock(&filterMu);
	int ok = 0;
	for (int i = 0; i < nFilter; i++)
		if (strcmp(name, filterProcs[i]) == 0) { ok = 1; break; }
	pthread_mutex_unlock(&filterMu);
	return ok;
}

/* ── Pending-event circular buffer ─────────────────────────────────────── */

/*
 * gPending is a circular buffer of CFRetained CGEventRefs waiting for
 * write-back acks from the cawk pipeline.
 *
 *   gHead  — next slot the tap thread writes into (producer).
 *   gTail  — next slot ackNext() consumes (consumer, called from Go).
 *
 * Invariant: gHead == gTail means empty; a slot is occupied when gTail ≠ gHead.
 * The buffer is considered full when (gHead+1)%CAP == gTail (one slot wasted).
 *
 * gPendMu serialises access.  It is held briefly in the tap callback and
 * in ackNext(); it is never held while posting events (CFPost is async).
 *
 * pendingKeyUp[kc] stores a CFRetained hardware keyUp event that arrived
 * while events for kc were still in the buffer.  ackNext() posts (or drops)
 * it once hasPendingForKey() returns false.
 */

#define PENDING_CAP 4096

typedef struct { CGEventRef ref; uint16_t kc; } PendingEntry;

static PendingEntry    gPending[PENDING_CAP];
static int             gHead = 0, gTail = 0;
static pthread_mutex_t gPendMu = PTHREAD_MUTEX_INITIALIZER;

static CGEventRef pendingKeyUp[256];

/* Must be called with gPendMu held. */
static int
hasPendingForKey(uint16_t kc)
{
	for (int i = gTail; i != gHead; i = (i + 1) % PENDING_CAP)
		if (gPending[i].kc == kc) return 1;
	return 0;
}

/*
 * ackNext — called from Go for each write-back line received on the keys
 * file.  Dequeues the oldest pending entry, forwards or drops it, then
 * releases any buffered keyUp for that keycode if no more events remain.
 */
void
ackNext(int forward)
{
	pthread_mutex_lock(&gPendMu);
	if (gTail == gHead) {          /* spurious ack — nothing pending */
		pthread_mutex_unlock(&gPendMu);
		return;
	}
	PendingEntry e = gPending[gTail];
	gTail = (gTail + 1) % PENDING_CAP;

	/* Check for remaining events for this keycode after advancing tail. */
	int more = hasPendingForKey(e.kc);
	CGEventRef up = NULL;
	if (!more && pendingKeyUp[e.kc & 0xFF]) {
		up = pendingKeyUp[e.kc & 0xFF];
		pendingKeyUp[e.kc & 0xFF] = NULL;
	}
	pthread_mutex_unlock(&gPendMu);

	/* Post or release the keyDown (outside the lock — CGEventPost is safe).
	 * kCGSessionEventTap: re-enters ABOVE the HID layer but BELOW the
	 * annotated-session layer, so system-shortcut handlers (Dock/Mission
	 * Control for ctrl+arrow Spaces switching, etc.) still see the event,
	 * while our own tap passes it through via PASSTHROUGH_MAGIC. */
	if (forward) {
		CGEventSetIntegerValueField(e.ref, kCGEventSourceUserData,
		    PASSTHROUGH_MAGIC);
		CGEventPost(kCGSessionEventTap, e.ref);
	}
	CFRelease(e.ref);

	/* Post or release the buffered keyUp with the same disposition. */
	if (up) {
		if (forward) {
			CGEventSetIntegerValueField(up, kCGEventSourceUserData,
			    PASSTHROUGH_MAGIC);
			CGEventPost(kCGSessionEventTap, up);
		}
		CFRelease(up);
	}
}

/* ── Modifier check ─────────────────────────────────────────────────────── */

static int
isModifier(CGKeyCode kc)
{
	switch (kc) {
	case 0x37: case 0x36: case 0x38: case 0x3C:
	case 0x3A: case 0x3D: case 0x3B: case 0x3E:
	case 0x39: case 0x3F:
		return 1;
	}
	return 0;
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

	if (CGEventGetIntegerValueField(event, kCGEventSourceUserData) ==
	    PASSTHROUGH_MAGIC)
		return event;

	if (type == kCGEventFlagsChanged)
		return event;

	CGKeyCode          kc    = (CGKeyCode)CGEventGetIntegerValueField(
	                               event, kCGKeyboardEventKeycode);
	unsigned long long flags = (unsigned long long)CGEventGetFlags(event);

	if (type == kCGEventKeyUp) {
		pthread_mutex_lock(&gPendMu);
		int pending = hasPendingForKey((uint16_t)kc);
		if (pending) {
			CFRetain(event);
			pendingKeyUp[kc & 0xFF] = event;
		}
		pthread_mutex_unlock(&gPendMu);
		return pending ? NULL : event;
	}

	if (type == kCGEventKeyDown) {
		if (isModifier(kc))
			return event;

		/*
		 * Auto-repeat events: pass through immediately without entering
		 * the pipeline.  The session-level auto-repeat timer only starts
		 * when a keyDown passes through kCGSessionEventTap, so if we
		 * suppress the first keyDown the OS never generates repeats.
		 * By the time the first repeat arrives (≥500 ms later) the
		 * initial keyDown has long since been acked and the C buffer is
		 * empty, so there is no ordering hazard.
		 */
		int isRepeat = (int)CGEventGetIntegerValueField(event,
		    kCGKeyboardEventAutorepeat);
		if (isRepeat)
			return event;

		if (!shouldInterceptEvent(event))
			return event;

		pthread_mutex_lock(&gPendMu);
		int next = (gHead + 1) % PENDING_CAP;
		if (next == gTail) {
			pthread_mutex_unlock(&gPendMu);
			return event;
		}
		CFRetain(event);
		gPending[gHead] = (PendingEntry){ event, (uint16_t)kc };
		gHead = next;
		pthread_mutex_unlock(&gPendMu);

		goKeyDown((unsigned short)kc, flags);
		return NULL;
	}

	return event;
}

/* ── Tap lifecycle ─────────────────────────────────────────────────────── */

void
startEventTap(void)
{
	memset(pendingKeyUp, 0, sizeof pendingKeyUp);
	gHead = gTail = 0;

	CGEventMask mask =
	    CGEventMaskBit(kCGEventKeyDown) |
	    CGEventMaskBit(kCGEventKeyUp)   |
	    CGEventMaskBit(kCGEventFlagsChanged);

	gTap = CGEventTapCreate(
		kCGSessionEventTap,
		kCGHeadInsertEventTap,
		kCGEventTapOptionDefault,
		mask, tapCallback, NULL);

	if (!gTap) {
		fprintf(stderr,
		    "acme-hotkey: CGEventTapCreate failed\n"
		    "  Grant Accessibility: System Settings → Privacy → Accessibility\n");
		return;
	}

	CFRunLoopSourceRef src =
	    CFMachPortCreateRunLoopSource(kCFAllocatorDefault, gTap, 0);
	CFRunLoopAddSource(CFRunLoopGetCurrent(), src, kCFRunLoopCommonModes);
	CGEventTapEnable(gTap, true);
	CFRunLoopRun();
}
