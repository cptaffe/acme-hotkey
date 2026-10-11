package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cptaffe/automaton/seq"
	"github.com/cptaffe/automaton/seq/hotkey"
)

type hub struct {
	mu     sync.Mutex
	text   string
	rules  *hotkey.Rules
	m      *seq.Machine
	app    string
	timers map[string]*time.Timer
	gen    int
	rc     string
}

var theHub *hub

func newHub() *hub {
	rc, err := exec.LookPath("rc")
	if err != nil {
		rc = filepath.Join(os.Getenv("PLAN9"), "bin", "rc")
	}
	return &hub{rc: rc, timers: map[string]*time.Timer{}}
}

func (h *hub) key(key, app string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		return false
	}
	h.app = app
	taken, fired := h.m.Step(h.rules.Table.Alpha.Of(key), seq.Env{"app": app})
	h.arm(h.m.Waits())
	h.run(fired, key)
	return taken
}

func (h *hub) sendLines(text string) error {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			return fmt.Errorf("send: want app key, got %q", line)
		}
		k, err := hotkey.Canon(f[1])
		if err != nil {
			return fmt.Errorf("send: %v", err)
		}
		h.key(k, f[0])
	}
	return nil
}

func (h *hub) arm(waits []string) {
	for _, t := range h.timers {
		t.Stop()
	}
	clear(h.timers)
	h.gen++
	gen := h.gen
	for _, name := range waits {
		h.timers[name] = time.AfterFunc(h.rules.Grammar.Delays[name], func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if gen != h.gen {
				return
			}
			_, fired := h.m.Step(h.rules.Table.Alpha.Of(name), seq.Env{"app": h.app})
			h.arm(h.m.Waits())
			h.run(fired, name)
		})
	}
}

func (h *hub) run(fired []*seq.Accept, key string) {
	for _, a := range fired {
		if a.Action == "" {
			continue
		}
		cmd := exec.Command(h.rc, "-c", h.rules.Prelude+"\n"+a.Action)
		cmd.Env = append(os.Environ(), "app="+h.app, "key="+key)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("acme-hotkey: %v", err)
			continue
		}
		go cmd.Wait()
	}
}

func (h *hub) parse(name, text string) (*hotkey.Rules, error) {
	return hotkey.Parse(strings.NewReader(text), name, knownKey)
}

func (h *hub) install(text string, r *hotkey.Rules) error {
	m, err := seq.NewMachine(r.Table, "")
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rules = r
	h.arm(nil)
	h.text, h.m = text, m
	return nil
}

func (h *hub) source() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.text
}

func (h *hub) card() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rules == nil {
		return ""
	}
	var b strings.Builder
	for _, r := range h.rules.Grammar.Rules {
		if r.Include != "" {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t{ %s }\n", r.Scope, r.Pattern, r.Label, strings.Join(strings.Fields(r.Action), " "))
	}
	return b.String()
}
