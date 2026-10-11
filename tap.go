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

var keyNames = map[string]bool{}

// initKeys must run before rules are loaded, which name keys.
func initKeys() {
	C.initKeyNames()
	for code := 0; code < 256; code++ {
		if name := keyCodeName(uint16(code)); name != "" {
			keyNames[name] = true
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
	name := keyCodeName(code)
	if name == "" {
		name = fmt.Sprintf("0x%02x", code)
	}
	return hotkey.Key(mods, name)
}

func keyCodeName(code uint16) string {
	name := C.virtualKeyName(C.ushort(code))
	if name == nil {
		return ""
	}
	return C.GoString(name)
}

func knownKey(name string) error {
	if !keyNames[name] {
		return fmt.Errorf("no such key on this keyboard layout")
	}
	return nil
}
