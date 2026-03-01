package main

/*
#cgo LDFLAGS: -framework Carbon -framework CoreGraphics -framework CoreFoundation -framework AppKit

#include "ctap.h"
*/
import "C"
import "unsafe"

// KeyEvent is a keyboard event published to the keys stream.
type KeyEvent struct {
	KeyCode  uint16
	Flags    uint64
	Str      string  // e.g. "command b" or "repeat command b"
	IsRepeat bool
	EventRef uintptr // CFRetained CGEventRef; must be disposed via forwardEvent or dropEvent
}

// eventStream carries KeyEvents from the CGEventTap callback into Go.
// Sized large enough that fast typing + auto-repeat never fills it.
var eventStream = make(chan KeyEvent, 4096)

// goKeyDown is called from C for every intercepted non-modifier keyDown.
// eventPtr is a CFRetained CGEventRef; ownership passes to Go here.
//
//export goKeyDown
func goKeyDown(kc C.ushort, flags C.ulonglong, eventPtr C.uintptr_t, isRepeat C.int) {
	code := uint16(kc)
	str := formatState(uint64(flags), []uint16{code})
	if isRepeat != 0 {
		str = "repeat " + str
	}
	ev := KeyEvent{
		KeyCode:  code,
		Flags:    uint64(flags),
		Str:      str,
		IsRepeat: isRepeat != 0,
		EventRef: uintptr(eventPtr),
	}
	select {
	case eventStream <- ev:
	default:
		// Buffer full — drop and immediately release the retained event.
		C.dropEvent(eventPtr, kc)
	}
}

// forwardEvent re-posts the original CGEventRef (with PASSTHROUGH_MAGIC) so
// the target application receives it as if it were never intercepted.
func forwardEvent(ev KeyEvent) {
	C.forwardEvent(C.uintptr_t(ev.EventRef), C.ushort(ev.KeyCode))
}

// dropEvent releases the retained CGEventRef without posting it; the key
// event is fully consumed (used for matched hotkeys).
func dropEvent(ev KeyEvent) {
	C.dropEvent(C.uintptr_t(ev.EventRef), C.ushort(ev.KeyCode))
}

// frontmostApp returns the BSD process name of the current frontmost
// application (delegates to NSWorkspace in frontapp.m).
func frontmostApp() string {
	return C.GoString(C.frontmostProcName())
}

// setListenerCount tells the C tap how many goroutines are actively reading
// the keys file.  When the count is zero the tap passes all events through.
func setListenerCount(n int) {
	C.setListenerCount(C.int(n))
}

// setFilter replaces the active process-name filter.  An empty slice means
// intercept all applications.
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
