package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"9fans.net/go/plan9"
)

// ---------------------------------------------------------------------------
// QID path constants — we only have two nodes in the tree.
// ---------------------------------------------------------------------------

const (
	ftRoot   = uint64(0)
	ftKeys   = uint64(1)
	ftFilter = uint64(2)
)

func makeQID(ft uint64) plan9.Qid {
	qt := uint8(plan9.QTFILE)
	if ft == ftRoot {
		qt = plan9.QTDIR
	}
	return plan9.Qid{Type: qt, Path: ft}
}

func makeDir(ft uint64) plan9.Dir {
	now := uint32(time.Now().Unix())
	switch ft {
	case ftRoot:
		return plan9.Dir{
			Qid:   makeQID(ftRoot),
			Mode:  plan9.DMDIR | 0555,
			Atime: now, Mtime: now,
			Name: "/", Uid: "none", Gid: "none", Muid: "none",
		}
	case ftKeys:
		return plan9.Dir{
			Qid:   makeQID(ftKeys),
			Mode:  0666,
			Atime: now, Mtime: now,
			Name: "keys", Uid: "none", Gid: "none", Muid: "none",
		}
	case ftFilter:
		return plan9.Dir{
			Qid:   makeQID(ftFilter),
			Mode:  0666,
			Atime: now, Mtime: now,
			Name: "filter", Uid: "none", Gid: "none", Muid: "none",
		}
	}
	panic("makeDir: unknown ft")
}

// dirContent returns the marshalled Dir entries for the root directory listing.
func dirContent() []byte {
	var out []byte
	for _, ft := range []uint64{ftKeys, ftFilter} {
		d := makeDir(ft)
		b, _ := d.Bytes()
		out = append(out, b...)
	}
	return out
}

// ---------------------------------------------------------------------------
// KeyBroker — fan-out from the tap goroutine to all open read fids.
// ---------------------------------------------------------------------------

// KeyBroker distributes KeyEvents to all current OREAD subscribers.
type KeyBroker struct {
	mu   sync.Mutex
	subs map[uint64]chan KeyEvent
	next uint64
}

func newKeyBroker() *KeyBroker {
	return &KeyBroker{subs: make(map[uint64]chan KeyEvent)}
}

// subscribe creates a new subscription and returns its numeric ID and
// the receive-only channel.  Callers must eventually call unsubscribe.
func (b *KeyBroker) subscribe() (id uint64, ch <-chan KeyEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	id = b.next
	c := make(chan KeyEvent, 64)
	b.subs[id] = c
	return id, c
}

// unsubscribe cancels the subscription and closes the channel so any
// goroutine blocked on it wakes up and can detect the closure.
func (b *KeyBroker) unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		close(ch)
		delete(b.subs, id)
	}
}

