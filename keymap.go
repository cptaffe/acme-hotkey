package main

import (
	"fmt"
	"strings"
)

// CGEventFlags modifier bits (from <CGEventTypes.h>).
const (
	flagShift   uint64 = 0x00020000
	flagControl uint64 = 0x00040000
	flagOption  uint64 = 0x00080000 // Option / Alt
	flagCommand uint64 = 0x00100000
)

// modifierOrder lists modifier tokens in emission order.
// This order is fixed so patterns in hotkeys.cawk are predictable.
var modifierOrder = []struct {
	flag uint64
	name string
}{
	{flagCommand, "command"},
	{flagShift, "shift"},
	{flagControl, "control"},
	{flagOption, "option"},
}

// formatState builds the canonical key-state string from the current
// CGEventFlags and the slice of held non-modifier key codes.
// Modifiers come first, then key names, all space-separated.
func formatState(flags uint64, codes []uint16) string {
	var parts []string
	for _, m := range modifierOrder {
		if flags&m.flag != 0 {
			parts = append(parts, m.name)
		}
	}
	for _, code := range codes {
		name := keyCodeName(code)
		if name == "" {
			name = fmt.Sprintf("0x%02x", code)
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, " ")
}
