package main

/*
#cgo LDFLAGS: -framework Carbon -framework CoreGraphics -framework CoreFoundation -framework AppKit

#include "ctap.h"
*/
import "C"

import (
	"sort"
	"sync"
	"unsafe"
)

// KeyEvent is a keyboard state snapshot published to subscribers.
type KeyEvent struct {
	KeyCode uint16 // triggering key (0 for modifier-only changes)
	Flags   uint64 // CGEventFlags at event time
	Str     string // full current held-key state, e.g. "command shift s"
	IsDown  bool   // true = keydown, false = keyup or flags-only change
}

// heldKeys tracks which non-modifier key codes are currently held down.
var heldKeys sync.Map // map[uint16]struct{}

// keyEventChan carries state snapshots from the CGEventTap into Go.
var keyEventChan = make(chan KeyEvent, 256)

// publish builds a KeyEvent from the current held-key state and sends it.
func publish(kc uint16, flags uint64, isDown bool) {
	ev := KeyEvent{
		KeyCode: kc,
		Flags:   flags,
		IsDown:  isDown,
		Str:     currentState(flags),
	}
	select {
	case keyEventChan <- ev:
	default: // drop when full
	}
}

// currentState formats all currently held keys as a space-separated string,
// modifiers first (command, shift, control, option), then regular keys sorted
// by key code for deterministic output.
func currentState(flags uint64) string {
	// Collect regular held keys.
	var codes []uint16
	heldKeys.Range(func(k, _ any) bool {
		codes = append(codes, k.(uint16))
		return true
	})
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	return formatState(flags, codes)
}

// goKeyDown is called from C for every non-modifier keyDown.
//
//export goKeyDown
func goKeyDown(kc C.ushort, flags C.ulonglong) {
	code := uint16(kc)
	heldKeys.Store(code, struct{}{})
	publish(code, uint64(flags), true)
}

// goKeyUp is called from C for every non-modifier keyUp whose keyDown was
// suppressed by our tap.
//
//export goKeyUp
func goKeyUp(kc C.ushort, flags C.ulonglong) {
	code := uint16(kc)
	heldKeys.Delete(code)
	publish(code, uint64(flags), false)
}



// frontmostApp returns the BSD process name of the current frontmost
// application (delegates to NSWorkspace in frontapp.m).
func frontmostApp() string {
	return C.GoString(C.frontmostProcName())
}

// setListenerCount tells the C tap how many goroutines are actively reading
// the keys file.  When the count is zero the tap passes all events through
// unconditionally, so a dead cawk pipeline never blocks input system-wide.
func setListenerCount(n int) {
	C.setListenerCount(C.int(n))
}

// setFilter replaces the active process-name filter.  An empty slice means
// intercept all applications.  Names are BSD process names as returned by
// proc_name(3) — typically the executable basename, e.g. "devdraw".
func setFilter(names []string) {
	C.clearFilter()
	for _, name := range names {
		cs := C.CString(name)
		C.addFilterProc(cs)
		C.free(unsafe.Pointer(cs))
	}
}

// startTap starts the CGEventTap run loop.  Blocks until the process exits.
func startTap() {
	C.startEventTap()
}

// reInjectKey re-injects ev into the event stream at the annotated-session
// level, bypassing our HID tap, so it reaches the focused application.
func reInjectKey(ev KeyEvent) {
	C.reInjectKey(C.ushort(ev.KeyCode), C.ulonglong(ev.Flags))
}
