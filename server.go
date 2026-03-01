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
// QID path constants
// ---------------------------------------------------------------------------

const (
	ftRoot      = uint64(0)
	ftKeys      = uint64(1)
	ftFilter    = uint64(2)
	ftFrontmost = uint64(3)
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
	case ftFrontmost:
		return plan9.Dir{
			Qid:   makeQID(ftFrontmost),
			Mode:  0444,
			Atime: now, Mtime: now,
			Name: "frontmost", Uid: "none", Gid: "none", Muid: "none",
		}
	}
	panic("makeDir: unknown ft")
}

func dirContent() []byte {
	var out []byte
	for _, ft := range []uint64{ftKeys, ftFilter, ftFrontmost} {
		d := makeDir(ft)
		b, _ := d.Bytes()
		out = append(out, b...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// Server holds shared state across all connections.
type Server struct {
	// keys ack queue: events that have been read by the consumer and are
	// waiting for a write-back ack.  Protected by ackMu.
	ackMu    sync.Mutex
	ackQueue []KeyEvent

	// keysReaderOpen enforces a single simultaneous OREAD on the keys file,
	// matching Acme's one-listener-at-a-time event-file semantics.
	keysReaderMu   sync.Mutex
	keysReaderOpen bool

	// filter state
	filterMu      sync.RWMutex
	pendingFilter []string
	filterProcs   []string
}

func newServer() *Server { return &Server{} }

func (s *Server) filterSnapshot() []byte {
	s.filterMu.RLock()
	defer s.filterMu.RUnlock()
	if len(s.pendingFilter) == 0 {
		return nil
	}
	return []byte(strings.Join(s.pendingFilter, "\n") + "\n")
}

func (s *Server) setPendingFilter(data []byte) {
	var names []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	s.filterMu.Lock()
	s.pendingFilter = names
	s.filterMu.Unlock()
}

func (s *Server) activateFilter() {
	s.filterMu.Lock()
	names := s.pendingFilter
	s.filterProcs = names
	s.filterMu.Unlock()
	setFilter(names)
}

func (s *Server) deactivateFilter() {
	s.filterMu.Lock()
	s.filterProcs = nil
	s.filterMu.Unlock()
	setFilter(nil)
}

// ackLine processes one write-back line from the cawk pipeline.
// Lines without a leading '!' → forwardEvent (original event re-posted).
// Lines with a leading '!'   → dropEvent    (original event consumed).
func (s *Server) ackLine(line string) {
	s.ackMu.Lock()
	if len(s.ackQueue) == 0 {
		s.ackMu.Unlock()
		return
	}
	ev := s.ackQueue[0]
	s.ackQueue = s.ackQueue[1:]
	s.ackMu.Unlock()

	if strings.HasPrefix(line, "!") {
		dropEvent(ev)
	} else {
		forwardEvent(ev)
	}
}

// ---------------------------------------------------------------------------
// Per-connection state
// ---------------------------------------------------------------------------

type fid struct {
	ft   uint64
	open bool
	mode uint8
	buf  []byte // buffered content for dir / filter reads
	wbuf []byte // accumulated write bytes
}

type conn struct {
	srv     *Server
	c       net.Conn
	fids    map[uint32]*fid
	msize   uint32
	pendMu  sync.Mutex
	pending map[uint16]context.CancelFunc
	wmu     sync.Mutex

	// isKeysReader is true if this connection opened keys for OREAD.
	isKeysReader bool
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
			if cn.isKeysReader {
				cn.srv.releaseKeysReader()
			}
			return
		}
		cn.dispatch(fc)
	}
}

func (s *Server) releaseKeysReader() {
	s.keysReaderMu.Lock()
	s.keysReaderOpen = false
	s.keysReaderMu.Unlock()
	setListenerCount(0)
	s.deactivateFilter()
}

func (cn *conn) send(resp *plan9.Fcall) {
	cn.wmu.Lock()
	defer cn.wmu.Unlock()
	plan9.WriteFcall(cn.c, resp) //nolint:errcheck
}

func rerr(tag uint16, msg string) *plan9.Fcall {
	return &plan9.Fcall{Type: plan9.Rerror, Tag: tag, Ename: msg}
}

func (cn *conn) cancelAll() {
	cn.pendMu.Lock()
	defer cn.pendMu.Unlock()
	for _, cancel := range cn.pending {
		cancel()
	}
}

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
		cn.doRead(fc)
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
// Message handlers
// ---------------------------------------------------------------------------

func (cn *conn) doVersion(fc *plan9.Fcall) *plan9.Fcall {
	msize := fc.Msize
	if msize > cn.msize {
		msize = cn.msize
	}
	cn.msize = msize
	cn.fids = make(map[uint32]*fid)
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
			nextFt = ftRoot
		case curFt == ftRoot && name == "keys":
			nextFt = ftKeys
		case curFt == ftRoot && name == "filter":
			nextFt = ftFilter
		case curFt == ftRoot && name == "frontmost":
			nextFt = ftFrontmost
		default:
			if i == 0 {
				return rerr(fc.Tag, "no such file")
			}
			goto done
		}
		wqids = append(wqids, makeQID(nextFt))
		curFt = nextFt
	}
