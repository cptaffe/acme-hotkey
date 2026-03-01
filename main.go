package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"9fans.net/go/plan9/client"
)

func main() {
	srvPath := client.Namespace() + "/acme-hotkey"
	os.Remove(srvPath)

	l, err := net.Listen("unix", srvPath)
	if err != nil {
		log.Fatalf("listen %s: %v", srvPath, err)
	}
	defer l.Close()
	defer os.Remove(srvPath)

	fmt.Fprintln(os.Stderr, "acme-hotkey: listening on", srvPath)
	fmt.Fprintf(os.Stderr, "acme-hotkey: 9p -a unix!%s read keys\n", srvPath)

	// Start the CGEventTap run loop (blocks until process exits).
	go startTap()

	// Clean up socket on signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Remove(srvPath)
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
