# acme-hotkey

A hotkey daemon for [acme](https://9fans.github.io/plan9port/man/man1/acme.html) on macOS.

It sits at the HID layer (via `CGEventTap`), intercepts keystrokes, and
exposes them through a **9P file server** using Acme's event-file protocol:
read a `keys` file to receive events; write back to forward or drop them.

---

## Requirements

| Requirement | Notes |
|-------------|-------|
| macOS 12+ | `CGEventTap` / `CGEventPost` |
| [plan9port](https://9fans.github.io/plan9port/) | `9p`, `9pserve`, `$NAMESPACE` |
| Go 1.21+ | CGo required (Xcode command-line tools) |
| Accessibility permission | System Settings → Privacy & Security → Accessibility |

---

## Building & installing

```sh
make install   # go install + codesign (required for TCC Accessibility identity)
```

Running plain `go build` or `go install` resets the code-signing identity and
macOS revokes the Accessibility permission.  Always use `make install`.

---

## Running

```sh
acme-hotkey &
# or via 9init:  9p write init/ctl <<<'start acme-hotkey'
```

The daemon posts itself to `$NAMESPACE/acme-hotkey` via `9pserve`, so any
plan9port tool can reach it by name without `-a unix!…` flags.

---

## File system layout

```
acme-hotkey/
├── new           read → "{id}\n"  (creates a session)
├── frontmost     read → current frontmost process name
└── {id}/
    ├── ctl       write "del\n" → remove session subtree
    ├── keys      OREAD delivers events; OWRITE accepts ack lines
    └── filter    read/write; newline-separated process name allowlist
```

### Sessions

Reading `new` allocates a session and returns its numeric ID:

```sh
id=$(9p read acme-hotkey/new)    # e.g. "0"
```

Each session has its own `keys`, `filter`, and `ctl` files under `$id/`.
Only one `keys` reader may be active at a time across all sessions.

Remove a session by writing `del` to its `ctl` file:

```sh
echo del | 9p write acme-hotkey/$id/ctl
```

---

## Event format

Each line delivered by `keys` is space-separated: modifiers first (in
canonical order), then the key name.

```
command b
command shift s
escape
option right
```

**Modifier order:** `command`, `shift`, `control`, `option`

**Key names** are derived from the current keyboard layout via
`UCKeyTranslate` — symbols appear as their actual characters:

| Category | Examples |
|----------|---------|
| Letters | `a` … `z` |
| Digits | `0` … `9` |
| Symbols | `; / \ = - [ ] ' `` , .` |
| Editing | `space`, `tab`, `return`, `delete`, `forwarddelete`, `escape` |
| Navigation | `up`, `down`, `left`, `right`, `home`, `end`, `pageup`, `pagedown` |
| Function | `f1` … `f16` |
| Keypad | same character as non-keypad equivalent (`0`–`9`, `*`, `+`, `-`, `/`, `=`, `.`); `clear`, `return` |

---

## Ack protocol

`keys` works like Acme's event file.  After the pipeline processes an event,
write the (possibly modified) line back to forward it to the application, or
prefix it with `!` to drop it:

```
command b\n    →  write back "command b\n"  to forward
                  write back "!command b\n" to consume
```

Events that are neither forwarded nor dropped are swallowed.

---

## Filtering by process

Writing newline-separated process names to `filter` restricts interception
to those processes.  An empty filter (the default) intercepts every process.

```sh
printf 'Acme\nTerminal\n' | 9p write acme-hotkey/$id/filter
```

The filter is applied when the `filter` fid is closed.  It is independent
of the tap: setting the filter does not require closing or reopening `keys`.

---

## Typical usage (cawk pipeline)

```sh
id=$(9p read acme-hotkey/new)
printf 'Acme\n' | 9p write acme-hotkey/$id/filter
9p read acme-hotkey/$id/keys \
    | cawk -f ~/src/acme-hotkey/hotkeys.cawk \
    | 9p write acme-hotkey/$id/keys
```

`hotkeys.cawk` matches event lines and either handles the hotkey locally
(printing `!event\n` to drop the keystroke) or forwards it unchanged.

---

## frontmost

Reading `frontmost` returns the process name of the frontmost application
at the moment of the read.  This is a snapshot — it does not block or update.

```sh
9p read acme-hotkey/frontmost   # e.g. "Acme\n"
```

---

## How it works

```
Physical key press
      │
      ▼
CGEventTap  (kCGTailAppendEventTap on kCGSessionEventTap)
      │  filters by process (filter file) and listener count
      │  swallows event; CFRetains it into gPending circular buffer
      │  calls goKeyDown() → publishes to Go eventStream channel
      ▼
9P Read on keys  (blocks in select{} until event or Tflush)
      │  returns "modifiers keyname\n"
      ▼
cawk pipeline  (pattern-matches, runs acme commands, passes through)
      │
      ▼
9P Write on keys  ("!line\n" → drop  /  "line\n" → forward)
      │
      ▼
ackNext()  →  CGEventPost(kCGAnnotatedSessionEventTap, event)
                 delivered directly to target app, bypassing our tap
```

### Key design choices

**`kCGTailAppendEventTap`** — inserted at the end of the session-level tap
chain, after Mission Control's head-insert tap.  Ctrl+arrow (Spaces) and
other ctrl shortcuts are consumed by Mission Control before we see them;
we additionally pass ctrl+key combos through unconditionally.

**`kCGAnnotatedSessionEventTap` for re-injection** — avoids re-entering
the session-level tap chain (no lag, no infinite loop).

**Circular buffer (gPending)** — the CGEventRef is retained in C until the
write-back ack arrives.  Go only carries the string representation.

**Tflush handling** — each blocking `Read` runs with a `context.Context`
derived from the 9P request.  When a Tflush arrives, `srv9p` cancels the
context; the `select` exits, and the in-flight CGEvent is dropped via
`ackNext(false)` to keep the buffer in sync.

**Single keys reader** — enforced by a global mutex with a 500 ms polling
window to tolerate rapid pipeline restarts (9init crash-restart race).
