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
	l, cleanup, err := p9srv.ListenUnix("acme-hotkey")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer cleanup()

	srvPath := p9srv.ServicePath("acme-hotkey")
	fmt.Fprintln(os.Stderr, "acme-hotkey: listening on", srvPath)
	fmt.Fprintf(os.Stderr, "acme-hotkey: 9p -a unix!%s read keys\n", srvPath)

	// Start the CGEventTap run loop (blocks until process exits).
	go startTap()

	// Clean up socket on signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()

	srv := newServer()
	for {
		c, err := l.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go srv.handleConn(c)
	}
}
