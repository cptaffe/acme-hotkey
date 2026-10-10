#include "ctap.h"
#include <Carbon/Carbon.h>
#include <libproc.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* ── Key name table ─────────────────────────────────────────────────────── */

/*
 * keyNameTable[kc] is the canonical name for virtual key code kc, or NULL.
 * keyNameStorage holds the UTF-8 bytes for character-producing keys derived
 * from UCKeyTranslate; semantic keys point directly to string literals.
 *
 * Populated once by initKeyNames(), called at the top of startEventTap().
 */
#define KEY_NAME_MAX 8

static char        keyNameStorage[256][KEY_NAME_MAX];
static const char *keyNameTable[256];

void
initKeyNames(void)
{
    /* Semantic keys: named by function, not by the character they print. */
    static const struct { uint16_t kc; const char *name; } specials[] = {
        { kVK_Return,           "return"        },
        { kVK_Tab,              "tab"           },
        { kVK_Space,            "space"         },
        { kVK_Delete,           "delete"        },
        { kVK_Escape,           "escape"        },
        { kVK_ForwardDelete,    "forwarddelete" },
        { kVK_Home,             "home"          },
        { kVK_End,              "end"           },
        { kVK_PageUp,           "pageup"        },
        { kVK_PageDown,         "pagedown"      },
        { kVK_LeftArrow,        "left"          },
        { kVK_RightArrow,       "right"         },
        { kVK_DownArrow,        "down"          },
        { kVK_UpArrow,          "up"            },
        { kVK_Help,             "help"          },
        { kVK_F1,               "f1"            },
        { kVK_F2,               "f2"            },
        { kVK_F3,               "f3"            },
        { kVK_F4,               "f4"            },
        { kVK_F5,               "f5"            },
        { kVK_F6,               "f6"            },
        { kVK_F7,               "f7"            },
        { kVK_F8,               "f8"            },
        { kVK_F9,               "f9"            },
        { kVK_F10,              "f10"           },
        { kVK_F11,              "f11"           },
        { kVK_F12,              "f12"           },
        { kVK_F13,              "f13"           },
        { kVK_F14,              "f14"           },
        { kVK_F15,              "f15"           },
        { kVK_F16,              "f16"           },
        /* Keypad keys that have no printable character equivalent. */
        { kVK_ANSI_KeypadClear, "clear"         },
        /* Keypad enter normalises to the same name as regular return. */
        { kVK_ANSI_KeypadEnter, "return"        },
    };
    for (size_t i = 0; i < sizeof specials / sizeof specials[0]; i++) {
        uint16_t kc = specials[i].kc;
        if (kc < 256)
            keyNameTable[kc] = specials[i].name;
    }

    /*
     * Character-producing keys (letters, digits, symbols, keypad operators):
     * ask the current keyboard layout what character each key produces with
     * no modifiers.  Keypad digits and symbols come out as "0"–"9", "*",
     * "+", "-", "/", "=" — identical to their non-keypad equivalents.
     */
    TISInputSourceRef src = TISCopyCurrentKeyboardInputSource();
    if (!src)
        return;
    CFDataRef layoutData =
        TISGetInputSourceProperty(src, kTISPropertyUnicodeKeyLayoutData);
    CFRelease(src);
    if (!layoutData)
        return;

    const UCKeyboardLayout *layout =
        (const UCKeyboardLayout *)CFDataGetBytePtr(layoutData);

#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
    UInt32 kbdType = LMGetKbdType();
#pragma clang diagnostic pop

    for (uint16_t kc = 0; kc < 256; kc++) {
        if (keyNameTable[kc])
            continue; /* already assigned a semantic name */

        UInt32      deadKeyState = 0;
        UniChar     chars[4];
        UniCharCount actualLen  = 0;

        OSStatus st = UCKeyTranslate(
            layout, kc, kUCKeyActionDisplay,
            0, kbdType, kUCKeyTranslateNoDeadKeysBit,
            &deadKeyState, 4, &actualLen, chars);
        if (st != noErr || actualLen == 0)
            continue;

        CFStringRef str =
            CFStringCreateWithCharacters(kCFAllocatorDefault, chars, actualLen);
        if (!str)
            continue;
        Boolean ok = CFStringGetCString(str, keyNameStorage[kc],
                                        KEY_NAME_MAX, kCFStringEncodingUTF8);
        CFRelease(str);

        if (ok && keyNameStorage[kc][0] != '\0')
            keyNameTable[kc] = keyNameStorage[kc];
    }
}

