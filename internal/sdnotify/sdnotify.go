// Package sdnotify tells systemd about service state, using the documented
// sd_notify datagram protocol directly so as to need no dependencies.
package sdnotify

import (
	"fmt"
	"net"
	"os"
)

// Send delivers state to the socket named by NOTIFY_SOCKET. Outside systemd
// that is unset, and Send does nothing.
func Send(state string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	// A leading @ names a socket in the abstract namespace.
	if path[0] == '@' {
		path = "\x00" + path[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("connect to NOTIFY_SOCKET: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("write to NOTIFY_SOCKET: %w", err)
	}
	return nil
}
