package main

/*
#cgo LDFLAGS: -framework Carbon -framework CoreGraphics -framework CoreFoundation

#include "ctap.h"
*/
import "C"
import (
	"fmt"

	"github.com/cptaffe/automaton/seq/hotkey"
)

var modFlags = []struct {
	flag uint64
	name string
}{
	{0x100000, "command"},
	{0x020000, "shift"},
	{0x040000, "control"},
	{0x080000, "option"},
}

var keyboard hotkey.Keyboard

// initKeys must run before rules are loaded, which name keys.
func initKeys() {
	C.initKeyNames()
	seen := map[string]bool{}
	for code := range 256 {
		if name := keyName(uint16(code)); !seen[name] {
			seen[name] = true
			keyboard = append(keyboard, name)
		}
	}
}

func startTap() { C.startEventTap() }

// goKey decides, while the tap waits, whether to consume a key-down.
//
//export goKey
func goKey(kc C.ushort, flags C.ulonglong, app *C.char) C.int {
	if theHub.key(keyOf(uint16(kc), uint64(flags)), C.GoString(app)) {
		return 1
	}
	return 0
}

func keyOf(code uint16, flags uint64) string {
	var mods []string
	for _, m := range modFlags {
		if flags&m.flag != 0 {
			mods = append(mods, m.name)
		}
	}
	return hotkey.Key(mods, keyName(code))
}

func keyName(code uint16) string {
	if name := keyCodeName(code); name != "" {
		return name
	}
	return fmt.Sprintf("0x%02x", code)
}

func keyCodeName(code uint16) string {
	name := C.virtualKeyName(C.ushort(code))
	if name == nil {
		return ""
	}
	return C.GoString(name)
}