const char *
virtualKeyName(uint16_t kc)
{
    return kc < 256 ? keyNameTable[kc] : NULL;
}

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
	 * kCGAnnotatedSessionEventTap delivers directly to the target application
	 * with minimal latency.  Session-level re-posting (kCGSessionEventTap)
	 * routes through the Dock's synchronous Mission Control tap for every
	 * event, adding a full keypress of lag.  We handle ctrl+key shortcuts by
	 * not intercepting them in the first place (see tapCallback below). */
	if (forward) {
		CGEventSetIntegerValueField(e.ref, kCGEventSourceUserData,
		    PASSTHROUGH_MAGIC);
		CGEventPost(kCGAnnotatedSessionEventTap, e.ref);
	}
	CFRelease(e.ref);

	/* Post or release the buffered keyUp with the same disposition. */
	if (up) {
		if (forward) {
			CGEventSetIntegerValueField(up, kCGEventSourceUserData,
			    PASSTHROUGH_MAGIC);
			CGEventPost(kCGAnnotatedSessionEventTap, up);
		}
		CFRelease(up);
	}
}

/*
 * drainAllPending — discard every entry in gPending without forwarding.
 *
 * Called from Go's releaseKeysReader (while keysReaderMu is held) so that
 * a pipeline restart begins with an empty circular buffer.  Without this,
 * one stale gPending entry survives each crash-restart cycle, permanently
 * offsetting the ack queue by one: the N-th ack posts event N-1.
 */
void
drainAllPending(void)
{
	pthread_mutex_lock(&gPendMu);
	while (gHead != gTail) {
		PendingEntry e = gPending[gTail];
		gTail = (gTail + 1) % PENDING_CAP;
		CGEventRef up = pendingKeyUp[e.kc & 0xFF];
		pendingKeyUp[e.kc & 0xFF] = NULL;
		pthread_mutex_unlock(&gPendMu);   /* release during CF calls */
		CFRelease(e.ref);
		if (up) CFRelease(up);
		pthread_mutex_lock(&gPendMu);
	}
	pthread_mutex_unlock(&gPendMu);
}

/* ── System shortcuts ──────────────────────────────────────────────────── */

/*
 * The window server handles system shortcuts (Spotlight, screenshots) only
 * for keystrokes that leave the tap chain normally.  An event we suppress and
 * re-post at kCGAnnotatedSessionEventTap goes straight to the application and
 * never reaches it, so ⌘Space and ⌘⇧4 silently stop working while Acme is
 * frontmost.  Pass through every keystroke that matches an enabled system
 * shortcut.
 *
 * The table starts from the macOS defaults, which com.apple.symbolichotkeys
 * omits until the user changes them, and is overridden by that domain's
 * AppleSymbolicHotKeys entries.  It is read once, by startEventTap().
 */
#define MOD_MASK (kCGEventFlagMaskCommand | kCGEventFlagMaskShift | \
                  kCGEventFlagMaskControl | kCGEventFlagMaskAlternate)
#define MAX_SYSKEYS 256

typedef struct { int id; CGKeyCode kc; unsigned long long mods; int on; } SysKey;

static SysKey sysKeys[MAX_SYSKEYS] = {
	{  28, kVK_ANSI_3, kCGEventFlagMaskCommand | kCGEventFlagMaskShift, 1 },
	{  29, kVK_ANSI_3, kCGEventFlagMaskCommand | kCGEventFlagMaskShift |
	                   kCGEventFlagMaskControl, 1 },
	{  30, kVK_ANSI_4, kCGEventFlagMaskCommand | kCGEventFlagMaskShift, 1 },
	{  31, kVK_ANSI_4, kCGEventFlagMaskCommand | kCGEventFlagMaskShift |
	                   kCGEventFlagMaskControl, 1 },
	{  64, kVK_Space,  kCGEventFlagMaskCommand, 1 },
	{  65, kVK_Space,  kCGEventFlagMaskCommand | kCGEventFlagMaskAlternate, 1 },
	{ 184, kVK_ANSI_5, kCGEventFlagMaskCommand | kCGEventFlagMaskShift, 1 },
};
static int nSysKeys = 7;

static int
cfInt(CFTypeRef v, long long *out)
{
	return v && CFGetTypeID(v) == CFNumberGetTypeID() &&
	    CFNumberGetValue((CFNumberRef)v, kCFNumberLongLongType, out);
}

