// Acme-hotkey runs rc scripts on keyboard shortcuts, matching keys against
// a rules file the way the plumber matches messages.
package main

import (
	"cmp"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"9fans.net/go/plan9/srv9p"
	"github.com/cptaffe/p9srv"
)

func main() {
	rulesPath := flag.String("rules", filepath.Join(os.Getenv("HOME"), "lib", "hotkeys"), "rules file to load at start")
	flag.Parse()

	initKeys()

	uid := cmp.Or(os.Getenv("USER"), "none")
	tree := srv9p.NewTree(uid, uid, 0555, nil)
	rulesFile, _ := tree.Root.Create("rules", uid, 0666, nil)
	cardFile, _ := tree.Root.Create("card", uid, 0444, nil)
	sendFile, _ := tree.Root.Create("send", uid, 0222, nil)
	theHub = newHub()

	if text, err := os.ReadFile(*rulesPath); err == nil {
		f, err := theHub.parse(*rulesPath, string(text))
		if err == nil {
			err = theHub.install(string(text), f)
		}
		if err != nil {
			log.Printf("acme-hotkey: %v", err)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("acme-hotkey: %v", err)
	}

	rw, cleanup, err := p9srv.Post("acme-hotkey")
	if err != nil {
		log.Fatalf("post: %v", err)
	}
	defer cleanup()
	fmt.Fprintln(os.Stderr, "acme-hotkey: posted as acme-hotkey")

	go startTap()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()

	buildServer(theHub, tree, rulesFile, cardFile, sendFile).Serve(rw, rw)
}
