package main

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type spawned struct {
	mu   sync.Mutex
	runs []string
}

func (s *spawned) record(script string, env []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, strings.TrimSpace(script)+" "+strings.Join(env, " "))
}

func (s *spawned) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.runs...)
}

func loaded(t *testing.T, rules string) (*hub, *spawned) {
	t.Helper()
	h, s := newHub(), &spawned{}
	h.spawn = s.record
	r, err := h.parse("rules", rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.install(rules, r); err != nil {
		t.Fatal(err)
	}
	return h, s
}

const acme = `main:
	-> acme when app == "devdraw"

acme:
	command-s		{ put }	# save
	control-x control-s	{ put }
	control-x (any \ escape)	{ }
`

func TestAKeyRunsTheRuleItFinishes(t *testing.T) {
	for _, c := range []struct {
		name  string
		given string
		when  [][2]string
		then  []bool
		runs  []string
	}{
		{
			name:  "a key in the app its rule holds for",
			given: acme,
			when:  [][2]string{{"devdraw", "command-s"}},
			then:  []bool{true},
			runs:  []string{"put app=devdraw key=command-s"},
		},
		{
			name:  "a key in another app passes through",
			given: acme,
			when:  [][2]string{{"Safari", "command-s"}},
			then:  []bool{false},
		},
		{
			name:  "a sequence runs once it is whole",
			given: acme,
			when:  [][2]string{{"devdraw", "control-x"}, {"devdraw", "control-s"}},
			then:  []bool{true, true},
			runs:  []string{"put app=devdraw key=control-s"},
		},
		{
			name:  "an empty block takes a key and runs nothing",
			given: acme,
			when:  [][2]string{{"devdraw", "control-x"}, {"devdraw", "j"}},
			then:  []bool{true, true},
		},
		{
			name:  "a key the rules leave out passes through",
			given: acme,
			when:  [][2]string{{"devdraw", "control-x"}, {"devdraw", "escape"}},
			then:  []bool{true, false},
		},
		{
			name:  "a key the keyboard lacks drops what was pending",
			given: acme,
			when:  [][2]string{{"devdraw", "control-x"}, {"devdraw", "no-such-key"}, {"devdraw", "control-s"}},
			then:  []bool{true, false, false},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, s := loaded(t, c.given)

			taken := make([]bool, 0, len(c.when))
			for _, k := range c.when {
				taken = append(taken, h.key(k[1], k[0]))
			}

			if !reflect.DeepEqual(taken, c.then) || !reflect.DeepEqual(s.got(), c.runs) {
				t.Errorf("took %v and ran %q; want %v and %q", taken, s.got(), c.then, c.runs)
			}
		})
	}
}

func TestADelayRunsItsRuleWhenItsTimeIsUp(t *testing.T) {
	h, s := loaded(t, "%delay settle 20ms\nmain:\n\tcontrol-x settle { show }\n")

	h.key("control-x", "devdraw")

	deadline := time.Now().Add(2 * time.Second)
	for len(s.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if want := []string{"show app=devdraw key=settle"}; !reflect.DeepEqual(s.got(), want) {
		t.Errorf("ran %q; want %q", s.got(), want)
	}
}

func TestSendReadsAnAppAndAKeyALine(t *testing.T) {
	for _, c := range []struct {
		name  string
		given string
		then  string
		runs  []string
	}{
		{"one line", "devdraw command-s\n", "", []string{"put app=devdraw key=command-s"}},
		{"two lines", "devdraw control-x\ndevdraw control-s\n", "", []string{"put app=devdraw key=control-s"}},
		{"a line without its key", "devdraw\n", `send: want app key, got "devdraw"`, nil},
		{"a key named twice over", "devdraw command-command-s\n", "send: command twice", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, s := loaded(t, acme)

			err := h.sendLines(c.given)

			if got := errText(err); got != c.then || !reflect.DeepEqual(s.got(), c.runs) {
				t.Errorf("said %q and ran %q; want %q and %q", got, s.got(), c.then, c.runs)
			}
		})
	}
}

func TestTheCardListsEveryRule(t *testing.T) {
	h, _ := loaded(t, acme)

	got := h.card()

	if want := "acme\tcommand-s\tsave\t{ put }\nacme\tcontrol-x control-s\t\t{ put }\nacme\tcontrol-x (any \\ escape)\t\t{ }\n"; got != want {
		t.Errorf("the card is\n%q\nwant\n%q", got, want)
	}
}

func TestRulesThatDoNotParseAreRefused(t *testing.T) {
	h := newHub()

	_, err := h.parse("rules", "main:\n\tj-k { x }\n")

	if got, want := errText(err), "rules:2: j-k: no such key"; got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
