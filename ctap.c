#include "ctap.h"
#include <Carbon/Carbon.h>
#include <libproc.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* ── Key name table ─────────────────────────────────────────────────────── */

/*
 * keyNameTable[kc] is the canonical name for virtual key code kc, or NULL.
 * keyNameStorage holds the UTF-8 bytes for character-producing keys derived
 * from UCKeyTranslate; semantic keys point directly to string literals.
 *
 * Populated once by initKeyNames(), called before the service is posted.
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
    if (!src) {
        fprintf(stderr, "acme-hotkey: no keyboard input source\n");
        return;
    }
    CFDataRef layoutData =
        TISGetInputSourceProperty(src, kTISPropertyUnicodeKeyLayoutData);
    CFRelease(src);
    if (!layoutData) {
        fprintf(stderr, "acme-hotkey: keyboard input source has no layout\n");
        return;
    }

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

/* Exported by cgo (//export in tap.go): 1 to consume the key, 0 to pass it. */
extern int goKey(unsigned short keyCode, unsigned long long flags, char *app);

static CFMachPortRef gTap;

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

/*
 * consumed[kc] is set when a key-down is consumed, so its auto-repeats and
 * its key-up are consumed too and the application never sees half a
 * keystroke.  Only the tap's run loop thread touches it.
 */
static unsigned char consumed[256];

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
	if (type != kCGEventKeyDown && type != kCGEventKeyUp)
		return event;

	CGKeyCode kc = (CGKeyCode)CGEventGetIntegerValueField(event,
	    kCGKeyboardEventKeycode);
	unsigned long long flags = (unsigned long long)CGEventGetFlags(event);
	unsigned char *c = &consumed[kc & 0xFF];

	if (type == kCGEventKeyUp) {
		if (*c) {
			*c = 0;
			return NULL;
		}
		return event;
	}

	if (CGEventGetIntegerValueField(event, kCGKeyboardEventAutorepeat))
		return *c ? NULL : event;
	if (isModifier(kc) || isSysKey(kc, flags))
		return event;

	pid_t pid = (pid_t)CGEventGetIntegerValueField(event,
	    kCGEventTargetUnixProcessID);
	char app[256];
	app[0] = '\0';
	proc_name((int)pid, app, (uint32_t)sizeof app);

	if (goKey(kc, flags, app)) {
		*c = 1;
		return NULL;
	}
	return event;
}

/* ── Tap lifecycle ─────────────────────────────────────────────────────── */

void
startEventTap(void)
{
	loadSysKeys();

	CGEventMask mask =
	    CGEventMaskBit(kCGEventKeyDown) |
	    CGEventMaskBit(kCGEventKeyUp);

	/*
	 * Appended at the end of the session tap chain, after the Dock's
	 * Mission Control tap, so the shortcuts it consumes never reach us.
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
