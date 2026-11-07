package target

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Lines in the kernel's format, from a real target: 10.99.0.1:4420 with 10.99.0.2 connected, and things that must not count.
const procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100630A:1144 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 19212 1 0000000000000000 100 0 0 10 0
   1: 0100630A:1144 0200630A:A0D8 01 00000000:00000000 00:00000000 00000000     0        0 0 1 0000000000000000 20 0 0 10 -1
   2: 0100630A:1144 0200630A:A0E0 01 00000000:00000000 00:00000000 00000000     0        0 0 1 0000000000000000 20 0 0 10 -1
   3: 0100630A:1144 0300630A:A0E0 01 00000000:00000000 00:00000000 00000000     0        0 0 1 0000000000000000 20 0 0 10 -1
   4: 0100630A:1144 0400630A:A0E1 06 00000000:00000000 00:00000000 00000000     0        0 0 1 0000000000000000 20 0 0 10 -1
   5: 0100630A:0016 0200630A:A0E2 01 00000000:00000000 00:00000000 00000000     0        0 0 1 0000000000000000 20 0 0 10 -1
   6: 0200630A:1144 0500630A:A0E3 01 00000000:00000000 00:00000000 00000000     0        0 0 1 0000000000000000 20 0 0 10 -1
   7: 0100630A:1145 0600630A:A0E4 01 00000000:00000000 00:00000000 00000000     0        0 0 1 0000000000000000 20 0 0 10 -1
`

func connectionsState() *State {
	return &State{
		Ports: map[uint16]*Port{
			1: {ID: 1, Transport: TransportTCP, Address: "10.99.0.1", Service: 4420},
			2: {ID: 2, Transport: TransportTCP, Address: "0.0.0.0", Service: 4421},
			3: {ID: 3, Transport: TransportTCP, Address: "10.99.0.9", Service: 4422},
		},
		Subsystems: map[string]*Subsystem{
			"nqn.a": {NQN: "nqn.a", Ports: []uint16{1}},
			"nqn.b": {NQN: "nqn.b", Ports: []uint16{3}},
		},
	}
}

func TestConnectionsFromSockets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tcp"), []byte(procTCP), 0o600); err != nil {
		t.Fatal(err)
	}
	tgt := &Target{ProcNet: dir}
	got, err := tgt.Connections(connectionsState(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []Connection{
		{Port: 1, Transport: TransportTCP, HostAddress: "10.99.0.2", Queues: 2},
		{Port: 1, Transport: TransportTCP, HostAddress: "10.99.0.3", Queues: 1},
		// Port 2 listens on every address, and this is the only socket on its service.
		{Port: 2, Transport: TransportTCP, HostAddress: "10.99.0.6", Queues: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}

	got, err = tgt.Connections(connectionsState(), "nqn.b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a subsystem on a port with nobody connected got %+v", got)
	}
	got, err = tgt.Connections(connectionsState(), "nqn.a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Port != 1 || got[1].Port != 1 {
		t.Errorf("a subsystem's connections should be its ports': %+v", got)
	}
	if _, err := tgt.Connections(connectionsState(), "nqn.none"); err == nil {
		t.Error("no error for a subsystem that doesn't exist")
	}
}

func TestParseProcAddr(t *testing.T) {
	for in, want := range map[string]string{
		"0100630A:1144":                         "10.99.0.1:4420",
		"0100007F:A0D8":                         "127.0.0.1:41176",
		"000099FD000000000000000001000000:1144": "[fd99::1]:4420",
		"0000000000000000FFFF00000100630A:1144": "10.99.0.1:4420",
	} {
		got, err := parseProcAddr(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
	for _, in := range []string{"", "0100630A", "0100630A:1", "01:1144", "zz00630A:1144"} {
		if _, err := parseProcAddr(in); err == nil {
			t.Errorf("no error for %q", in)
		}
	}
}
