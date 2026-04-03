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

	// Record the inode of the socket we just created so cleanup never
	// removes a replacement socket created by a newer instance.
	ownIno := socketIno(srvPath)

	removeSelf := func() {
		if ownIno != 0 && socketIno(srvPath) == ownIno {
			os.Remove(srvPath)
		}
	}

	defer l.Close()
	defer removeSelf()

	fmt.Fprintln(os.Stderr, "acme-hotkey: listening on", srvPath)
	fmt.Fprintf(os.Stderr, "acme-hotkey: 9p -a unix!%s read keys\n", srvPath)

	// Start the CGEventTap run loop (blocks until process exits).
	go startTap()

	// Clean up socket on signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		removeSelf()
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

// socketIno returns the inode number of the Unix socket at path,
// or 0 if the path does not exist or is not a socket.
func socketIno(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return 0
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}
