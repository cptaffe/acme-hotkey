package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"9fans.net/go/plan9"
	"9fans.net/go/plan9/srv9p"
)

// ── Session ───────────────────────────────────────────────────────────────
//
// Each read of "new" allocates one session and creates a subtree:
//
//   {id}/
//   ├── ctl     write "del\n" → remove session subtree
//   ├── keys    OREAD activates tap; Clunk deactivates; OWRITE accepts acks
//   ├── filter  read/write; current filter applied immediately on clunk
//   └── grab    read/write; keystrokes to intercept, applied on clunk

type session struct {
	id  int
	dir *srv9p.File
	// child files — retained for removal when "del" is written to ctl
	keys, filter, grab, ctl *srv9p.File

	mu          sync.Mutex
	filterNames []string // current set; protected by mu
	grabLines   []string // bound keystrokes; protected by mu
}

// grabClone returns a copy of the bound keystrokes.
func (s *session) grabClone() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.grabLines...)
}

// grabSnapshot returns the bound keystrokes, one per line, for reads.
func (s *session) grabSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.grabLines) == 0 {
		return ""
	}
	return strings.Join(s.grabLines, "\n") + "\n"
}

// applyGrab parses raw write data, stores the lines, and calls setGrabs.
func (s *session) applyGrab(data []byte) {
	lines := parseNames(data)
	s.mu.Lock()
	s.grabLines = lines
	s.mu.Unlock()
	setGrabs(lines)
}

// filterClone returns a copy of the current filter slice, safe to pass to C.
func (s *session) filterClone() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.filterNames) == 0 {
		return nil
	}
	out := make([]string, len(s.filterNames))
	copy(out, s.filterNames)
	return out
}

// filterSnapshot returns the filter as a newline-separated string for reads.
func (s *session) filterSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.filterNames) == 0 {
		return ""
	}
	return strings.Join(s.filterNames, "\n") + "\n"
}

// applyFilter parses raw write data, stores the names, and calls setFilter.
func (s *session) applyFilter(data []byte) {
	names := parseNames(data)
	s.mu.Lock()
	s.filterNames = names
	s.mu.Unlock()
	setFilter(names)
}

// del removes the session subtree from the file tree.
// Existing open fids are unaffected (9P semantics); they will naturally
// call Clunk when the client clunks them.
func (s *session) del() error {
	for _, f := range []*srv9p.File{s.keys, s.filter, s.grab, s.ctl} {
		if err := f.Remove(); err != nil {
			return err
		}
	}
	return s.dir.Remove()
}

// ── File Aux types (stored in File.Aux at creation time) ─────────────────

type fileKind int

const (
	kindKeys   fileKind = iota
	kindFilter          // nolint:deadcode
	kindCtl             // nolint:deadcode
	kindGrab
)

// sessionFile is attached to every file inside a session directory.
type sessionFile struct {
	kind fileKind
	sess *session
}

// ── Fid Aux types (stored in fid.SetAux at Open time) ────────────────────

// openedNew: fid opened on the "new" file; carries the session ID text.
type openedNew struct{ text string }

// openedKeys: fid opened on a keys file.
type openedKeys struct {
	isReader bool   // true iff opened for OREAD or ORDWR
	wbuf     []byte // ack line accumulator (writer side)
}

// openedFilter: fid opened on a filter file.
type openedFilter struct {
	snapshot string // read snapshot, captured at Open
	wbuf     []byte // write accumulator
}

// openedCtl: fid opened on a ctl file.
type openedCtl struct{ wbuf []byte }

// ── Global keys-reader exclusion lock ────────────────────────────────────

var (
	keysReaderMu   sync.Mutex
	keysReaderOpen bool
)

