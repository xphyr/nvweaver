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

const rpcTimeout = 60 * time.Second

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

// client speaks JSON-RPC 2.0 over SPDK's Unix socket, one request at a time.
type client struct {
	mu   sync.Mutex
	conn net.Conn
	dec  *json.Decoder
	id   int
}

func dial(path string) (*client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return &client{conn: conn, dec: json.NewDecoder(conn)}, nil
}

func (c *client) Call(method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id++
	req := struct {
		Version string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", c.id, method, params}
	if err := c.conn.SetDeadline(time.Now().Add(rpcTimeout)); err != nil {
		return err
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := c.conn.Write(b); err != nil {
		return fmt.Errorf("spdk %s: %w", method, err)
	}
	var resp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := c.dec.Decode(&resp); err != nil {
		return fmt.Errorf("spdk %s: %w", method, err)
	}
	if resp.ID != c.id {
		return fmt.Errorf("spdk %s: response id %d does not match request %d", method, resp.ID, c.id)
	}
	if resp.Error != nil {
		resp.Error.Method = method
		return resp.Error
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}

func (c *client) close() error { return c.conn.Close() }
