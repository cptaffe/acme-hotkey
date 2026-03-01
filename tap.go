package main

/*
#cgo LDFLAGS: -framework Carbon -framework CoreGraphics -framework CoreFoundation -framework AppKit

#include "ctap.h"
*/
import "C"
import "unsafe"

// KeyEvent is a keyboard event published to the keys stream.
// The original CGEventRef lives entirely in C's circular buffer; Go never
// holds it.
type KeyEvent struct {
	KeyCode  uint16
	Flags    uint64
	Str      string // e.g. "command b" or "repeat command b"
	IsRepeat bool
}

// eventStream carries KeyEvents from the CGEventTap callback into Go.
var eventStream = make(chan KeyEvent, 4096)

// goKeyDown is called from C for every intercepted non-modifier keyDown.
// The corresponding CGEventRef has already been CFRetain'd and stored in
// C's circular buffer; Go just receives the metadata needed to forward
// the event string to the cawk pipeline.
//
//export goKeyDown
func goKeyDown(kc C.ushort, flags C.ulonglong, isRepeat C.int) {
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
	}
	select {
	case eventStream <- ev:
	default:
		// Buffer full — drop: advance the C buffer tail immediately.
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
