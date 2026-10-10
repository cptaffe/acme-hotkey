package main

/*
#cgo LDFLAGS: -framework Carbon -framework CoreGraphics -framework CoreFoundation -framework AppKit

#include "ctap.h"
*/
import "C"
import (
	"log"
	"strings"
	"unsafe"
)

// KeyEvent is a keyboard event published to the keys stream.
// The original CGEventRef lives entirely in C's circular buffer; Go never
// holds it.  Auto-repeat events are passed through directly in C and never
// reach Go.
type KeyEvent struct {
	KeyCode uint16
	Flags   uint64
	Str     string // e.g. "command b", "shift a", "escape"
}

// eventStream carries KeyEvents from the CGEventTap callback into Go.
var eventStream = make(chan KeyEvent, 4096)

// goKeyDown is called from C for every intercepted non-modifier, non-repeat
// keyDown.  Auto-repeat events are passed through directly in the tap
// callback and never reach Go.  The CGEventRef is already CFRetain'd and
// stored in C's circular buffer; Go only needs the string for the pipeline.
//
//export goKeyDown
func goKeyDown(kc C.ushort, flags C.ulonglong) {
	code := uint16(kc)
	str := formatState(uint64(flags), []uint16{code})
	ev := KeyEvent{KeyCode: code, Flags: uint64(flags), Str: str}
	select {
	case eventStream <- ev:
	default:
		C.ackNext(0)
	}
}

// ackNext tells the C layer to dispose of the oldest pending event.
// forward=true  → re-post original CGEventRef (PASSTHROUGH_MAGIC stamped).
// forward=false → drop it silently.
func ackNext(forward bool) {
	if forward {
		C.ackNext(1)
	} else {
		C.ackNext(0)
	}
}

func frontmostApp() string { return C.GoString(C.frontmostProcName()) }

func setListenerCount(n int) { C.setListenerCount(C.int(n)) }

func setFilter(names []string) {
	C.clearFilter()
	for _, name := range names {
		cs := C.CString(name)
		C.addFilterProc(cs)
		C.free(unsafe.Pointer(cs))
	}
}

// modFlags maps event modifier names to CGEventFlags masks.
var modFlags = map[string]uint64{
	"command": 0x100000,
	"shift":   0x020000,
	"control": 0x040000,
	"option":  0x080000,
}

// setGrabs restricts interception to the keystrokes named by lines, each
// in the event format ("command shift b").  Lines naming no known key are
// skipped.  No lines restores intercepting every keystroke.
func setGrabs(lines []string) {
	C.clearGrabs()
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		var mods uint64
		ok := true
		for _, m := range f[:len(f)-1] {
			flag, found := modFlags[m]
			ok = ok && found
			mods |= flag
		}
		kc, found := keyCode(f[len(f)-1])
		if !ok || !found {
			log.Printf("acme-hotkey: grab %q: unknown key", line)
			continue
		}
		C.addGrab(C.ushort(kc), C.ulonglong(mods))
	}
}

// keyCode is the inverse of keyCodeName.
func keyCode(name string) (uint16, bool) {
	for code := 0; code < 256; code++ {
		if keyCodeName(uint16(code)) == name {
			return uint16(code), true
		}
	}
	return 0, false
}

// initKeys builds the key-name table from the current keyboard layout.
// It must run before clients can write grabs, which are resolved by name.
func initKeys() { C.initKeyNames() }

func startTap() { C.startEventTap() }

// keyCodeName returns the canonical name for a virtual key code as derived
// from the current keyboard layout by initKeyNames (called at tap startup).
// Returns "" for unknown or unassigned key codes.
func keyCodeName(code uint16) string {
	name := C.virtualKeyName(C.ushort(code))
	if name == nil {
		return ""
	}
	return C.GoString(name)
}

// drainPending discards all C-side pending events without forwarding them.
// Call when the keys reader disconnects so the next pipeline start has a
// clean circular buffer and no stale-offset artefacts in the ack queue.
func drainPending() { C.drainAllPending() }
