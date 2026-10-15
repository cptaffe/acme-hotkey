package main

import (
	"context"
	"errors"
	"log"
	"strings"

	"9fans.net/go/plan9"
	"9fans.net/go/plan9/srv9p"
)

// A write to rules is checked as it arrives and installed at clunk.
type rulesWrite struct {
	buf    []byte
	failed bool
}

type server struct {
	h                     *hub
	rules, card, sendFile *srv9p.File
}

func buildServer(h *hub, tree *srv9p.Tree, rulesFile, cardFile, sendFile *srv9p.File) *srv9p.Server {
	s := &server{h: h, rules: rulesFile, card: cardFile, sendFile: sendFile}
	return &srv9p.Server{Tree: tree, Open: s.open, Read: s.read, Write: s.write, Clunk: s.clunk}
}

func (s *server) open(ctx context.Context, fid *srv9p.Fid, mode uint8) error {
	m := mode & 3
	switch f := fid.File(); {
	case f == s.rules && (m == plan9.OWRITE || m == plan9.ORDWR):
		fid.SetAux(&rulesWrite{})
	case f == s.rules:
		fid.SetAux(s.h.source())
	case f == s.card:
		fid.SetAux(s.h.card())
	}
	return nil
}

func (s *server) read(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
	if a, ok := fid.Aux().(string); ok {
		return fid.ReadString(data, offset, a)
	}
	return 0, errors.New("not readable")
}

func (s *server) write(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
	if fid.File() == s.sendFile {
		return len(data), s.h.sendLines(string(data))
	}
	w, ok := fid.Aux().(*rulesWrite)
	if !ok {
		return 0, errors.New("not writable")
	}
	return w.add(s.h, data)
}

func (w *rulesWrite) add(h *hub, data []byte) (int, error) {
	if w.failed {
		return 0, errors.New("rules: an earlier write failed")
	}
	w.buf = append(w.buf, data...)
	text := string(w.buf)
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		if _, err := h.parse("rules", text[:i+1]); err != nil {
			w.failed = true
			return 0, err
		}
	}
	return len(data), nil
}

func (s *server) clunk(fid *srv9p.Fid) {
	a, ok := fid.Aux().(*rulesWrite)
	if !ok || a.failed {
		return
	}
	f, err := s.h.parse("rules", string(a.buf))
	if err != nil {
		log.Printf("acme-hotkey: %v", err)
		return
	}
	if err := s.h.install(string(a.buf), f); err != nil {
		log.Printf("acme-hotkey: %v", err)
	}
}