// publish sends ev to every current subscriber, dropping events for
// subscribers whose buffer is full.
func (b *KeyBroker) publish(ev KeyEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// 9P server
// ---------------------------------------------------------------------------

// Server holds shared state across all connections.
type Server struct {
	broker *KeyBroker

	// filterMu protects filterProcs, the Go-side mirror of the C filter.
	filterMu    sync.RWMutex
	filterProcs []string
}

func newServer(broker *KeyBroker) *Server {
	return &Server{broker: broker}
}

// filterSnapshot returns a newline-terminated snapshot of the current filter.
func (s *Server) filterSnapshot() []byte {
	s.filterMu.RLock()
	defer s.filterMu.RUnlock()
	if len(s.filterProcs) == 0 {
		return nil
	}
	return []byte(strings.Join(s.filterProcs, "\n") + "\n")
}

// applyFilter parses newline-separated process names from data, updates the
// Go-side state, and pushes the change down to the C event tap.
func (s *Server) applyFilter(data []byte) {
	var names []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	s.filterMu.Lock()
	s.filterProcs = names
	s.filterMu.Unlock()
	setFilter(names) // pushes to C
}

// ---------------------------------------------------------------------------
// Per-connection state
// ---------------------------------------------------------------------------

// fid is the per-fid state, mirroring acme-styles' fid struct.
type fid struct {
	ft   uint64
	open bool
	mode uint8

	// Buffered content for directory reads (set at Topen).
	buf []byte

	// Accumulated write bytes for the keys file.
	wbuf []byte

	// Non-nil when the keys file is opened OREAD / ORDWR.
	subID uint64
	keyCh <-chan KeyEvent
}

// conn is the per-connection state, mirroring acme-styles' conn struct.
type conn struct {
	srv   *Server
	c     net.Conn
	fids  map[uint32]*fid
	msize uint32

	// Pending blocking Treads: tag → cancel function.
	// Protected by pendMu; goroutines spawned by doRead update this.
	pendMu  sync.Mutex
	pending map[uint16]context.CancelFunc

	// Serialises all writes to c so goroutines don't race.
	wmu sync.Mutex
}

// ---------------------------------------------------------------------------
// Connection handler
// ---------------------------------------------------------------------------

func (s *Server) handleConn(c net.Conn) {
	defer c.Close()
	cn := &conn{
		srv:     s,
		c:       c,
		fids:    make(map[uint32]*fid),
		msize:   8192 + plan9.IOHDRSZ,
		pending: make(map[uint16]context.CancelFunc),
	}
	for {
		fc, err := plan9.ReadFcall(c)
		if err != nil {
			cn.cancelAll()
			return
		}
		cn.dispatch(fc)
	}
}

// send writes a response, serialised via wmu.
func (cn *conn) send(resp *plan9.Fcall) {
	cn.wmu.Lock()
	defer cn.wmu.Unlock()
	plan9.WriteFcall(cn.c, resp) //nolint:errcheck
}

func rerr(tag uint16, msg string) *plan9.Fcall {
	return &plan9.Fcall{Type: plan9.Rerror, Tag: tag, Ename: msg}
}

// cancelAll cancels any in-flight blocking reads; called on disconnect.
func (cn *conn) cancelAll() {
	cn.pendMu.Lock()
	defer cn.pendMu.Unlock()
	for _, cancel := range cn.pending {
		cancel()
	}
}

// dispatch routes an incoming Fcall.  It follows the same structure as
// acme-styles; the only addition is that Tread on the keys file spawns a
// goroutine so Tflush can arrive while a read is outstanding.
func (cn *conn) dispatch(fc *plan9.Fcall) {
	switch fc.Type {
	case plan9.Tversion:
		cn.send(cn.doVersion(fc))
	case plan9.Tauth:
		cn.send(rerr(fc.Tag, "no authentication required"))
	case plan9.Tattach:
		cn.send(cn.doAttach(fc))
	case plan9.Tflush:
		cn.doFlush(fc)
	case plan9.Twalk:
		cn.send(cn.doWalk(fc))
	case plan9.Topen:
		cn.send(cn.doOpen(fc))
	case plan9.Tcreate:
		cn.send(rerr(fc.Tag, "create not supported"))
	case plan9.Tread:
		cn.doRead(fc) // may be async; does its own cn.send
	case plan9.Twrite:
		cn.send(cn.doWrite(fc))
	case plan9.Tclunk:
		cn.send(cn.doClunk(fc))
	case plan9.Tremove:
		cn.send(rerr(fc.Tag, "remove not supported"))
	case plan9.Tstat:
		cn.send(cn.doStat(fc))
	case plan9.Twstat:
		cn.send(rerr(fc.Tag, "wstat not supported"))
	default:
		cn.send(rerr(fc.Tag, "unknown message type"))
	}
}

// ---------------------------------------------------------------------------
// Individual message handlers
// ---------------------------------------------------------------------------

func (cn *conn) doVersion(fc *plan9.Fcall) *plan9.Fcall {
	msize := fc.Msize
	if msize > cn.msize {
		msize = cn.msize
	}
	cn.msize = msize
	cn.fids = make(map[uint32]*fid) // reset state on Tversion
	ver := "9P2000"
	if !strings.HasPrefix(fc.Version, "9P2000") {
		ver = "unknown"
	}
	return &plan9.Fcall{Type: plan9.Rversion, Tag: fc.Tag, Msize: msize, Version: ver}
}

func (cn *conn) doAttach(fc *plan9.Fcall) *plan9.Fcall {
	cn.fids[fc.Fid] = &fid{ft: ftRoot}
	return &plan9.Fcall{Type: plan9.Rattach, Tag: fc.Tag, Qid: makeQID(ftRoot)}
}

// doFlush cancels any pending blocking read with the given tag, then
// replies immediately.  The blocked goroutine will see ctx.Done() and exit
// without sending an Rread, satisfying the 9P rule that Rflush must arrive
// before any response to the flushed tag.
func (cn *conn) doFlush(fc *plan9.Fcall) {
	cn.pendMu.Lock()
	if cancel, ok := cn.pending[fc.Oldtag]; ok {
		cancel()
		delete(cn.pending, fc.Oldtag)
	}
	cn.pendMu.Unlock()
	cn.send(&plan9.Fcall{Type: plan9.Rflush, Tag: fc.Tag})
}

func (cn *conn) doWalk(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	if f.open {
		return rerr(fc.Tag, "fid is open")
	}

	curFt := f.ft
	wqids := make([]plan9.Qid, 0, len(fc.Wname))

	for i, name := range fc.Wname {
		var nextFt uint64
		switch {
		case name == "..":
			nextFt = ftRoot // at root ".." stays at root
		case curFt == ftRoot && name == "keys":
			nextFt = ftKeys
		case curFt == ftRoot && name == "filter":
			nextFt = ftFilter
		default:
			if i == 0 {
				return rerr(fc.Tag, "no such file")
			}
			// Partial walk: return as many qids as we resolved.
			goto done
		}
		wqids = append(wqids, makeQID(nextFt))
		curFt = nextFt
	}

done:
	newf := &fid{ft: curFt}
	if len(fc.Wname) == 0 {
		newf.ft = f.ft // clone, same location
	}
	if len(wqids) == len(fc.Wname) {
		cn.fids[fc.Newfid] = newf
	}
	return &plan9.Fcall{Type: plan9.Rwalk, Tag: fc.Tag, Wqid: wqids}
}

func (cn *conn) doOpen(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	if f.open {
		return rerr(fc.Tag, "already open")
	}
	mode := fc.Mode & 3

	switch f.ft {
	case ftRoot:
		if mode != plan9.OREAD {
			return rerr(fc.Tag, "is a directory")
		}
		f.buf = dirContent()

	case ftKeys:
		// Subscribe to the broker so this fid receives future key events.
		if mode == plan9.OREAD || mode == plan9.ORDWR {
			id, ch := cn.srv.broker.subscribe()
			f.subID = id
			f.keyCh = ch
		}

	case ftFilter:
		// For reads: snapshot the current filter into buf so it can be
		// served with standard buffered reads.
		if mode == plan9.OREAD {
			f.buf = cn.srv.filterSnapshot()
		}
	}

	f.open = true
	f.mode = fc.Mode
	return &plan9.Fcall{
		Type:   plan9.Ropen,
		Tag:    fc.Tag,
		Qid:    makeQID(f.ft),
		Iounit: cn.msize - uint32(plan9.IOHDRSZ),
	}
}

// doRead handles Tread.  For a keys fid opened for reading it spawns a
// goroutine that blocks until the next key event (or a Tflush / disconnect).
// All other reads are satisfied synchronously as in acme-styles.
func (cn *conn) doRead(fc *plan9.Fcall) {
	f := cn.fids[fc.Fid]
	if f == nil {
		cn.send(rerr(fc.Tag, "fid unknown"))
		return
	}
	if !f.open {
		cn.send(rerr(fc.Tag, "not open"))
		return
	}

	// Streaming read from the keys file — block until a keystroke arrives.
	if f.ft == ftKeys && f.keyCh != nil {
		ctx, cancel := context.WithCancel(context.Background())

		cn.pendMu.Lock()
		cn.pending[fc.Tag] = cancel
		cn.pendMu.Unlock()

		// Capture locals for the goroutine.
		keyCh := f.keyCh
		tag := fc.Tag
		count := fc.Count

		go func() {
			defer func() {
				cn.pendMu.Lock()
				delete(cn.pending, tag)
				cn.pendMu.Unlock()
				cancel()
			}()

			select {
			case ev, ok := <-keyCh:
				// Check whether a Tflush arrived between the event being
				// received and us trying to reply.
				select {
				case <-ctx.Done():
					return
				default:
				}
				if !ok {
					// The channel was closed by doClunk; don't send anything.
					return
				}
				data := []byte(ev.Str + "\n")
				if uint32(len(data)) > count {
					data = data[:count]
				}
				cn.send(&plan9.Fcall{Type: plan9.Rread, Tag: tag, Data: data})

			case <-ctx.Done():
				// Cancelled by Tflush; Rflush already sent by doFlush.
			}
		}()
		return
	}

	// Buffered read (directory listing or write-only fid returning EOF).
	off := fc.Offset
	n := fc.Count
	if off >= uint64(len(f.buf)) {
		cn.send(&plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag})
		return
	}
	end := off + uint64(n)
	if end > uint64(len(f.buf)) {
		end = uint64(len(f.buf))
	}
	cn.send(&plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Data: f.buf[off:end]})
}

