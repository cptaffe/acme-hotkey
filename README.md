# acme-hotkey

A hotkey daemon for [acme](https://9fans.github.io/plan9port/man/man1/acme.html) on macOS.

It sits between the keyboard and Acme at the HID layer (via `CGEventTap`),
intercepts every keystroke, and exposes them through a **9P file server** as a
single file called `keys`.  The protocol mirrors Acme's own event file:

- **Reading** `keys` blocks until a keystroke arrives, then returns one line:

  ```
  control+b
  command+shift+a
  a
  space
  option+right
  ```

- **Writing** a key string back to `keys` re-injects the keystroke into the
  event stream so the focused application (Acme, or any other) receives it as
  if it were typed normally.

Keystrokes that are neither re-injected nor otherwise consumed are swallowed.

---

## Requirements

| Requirement | Notes |
|-------------|-------|
| macOS 12+ | `CGEventTap` at the HID level |
| [plan9port](https://9fans.github.io/plan9port/) | for `9p`, `9pfuse`, `$NAMESPACE` |
| Go 1.21+ | CGo is required (Xcode command-line tools) |
| Accessibility permission | System Settings → Privacy & Security → Accessibility |

---

## Building

```sh
git clone ...
cd acme-hotkey
go build -o acme-hotkey .
```

---

## Running

```sh
./acme-hotkey &
```

The daemon prints its socket path on startup:

```
acme-hotkey: listening on /tmp/ns.alice.:0/acme-hotkey
acme-hotkey: 9p -a unix!/tmp/ns.alice.:0/acme-hotkey read keys
```

It posts itself to `$NAMESPACE/acme-hotkey`, the same directory Acme and
other plan9port services use, so tools that speak 9P can find it by name.

---

## Accessing the keys file

### Direct read (stream keystrokes to stdout)

```sh
9p -a unix!$NAMESPACE/acme-hotkey read keys
```

### Mounting with 9pfuse

```sh
9pfuse unix!$NAMESPACE/acme-hotkey ~/mnt/acme-hotkey
# now ordinary file access works:
cat ~/mnt/acme-hotkey/keys
```

Once mounted, the usage from the top of this document works verbatim:

```sh
9p read acme-hotkey/keys       # if mounted at ./acme-hotkey
```

---

## Key string format

```
[modifier+]...[modifier+]keyname
```

Modifiers (in canonical order): `control`, `option`, `shift`, `command`

Key names match `HIToolbox/Events.h` virtual key codes:

| Category | Examples |
|----------|---------|
| Letters | `a` … `z` |
| Digits | `0` … `9` |
| Punctuation | `minus`, `equal`, `leftbracket`, `rightbracket`, `backslash`, `semicolon`, `quote`, `grave`, `comma`, `period`, `slash` |
| Whitespace / editing | `space`, `tab`, `return`, `delete`, `forwarddelete`, `escape` |
| Navigation | `up`, `down`, `left`, `right`, `home`, `end`, `pageup`, `pagedown` |
| Function | `f1` … `f16` |
| Keypad | `kp-0` … `kp-9`, `kp-plus`, `kp-minus`, `kp-multiply`, `kp-divide`, `kp-enter`, `kp-period`, `kp-equal`, `kp-clear` |

Examples: `control+b`, `command+shift+a`, `option+right`, `f5`

---

## Writing back (pass-through)

Write the same key string (plus a newline) to re-inject the keystroke:

```sh
printf 'control+b\n' | 9p -a unix!$NAMESPACE/acme-hotkey write keys
```

Multiple keys can be written in a single transaction, one per line.

---

## Building a key filter

The typical use case is an intermediary process that reads `keys`, decides
whether to act on a keystroke or pass it to Acme, and writes it back when
it should be forwarded:

```sh
#!/bin/sh
# Example: intercept command+b to bold the selection, forward everything else.
9p -a unix!$NAMESPACE/acme-hotkey read keys | while IFS= read -r key; do
    case "$key" in
    command+b)
        # handle locally — do not write back
        my-bold-command
        ;;
    *)
        # forward to Acme
        printf '%s\n' "$key" | 9p -a unix!$NAMESPACE/acme-hotkey write keys
        ;;
    esac
done
```

Multiple readers each get their own independent stream of every keystroke —
the broker fans out to all subscribers, so a pipeline of filters is possible.

---

## How it works

```
Physical key press
      │
      ▼
CGEventTap  (kCGHIDEventTap — lowest macOS level)
      │  swallows event; calls goKeyEvent()
      ▼
keyEventChan  (Go buffered channel, 256 events)
      │
      ▼
KeyBroker.publish()
      │  fans out to every open keys-fid subscription channel
      ▼
9P Tread goroutine  (one per blocking read; cancelled by Tflush)
      │  sends  "keyname\n"  as Rread data
      ▼
9p / cat / custom reader

               ── write path ──

Client writes "keyname\n" to keys fid
      │
      ▼
doWrite() → parseKeyEvent() → reInjectKey()
      │
      ▼
CGEventPost(kCGAnnotatedSessionEventTap, …)
      │  posted above the HID tap, so our tap never sees it
      ▼
Focused application (Acme, Terminal, …)
```

### Why `kCGAnnotatedSessionEventTap` for re-injection?

Events posted at `kCGAnnotatedSessionEventTap` enter the event pipeline
*after* the HID-level tap, so our own tap never re-intercepts them.  This
avoids infinite loops without needing a sentinel marker in the event.

### Tflush handling

9P clients (e.g. `9p read` interrupted with ^C) send `Tflush` to cancel an
outstanding `Tread`.  Each blocking read goroutine holds a `context.Context`;
`doFlush` calls the matching `cancel()`, the goroutine exits without sending
`Rread`, and `Rflush` is sent before the goroutine finishes — satisfying the
9P ordering requirement.
