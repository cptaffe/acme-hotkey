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

// macOS virtual key codes from HIToolbox/Events.h.
var keyCodeNames = map[uint16]string{
	// ANSI letters
	0x00: "a", 0x01: "s", 0x02: "d", 0x03: "f",
	0x04: "h", 0x05: "g", 0x06: "z", 0x07: "x",
	0x08: "c", 0x09: "v", 0x0B: "b", 0x0C: "q",
	0x0D: "w", 0x0E: "e", 0x0F: "r", 0x10: "y",
	0x11: "t", 0x12: "1", 0x13: "2", 0x14: "3",
	0x15: "4", 0x16: "6", 0x17: "5", 0x18: "equal",
	0x19: "9", 0x1A: "7", 0x1B: "minus", 0x1C: "8",
	0x1D: "0", 0x1E: "rightbracket", 0x1F: "o",
	0x20: "u", 0x21: "leftbracket", 0x22: "i", 0x23: "p",
	// Special keys
	0x24: "return",
	0x25: "l", 0x26: "j", 0x27: "quote",
	0x28: "k", 0x29: "semicolon", 0x2A: "backslash", 0x2B: "comma",
	0x2C: "slash", 0x2D: "n", 0x2E: "m", 0x2F: "period",
	0x30: "tab", 0x31: "space", 0x32: "grave", 0x33: "delete",
	0x35: "escape",
	// Keypad
	0x41: "kp-period", 0x43: "kp-multiply", 0x45: "kp-plus",
	0x47: "kp-clear", 0x4B: "kp-divide", 0x4C: "kp-enter",
	0x4E: "kp-minus", 0x51: "kp-equal",
	0x52: "kp-0", 0x53: "kp-1", 0x54: "kp-2",
	0x55: "kp-3", 0x56: "kp-4", 0x57: "kp-5",
	0x58: "kp-6", 0x59: "kp-7", 0x5B: "kp-8", 0x5C: "kp-9",
	// Function keys
	0x60: "f5", 0x61: "f6", 0x62: "f7", 0x63: "f3",
	0x64: "f8", 0x65: "f9", 0x67: "f11", 0x69: "f13",
	0x6A: "f16", 0x6B: "f14", 0x6D: "f10", 0x6F: "f12",
	0x71: "f15", 0x76: "f4", 0x78: "f2", 0x7A: "f1",
	// Navigation / editing
	0x72: "help", 0x73: "home", 0x74: "pageup",
	0x75: "forwarddelete", 0x77: "end", 0x79: "pagedown",
	// Arrows
	0x7B: "left", 0x7C: "right", 0x7D: "down", 0x7E: "up",
}

// nameToKeyCode is the reverse of keyCodeNames, built at init time.
var nameToKeyCode map[string]uint16

func init() {
	nameToKeyCode = make(map[string]uint16, len(keyCodeNames))
	for code, name := range keyCodeNames {
		nameToKeyCode[name] = code
	}
}

// modifierNames lists modifier tokens in emission order: most common first.
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
// CGEventFlags and the slice of held non-modifier key codes (sorted by code).
// Modifiers come first, then key names, all space-separated.
// An empty string means nothing is held.
func formatState(flags uint64, codes []uint16) string {
	var parts []string
	for _, m := range modifierOrder {
		if flags&m.flag != 0 {
			parts = append(parts, m.name)
		}
	}
	for _, code := range codes {
		name, ok := keyCodeNames[code]
		if !ok {
			name = fmt.Sprintf("0x%02x", code)
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, " ")
}

// parseKeyEvent parses a key-state string such as "command s" or
// "command shift a" into a KeyEvent suitable for re-injection.
// The last token must be a non-modifier key name.
func parseKeyEvent(s string) (KeyEvent, bool) {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return KeyEvent{}, false
	}
	var ev KeyEvent
	for _, tok := range parts[:len(parts)-1] {
		switch tok {
		case "control", "ctrl":
			ev.Flags |= flagControl
		case "option", "alt":
			ev.Flags |= flagOption
		case "shift":
			ev.Flags |= flagShift
		case "command", "cmd":
			ev.Flags |= flagCommand
		default:
			return KeyEvent{}, false
		}
	}
	keyName := parts[len(parts)-1]
	code, ok := nameToKeyCode[keyName]
	if !ok {
		return KeyEvent{}, false
	}
	ev.KeyCode = code
	ev.Str = s
	return ev, true
}
