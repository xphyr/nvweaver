package sdnotify

import (
	"net"
	"path/filepath"
	"testing"
)

func listen(t *testing.T, name string) *net.UnixConn {
	t.Helper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func expect(t *testing.T, conn *net.UnixConn, want string) {
	t.Helper()
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != want {
		t.Errorf("received %q, want %q", got, want)
	}
}

func TestSendToPathSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify")
	conn := listen(t, path)
	t.Setenv("NOTIFY_SOCKET", path)
	if err := Send("READY=1"); err != nil {
		t.Fatal(err)
	}
	expect(t, conn, "READY=1")
}

func TestSendToAbstractSocket(t *testing.T) {
	name := "nvweaver-sdnotify-test-" + filepath.Base(t.TempDir())
	conn := listen(t, "\x00"+name)
	t.Setenv("NOTIFY_SOCKET", "@"+name)
	if err := Send("STOPPING=1"); err != nil {
		t.Fatal(err)
	}
	expect(t, conn, "STOPPING=1")
}

func TestNoSocketIsNotAnError(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Send("READY=1"); err != nil {
		t.Errorf("Send outside systemd = %v", err)
	}
}
