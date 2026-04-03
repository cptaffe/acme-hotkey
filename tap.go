package main

/*
#cgo LDFLAGS: -framework Carbon -framework CoreGraphics -framework CoreFoundation -framework AppKit

#include "ctap.h"
*/
import "C"
import "unsafe"

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
