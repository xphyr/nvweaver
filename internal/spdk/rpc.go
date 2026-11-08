package spdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// rpcTimeout is a variable so that tests can shorten it.
var rpcTimeout = 60 * time.Second

// Caller makes a JSON-RPC call. Tests substitute a fake SPDK.
type Caller interface {
	// Call invokes method. A nil result discards the response.
	Call(method string, params, result any) error
}

// RPCError is an error reported by SPDK.
type RPCError struct {
	Method  string
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("spdk %s: %s (code %d)", e.Method, e.Message, e.Code)
}

const invalidParams = -32602

func isBusy(err error) bool {
	var re *RPCError
	return errors.As(err, &re) && strings.Contains(strings.ToLower(re.Message), "busy")
}

// client speaks JSON-RPC 2.0 over SPDK's Unix socket, one request at a time. After a call fails without a proper
// reply, the connection may still carry that reply, and a json.Decoder keeps a read error for good, so it's replaced.
type client struct {
	mu   sync.Mutex
	path string
	conn net.Conn // nil after a failed dial, so that the next call dials first
	dec  *json.Decoder
	id   int
}

func dial(path string) (*client, error) {
	c := &client{path: path}
	if err := c.redial(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *client) redial() error {
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn, c.dec = nil, nil
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return err
	}
	c.conn, c.dec = conn, json.NewDecoder(conn)
	return nil
}

// broken replaces the connection, and returns err. A failed dial is left for the next call to try again.
func (c *client) broken(err error) error {
	_ = c.redial()
	return err
}

func (c *client) Call(method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.redial(); err != nil {
			return fmt.Errorf("spdk %s: %w", method, err)
		}
	}
	c.id++
	req := struct {
		Version string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", c.id, method, params}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := c.conn.SetDeadline(time.Now().Add(rpcTimeout)); err != nil {
		return c.broken(fmt.Errorf("spdk %s: %w", method, err))
	}
	if _, err := c.conn.Write(b); err != nil {
		return c.broken(fmt.Errorf("spdk %s: %w", method, err))
	}
	var resp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := c.dec.Decode(&resp); err != nil {
		return c.broken(fmt.Errorf("spdk %s: %w", method, err))
	}
	if resp.ID != c.id {
		return c.broken(fmt.Errorf("spdk %s: response id %d does not match request %d", method, resp.ID, c.id))
	}
	// From here, the reply was read whole, so the connection is fine whatever it says.
	if resp.Error != nil {
		resp.Error.Method = method
		return resp.Error
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}

func (c *client) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