static void
loadSysKey(const void *key, const void *value, void *ctx)
{
	(void)ctx;
	char buf[16];
	if (CFGetTypeID(key) != CFStringGetTypeID() ||
	    !CFStringGetCString((CFStringRef)key, buf, sizeof buf, kCFStringEncodingUTF8) ||
	    CFGetTypeID(value) != CFDictionaryGetTypeID())
		return;
	int id = atoi(buf);
	CFDictionaryRef d = (CFDictionaryRef)value;

	CFTypeRef en = CFDictionaryGetValue(d, CFSTR("enabled"));
	int on = en && CFGetTypeID(en) == CFBooleanGetTypeID() &&
	    CFBooleanGetValue((CFBooleanRef)en);

	long long kc = -1, mods = 0;
	CFTypeRef v = CFDictionaryGetValue(d, CFSTR("value"));
	if (v && CFGetTypeID(v) == CFDictionaryGetTypeID()) {
		CFTypeRef p = CFDictionaryGetValue((CFDictionaryRef)v, CFSTR("parameters"));
		if (p && CFGetTypeID(p) == CFArrayGetTypeID() &&
		    CFArrayGetCount((CFArrayRef)p) >= 3) {
			cfInt(CFArrayGetValueAtIndex((CFArrayRef)p, 1), &kc);
			cfInt(CFArrayGetValueAtIndex((CFArrayRef)p, 2), &mods);
		}
	}

	SysKey *k = NULL;
	for (int i = 0; i < nSysKeys; i++)
		if (sysKeys[i].id == id) { k = &sysKeys[i]; break; }
	if (!k) {
		if (nSysKeys == MAX_SYSKEYS)
			return;
		k = &sysKeys[nSysKeys++];
		*k = (SysKey){ id, 0, 0, 0 };
	}
	k->on = on;
	if (kc >= 0 && kc != 0xFFFF) {
		k->kc = (CGKeyCode)kc;
		k->mods = (unsigned long long)mods & MOD_MASK;
	} else if (k->mods == 0) {
		k->on = 0;	/* enabled but unbound: nothing to match */
	}
}

static void
loadSysKeys(void)
{
	CFPropertyListRef hk = CFPreferencesCopyAppValue(
	    CFSTR("AppleSymbolicHotKeys"), CFSTR("com.apple.symbolichotkeys"));
	if (!hk)
		return;
	if (CFGetTypeID(hk) == CFDictionaryGetTypeID())
		CFDictionaryApplyFunction((CFDictionaryRef)hk, loadSysKey, NULL);
	CFRelease(hk);
}

static int
isSysKey(CGKeyCode kc, unsigned long long flags)
{
	flags &= MOD_MASK;
	for (int i = 0; i < nSysKeys; i++)
		if (sysKeys[i].on && sysKeys[i].kc == kc && sysKeys[i].mods == flags)
			return 1;
	return 0;
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

		/*
		 * Pass ctrl+key combos through unintercepted.  Spaces switching
		 * (ctrl+arrow, ctrl+number) and other ctrl shortcuts are handled
		 * by the window server after ALL CGEventTaps have run.  If we
		 * suppress a ctrl+key event here, it never reaches the window
		 * server and Spaces switching silently fails.  Passing through
		 * lets it reach the window server naturally.
		 *
		 * kCGEventFlagMaskControl = 0x0000000000040000
		 */
		if (flags & kCGEventFlagMaskControl)
			return event;

		if (isSysKey(kc, flags))
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
	initKeyNames();
	loadSysKeys();
	memset(pendingKeyUp, 0, sizeof pendingKeyUp);
	gHead = gTail = 0;

	CGEventMask mask =
	    CGEventMaskBit(kCGEventKeyDown) |
	    CGEventMaskBit(kCGEventKeyUp)   |
	    CGEventMaskBit(kCGEventFlagsChanged);

	/*
	 * kCGTailAppendEventTap: we are inserted at the END of the session-level
	 * tap chain, after all head-insert taps (including the Dock's Mission
	 * Control tap).  Events that Mission Control consumes (ctrl+arrow for
	 * Spaces switching, ctrl+number for direct-Space jump, etc.) never reach
	 * us.  Events that pass through all head-insert taps arrive here; we
	 * suppress them (return NULL) and re-post at kCGAnnotatedSessionEventTap
	 * so they are delivered directly to the target application without
	 * traversing any session-level tap again — giving lag-free delivery.
	 */
	gTap = CGEventTapCreate(
		kCGSessionEventTap,
		kCGTailAppendEventTap,
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
