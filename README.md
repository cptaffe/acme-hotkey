# acme-hotkey

Keyboard shortcuts for [acme](https://9fans.github.io/plan9port/man/man1/acme.html)
on macOS, built the way the
[plumber](https://9fans.github.io/plan9port/man/man4/plumber.html) is.

A `CGEventTap` hands each keystroke to the daemon, which matches it against a
rules file while the tap waits.  A key no rule wants is left alone, so the
system and the application see it as if acme-hotkey were not there.  A key a
rule wants is consumed, and the rule's rc script runs.

---

## Requirements

| Requirement | Notes |
|-------------|-------|
| macOS 12+ | `CGEventTap` |
| [plan9port](https://9fans.github.io/plan9port/) | `9p`, `9pserve`, `$NAMESPACE` |
| Go 1.24+ | cgo (Xcode command-line tools) |
| Accessibility permission | System Settings → Privacy & Security → Accessibility |

---

## Building and installing

```sh
make install   # go install + codesign (required for TCC Accessibility identity)
```

Running plain `go build` or `go install` resets the code-signing identity and
macOS revokes the Accessibility permission.  Always use `make install`.

---

## Running

```sh
acme-hotkey [-rules file]   # default ~/lib/hotkeys
```

The daemon posts itself to `$NAMESPACE/acme-hotkey`.

---

## Rules

Rules are written in the hotkey dialect of
[seq](https://github.com/cptaffe/automaton), and run as rc scripts:

```
# Edit ,>9p write acme-hotkey/rules
%{
fn ctl { echo $1 | 9p write acme/`{9p read acme-focused/focus}/ctl }
%}
%delay settle 1s

main:
	-> acme when app == "devdraw"

acme:
	command-s		{ ctl put }	# save
	control-x control-s	{ ctl put }
	control-x settle	{ ctl show }
	control-x / any	{ ctl show }
	command-g		{ ctl nomark } -> group

group:
	-> acme
	escape			{ ctl mark } -> main
```

- **Scopes.** A line `name:` starts a scope.  Matching starts in the
  first, and returns to the current scope after each match.
- **Includes.** `-> scope` on a line of its own reads that scope's rules
  in, optionally with `when` and an expression over `app`, the process
  the key is headed for: `==`, `!=`, `~` (a regular expression), `&&`,
  `||`, `!`, and parentheses.  A key is taken only when a rule that holds
  binds it; every other key passes through.
- **Rules.** Keys, an rc block in braces, and optionally `-> scope`, which
  makes that scope current until another rule leaves it.  A key is the
  name the layout gives it after any of `command-`, `shift-`, `control-`,
  and `option-`, in any order.  Several keys make a sequence.  A comment
  after a rule is its label.  Blocks may span lines and nest.
- **The prelude** between `%{` and `%}` is prepended to every block.  Each
  block runs in its own rc with `$app` and `$key` set.
- **Delays** are named: `%delay name duration`, and a delay's name in a
  pattern is read when the machine has waited that long, so
  `control-x settle` fires once a second passes with nothing after
  `control-x`.  A rule that is also the start of a longer one is an error
  unless it ends this way, as are two rules on the same keys.
- **Patterns** are expressions over keys, as in a lexer: keys in
  sequence, `|` between alternatives, `( )`, and `*`, `+` or `?` after a
  key or group.  `\` takes one set of keys from another, as in
  `any \ escape`, while a `-` only joins the parts of a chord.  A key
  that is only punctuation is quoted, as in `"/"`.
- **`any`** reads a key no other rule names at that point, and `/` ends
  a pattern with a key it reads without taking, so `control-x / any`
  fires at once and lets the next key do what it would have.  An empty
  block takes a key and does nothing; `... any { }` keeps every key in a
  scope from the application.
- **Order.** When two rules that hold bind the same keys, the one written
  first takes them.
- **System shortcuts** (Spotlight, screenshots, and any other enabled
  symbolic hotkey) always pass through.

## File system

```
acme-hotkey/
├── rules   read: the rules; write: replace them
├── card    read: every binding, one per line: scope, keys, action, label
└── send    write: "app key" lines, matched as if the key were pressed
```

As with `plumb/rules`, a write to `rules` is checked as it arrives, so an
error comes back on the write with its line number, and nothing is
installed.  The rules are replaced when the file is closed:

```sh
9p write acme-hotkey/rules <$home/lib/hotkeys
```

```sh
echo 'devdraw command-s' | 9p write acme-hotkey/send
```

---

## How it works

- The tap is appended to the end of the session tap chain, so shortcuts the
  Dock's Mission Control tap consumes never reach it.
- For each key-down that is not a modifier, an auto-repeat or a system
  shortcut, the tap calls into Go with the key and the target process.  Go
  steps the compiled rules and answers consume or pass before the callback
  returns.  A consumed key's auto-repeats and key-up are consumed too.
- The rules compile to one table with seq.  A sequence in progress waits on
  the delays its state names; the daemon runs a timer for each.
- Nothing is held or re-posted: a passed key is the original event.