// acquireKeysReader spins up to 500 ms waiting for any previous reader
// to finish its cleanup, then marks the slot as taken.
//
// The wait is necessary because a rapid 9init restart races with the
// goroutine that detects the closed socket and calls releaseKeysReader.
// Without the wait, the new 9p read process would fail immediately and
// trigger the 9init crash budget.
func acquireKeysReader() error {
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		keysReaderMu.Lock()
		if !keysReaderOpen {
			keysReaderOpen = true
			keysReaderMu.Unlock()
			return nil
		}
		keysReaderMu.Unlock()
		if time.Now().After(deadline) {
			return errors.New("keys already open for reading")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// releaseKeysReader holds keysReaderMu for the entire cleanup sequence.
// The polling loop in acquireKeysReader waits for this to complete before
// proceeding — once it sees keysReaderOpen==false, setListenerCount(0)
// and drainPending have already run, guaranteeing a clean slate.
func releaseKeysReader() {
	keysReaderMu.Lock()
	setListenerCount(0)
	drainPending()
	keysReaderOpen = false // set last: polling loop exits only after clean state
	keysReaderMu.Unlock()
}

// ── Helpers ───────────────────────────────────────────────────────────────

var nextID atomic.Int64

func parseNames(data []byte) []string {
	var names []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names
}

func treeUID() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "none"
}

func newSession(tree *srv9p.Tree, uid string) *session {
	id := int(nextID.Add(1)) - 1
	s := &session{id: id}

	dir, _ := tree.Root.Create(fmt.Sprintf("%d", id), uid, plan9.DMDIR|0555, nil)
	keys, _ := dir.Create("keys", uid, 0666, &sessionFile{kindKeys, s})
	filter, _ := dir.Create("filter", uid, 0666, &sessionFile{kindFilter, s})
	grab, _ := dir.Create("grab", uid, 0666, &sessionFile{kindGrab, s})
	ctl, _ := dir.Create("ctl", uid, 0222, &sessionFile{kindCtl, s})

	// Store session pointer in dir.Aux for future reference (e.g., via Walk).
	dir.Aux = s

	s.dir = dir
	s.keys = keys
	s.filter = filter
	s.grab = grab
	s.ctl = ctl
	return s
}

// ── Server builder ────────────────────────────────────────────────────────

func buildServer() *srv9p.Server {
	uid := treeUID()
	tree := srv9p.NewTree(uid, uid, 0555, nil)

	// Static top-level files; identified by pointer in the Open callback.
	newFile, _ := tree.Root.Create("new", uid, 0444, nil)
	frontmostFile, _ := tree.Root.Create("frontmost", uid, 0444, nil)

	return &srv9p.Server{
		Tree: tree,

		// ── Open ─────────────────────────────────────────────────────────
		Open: func(ctx context.Context, fid *srv9p.Fid, mode uint8) error {
			switch fid.File() {
			case newFile:
				// Allocate a session and record the ID text for Read.
				s := newSession(tree, uid)
				fid.SetAux(&openedNew{text: fmt.Sprintf("%d\n", s.id)})

			case frontmostFile:
				// Snapshot the frontmost app at open time.
				fid.SetAux(frontmostApp() + "\n")

			default:
				sf, ok := fid.File().Aux.(*sessionFile)
				if !ok {
					break // root, session dir — nothing extra needed
				}
				m := mode & 3
				switch sf.kind {
				case kindKeys:
					if m == plan9.OREAD || m == plan9.ORDWR {
						if err := acquireKeysReader(); err != nil {
							return err
						}
						fid.SetAux(&openedKeys{isReader: true})
						setListenerCount(1)
						// Apply this session's filter so the tap captures
						// the right processes from the very first event.
						setFilter(sf.sess.filterClone())
						setGrabs(sf.sess.grabClone())
					} else {
						fid.SetAux(&openedKeys{isReader: false})
					}

				case kindFilter, kindGrab:
					of := &openedFilter{}
					if m == plan9.OREAD || m == plan9.ORDWR {
						if sf.kind == kindGrab {
							of.snapshot = sf.sess.grabSnapshot()
						} else {
							of.snapshot = sf.sess.filterSnapshot()
						}
					}
					fid.SetAux(of)

				case kindCtl:
					fid.SetAux(&openedCtl{})
				}
			}
			return nil
		},

		// ── Read ──────────────────────────────────────────────────────────
		//
		// Directory reads (root and session dirs) are served by srv9p's
		// built-in dirReader and never reach this callback.
		Read: func(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
			switch aux := fid.Aux().(type) {
			case *openedNew:
				return fid.ReadString(data, offset, aux.text)

			case string: // frontmost snapshot
				return fid.ReadString(data, offset, aux)

			case *openedKeys:
				if !aux.isReader {
					return 0, errors.New("not open for reading")
				}
				// Block until a key event arrives or the request is flushed.
				select {
				case ev, ok := <-eventStream:
					if !ok {
						return 0, errors.New("event stream closed")
					}
					// Guard against the race where both eventStream and
					// ctx.Done() become ready simultaneously: if already
					// flushed, drop the event so the C buffer stays in sync.
					select {
					case <-ctx.Done():
						ackNext(false)
						return 0, nil
					default:
					}
					return copy(data, ev.Str+"\n"), nil
				case <-ctx.Done():
					return 0, nil
				}

			case *openedFilter:
				return fid.ReadString(data, offset, aux.snapshot)
			}
			return 0, nil
		},

		// ── Write ─────────────────────────────────────────────────────────
		Write: func(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
			sf, ok := fid.File().Aux.(*sessionFile)
			if !ok {
				return 0, errors.New("not writable")
			}
			switch sf.kind {
			case kindKeys:
				// Ack lines: "!…" → drop CGEvent, "…" → forward it.
				kf, ok := fid.Aux().(*openedKeys)
				if !ok {
					return 0, errors.New("keys: not open")
				}
				kf.wbuf = append(kf.wbuf, data...)
				for {
					nl := bytes.IndexByte(kf.wbuf, '\n')
					if nl < 0 {
						break
					}
					line := string(kf.wbuf[:nl])
					kf.wbuf = kf.wbuf[nl+1:]
					if line != "" {
						ackNext(!strings.HasPrefix(line, "!"))
					}
				}

			case kindFilter, kindGrab:
				// Accumulate writes by offset (like an io.WriterAt).
				of, ok := fid.Aux().(*openedFilter)
				if !ok {
					return 0, errors.New("filter: not open")
				}
				end := int(offset) + len(data)
				if end > len(of.wbuf) {
					of.wbuf = append(of.wbuf, make([]byte, end-len(of.wbuf))...)
				}
				copy(of.wbuf[offset:], data)

			case kindCtl:
				// Process complete lines immediately so "del" takes effect
				// while the ctl fid is still open.
				cf, ok := fid.Aux().(*openedCtl)
				if !ok {
					return 0, errors.New("ctl: not open")
				}
				cf.wbuf = append(cf.wbuf, data...)
				for {
					nl := bytes.IndexByte(cf.wbuf, '\n')
					if nl < 0 {
						break
					}
					cmd := strings.TrimSpace(string(cf.wbuf[:nl]))
					cf.wbuf = cf.wbuf[nl+1:]
					switch cmd {
					case "del":
						if err := sf.sess.del(); err != nil {
							return 0, fmt.Errorf("del: %w", err)
						}
					case "":
						// ignore blank lines
					default:
						return 0, fmt.Errorf("unknown ctl command: %q", cmd)
					}
				}
			}
			return len(data), nil
		},

		// ── Clunk ─────────────────────────────────────────────────────────
		Clunk: func(fid *srv9p.Fid) {
			switch aux := fid.Aux().(type) {
			case *openedKeys:
				if aux.isReader {
					releaseKeysReader()
				}

			case *openedFilter:
				// Apply the accumulated filter content when the write side
				// closes.  Filter is independent of the tap lifecycle: it
				// is not cleared on keys Clunk, only replaced here or on
				// the next keys Open.
				if len(aux.wbuf) > 0 {
					if sf, ok := fid.File().Aux.(*sessionFile); ok {
						if sf.kind == kindGrab {
							sf.sess.applyGrab(aux.wbuf)
						} else {
							sf.sess.applyFilter(aux.wbuf)
						}
					}
				}
			}
		},
	}
}