done:
	newf := &fid{ft: curFt}
	if len(fc.Wname) == 0 {
		newf.ft = f.ft
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
		if mode == plan9.OREAD || mode == plan9.ORDWR {
			// Enforce single listener.
			cn.srv.keysReaderMu.Lock()
			if cn.srv.keysReaderOpen {
				cn.srv.keysReaderMu.Unlock()
				return rerr(fc.Tag, "keys already open for reading")
			}
			cn.srv.keysReaderOpen = true
			cn.srv.keysReaderMu.Unlock()
			cn.isKeysReader = true
			setListenerCount(1)
			cn.srv.activateFilter()
		}
		// OWRITE: no special setup; writes go to the global ack handler.

	case ftFilter:
		if mode == plan9.OREAD {
			f.buf = cn.srv.filterSnapshot()
		}

	case ftFrontmost:
		if mode != plan9.OREAD {
			return rerr(fc.Tag, "read only")
		}
		if name := frontmostApp(); name != "" {
			f.buf = []byte(name + "\n")
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

// doRead handles Tread.  Reading from keys blocks until the tap delivers a
// key event, then pushes it to the global ack queue so the corresponding
// write-back can dispose of the original CGEventRef.
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

	// Streaming read: block until a key event arrives.
	if f.ft == ftKeys && cn.isKeysReader {
		ctx, cancel := context.WithCancel(context.Background())
		cn.pendMu.Lock()
		cn.pending[fc.Tag] = cancel
		cn.pendMu.Unlock()

		tag, count := fc.Tag, fc.Count
		go func() {
			defer func() {
				cn.pendMu.Lock()
				delete(cn.pending, tag)
				cn.pendMu.Unlock()
				cancel()
			}()
			select {
			case ev, ok := <-eventStream:
				select {
				case <-ctx.Done():
					// Flush cancelled; release the event immediately.
					if ok {
						dropEvent(ev)
					}
					return
				default:
				}
				if !ok {
					return
				}
				// Push to ack queue before replying — the write-back must
				// dequeue in the same order events were delivered.
				cn.srv.ackMu.Lock()
				cn.srv.ackQueue = append(cn.srv.ackQueue, ev)
				cn.srv.ackMu.Unlock()

				data := []byte(ev.Str + "\n")
				if uint32(len(data)) > count {
					data = data[:count]
				}
				cn.send(&plan9.Fcall{Type: plan9.Rread, Tag: tag, Data: data})

			case <-ctx.Done():
			}
		}()
		return
	}

	// Buffered read (directory listing, filter snapshot, frontmost).
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

// doWrite handles Twrite.
// Writing to keys is a write-back ack: each newline-terminated line is either
//   "!..." → dropEvent  (key consumed as hotkey)
//   "..."  → forwardEvent (key passed through to the application)
// Writing to filter accumulates the new filter (applied on next keys open).
func (cn *conn) doWrite(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f == nil {
		return rerr(fc.Tag, "fid unknown")
	}
	if !f.open {
		return rerr(fc.Tag, "not open")
	}

	switch f.ft {
	case ftKeys:
		// Accumulate and process complete lines.
		f.wbuf = append(f.wbuf, fc.Data...)
		for {
			nl := bytes.IndexByte(f.wbuf, '\n')
			if nl < 0 {
				break
			}
			line := string(f.wbuf[:nl])
			f.wbuf = f.wbuf[nl+1:]
			if line == "" {
				continue
			}
			cn.srv.ackLine(line)
		}

	case ftFilter:
		f.wbuf = append(f.wbuf, fc.Data...)

	default:
		return rerr(fc.Tag, "not writable")
	}

	return &plan9.Fcall{Type: plan9.Rwrite, Tag: fc.Tag, Count: uint32(len(fc.Data))}
}

func (cn *conn) doClunk(fc *plan9.Fcall) *plan9.Fcall {
	f := cn.fids[fc.Fid]
	if f != nil && f.open {
		if f.ft == ftKeys && cn.isKeysReader {
			cn.isKeysReader = false
			cn.srv.releaseKeysReader()
		}
		if f.ft == ftFilter && f.mode&3 != plan9.OREAD && len(f.wbuf) > 0 {
			cn.srv.setPendingFilter(f.wbuf)
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