// doWrite handles Twrite.  Writing to the keys file re-injects the named
// keystroke.  Lines are accumulated across writes and processed one per '\n',
// mirroring acme-styles' ctl file handling.
func (cn *conn) doWrite(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	if !f.open {
		return rerr(fc.Tag, "not open")
	}
	if f.ft != ftKeys && f.ft != ftFilter {
		return rerr(fc.Tag, "not writable")
	}

	// Filter writes just accumulate — the filter is applied atomically at clunk.
	if f.ft == ftFilter {
		f.wbuf = append(f.wbuf, fc.Data...)
		return &plan9.Fcall{Type: plan9.Rwrite, Tag: fc.Tag, Count: uint32(len(fc.Data))}
	}

	f.wbuf = append(f.wbuf, fc.Data...)
	for {
		nl := bytes.IndexByte(f.wbuf, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimSpace(string(f.wbuf[:nl]))
		f.wbuf = f.wbuf[nl+1:]
		if line == "" {
			continue
		}
		ev, ok := parseKeyEvent(line)
		if !ok {
			continue // modifier-only or unknown state line — nothing to inject
		}
		reInjectKey(ev)
	}
	return &plan9.Fcall{Type: plan9.Rwrite, Tag: fc.Tag, Count: uint32(len(fc.Data))}
}

func (cn *conn) doClunk(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f != nil && f.open {
		if f.keyCh != nil {
			// Wake any goroutine blocked in doRead.
			cn.srv.broker.unsubscribe(f.subID)
		}
		if f.ft == ftFilter && f.mode&3 != plan9.OREAD && len(f.wbuf) > 0 {
			// Apply the accumulated filter atomically on close.
			cn.srv.applyFilter(f.wbuf)
		}
	}
	delete(cn.fids, fc.Fid)
	return &plan9.Fcall{Type: plan9.Rclunk, Tag: fc.Tag}
}

func (cn *conn) doStat(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	d := makeDir(f.ft)
	stat, err := d.Bytes()
	if err != nil {
		return rerr(fc.Tag, err.Error())
	}
	return &plan9.Fcall{Type: plan9.Rstat, Tag: fc.Tag, Stat: stat}
}
