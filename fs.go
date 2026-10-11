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

func buildServer(h *hub, tree *srv9p.Tree, rulesFile, cardFile, sendFile *srv9p.File) *srv9p.Server {
	return &srv9p.Server{
		Tree: tree,

		Open: func(ctx context.Context, fid *srv9p.Fid, mode uint8) error {
			m := mode & 3
			switch f := fid.File(); {
			case f == rulesFile && (m == plan9.OWRITE || m == plan9.ORDWR):
				fid.SetAux(&rulesWrite{})
			case f == rulesFile:
				fid.SetAux(h.source())
			case f == cardFile:
				fid.SetAux(h.card())
			}
			return nil
		},

		Read: func(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
			if a, ok := fid.Aux().(string); ok {
				return fid.ReadString(data, offset, a)
			}
			return 0, errors.New("not readable")
		},

		Write: func(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
			if fid.File() == sendFile {
				return len(data), h.sendLines(string(data))
			}
			w, ok := fid.Aux().(*rulesWrite)
			if !ok {
				return 0, errors.New("not writable")
			}
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
		},

		Clunk: func(fid *srv9p.Fid) {
			a, ok := fid.Aux().(*rulesWrite)
			if !ok || a.failed {
				return
			}
			f, err := h.parse("rules", string(a.buf))
			if err != nil {
				log.Printf("acme-hotkey: %v", err)
				return
			}
			h.install(string(a.buf), f)
		},
	}
}
