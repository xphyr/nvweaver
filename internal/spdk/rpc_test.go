package spdk

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSocket is SPDK's socket. Each connection is handed to answer, with how many came before it.
func fakeSocket(t *testing.T, answer func(n int, conn net.Conn, id int)) (path string, accepted *atomic.Int32) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "spdk.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	accepted = &atomic.Int32{}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			n := int(accepted.Add(1))
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				dec := json.NewDecoder(r)
				for {
					var req struct {
						ID int `json:"id"`
					}
					if dec.Decode(&req) != nil {
						return
					}
					answer(n, conn, req.ID)
				}
			}()
		}
	}()
	return path, accepted
}

func reply(conn net.Conn, id int) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": true})
	conn.Write(b)
}

// After a reply that can't be the one asked for, the connection is out of step, so the client starts another.
func TestABrokenConnectionIsReplaced(t *testing.T) {
	path, accepted := fakeSocket(t, func(n int, conn net.Conn, id int) {
		if n == 1 {
			reply(conn, id+100) // the wrong reply, as after an earlier call gave up
			return
		}
		reply(conn, id)
	})
	c, err := dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err := c.Call("first", nil, nil); err == nil {
		t.Fatal("a mismatched reply was accepted")
	}
	var ok bool
	if err := c.Call("second", nil, &ok); err != nil || !ok {
		t.Fatalf("the next call: %v, %v", ok, err)
	}
	if n := accepted.Load(); n != 2 {
		t.Errorf("%d connections, want a new one after the failure", n)
	}
}

// A call that times out leaves its reply to come, and a json.Decoder keeps its error, so before this was fixed every
// later call failed until the service restarted.
func TestATimeoutDoesntBreakLaterCalls(t *testing.T) {
	old := rpcTimeout
	rpcTimeout = 200 * time.Millisecond
	t.Cleanup(func() { rpcTimeout = old })
	path, accepted := fakeSocket(t, func(n int, conn net.Conn, id int) {
		if n == 1 {
			time.Sleep(400 * time.Millisecond) // too late
		}
		reply(conn, id)
	})
	c, err := dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err := c.Call("slow", nil, nil); err == nil {
		t.Fatal("no timeout")
	}
	for i := range 3 {
		if err := c.Call("next", nil, nil); err != nil {
			t.Fatalf("call %d after the timeout: %v", i, err)
		}
	}
	if n := accepted.Load(); n != 2 {
		t.Errorf("%d connections", n)
	}
}

// An error that SPDK reports, in a proper reply, leaves the connection as it is.
func TestAnRPCErrorKeepsTheConnection(t *testing.T) {
	path, accepted := fakeSocket(t, func(n int, conn net.Conn, id int) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -19, "message": "No such device"}})
		conn.Write(b)
	})
	c, err := dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	for range 2 {
		var re *RPCError
		if err := c.Call("bdev_get_bdevs", nil, nil); !errors.As(err, &re) {
			t.Fatalf("got %v", err)
		}
	}
	if n := accepted.Load(); n != 1 {
		t.Errorf("%d connections", n)
	}
}
