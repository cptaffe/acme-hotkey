package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/cptaffe/p9srv"
)

func main() {
	initKeys()
	rw, cleanup, err := p9srv.Post("acme-hotkey")
	if err != nil {
		log.Fatalf("post: %v", err)
	}
	defer cleanup()

	fmt.Fprintln(os.Stderr, "acme-hotkey: posted as acme-hotkey")

	// Start the CGEventTap run loop in the background.
	go startTap()

	// Release the post on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()

	buildServer().Serve(rw, rw)
}
