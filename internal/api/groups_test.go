package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const groupSub = "nqn.2026-11.net.example:g"

// groupEnv has volumes a, b and c, with a and b exported in one subsystem, as namespaces 1 and 2, and c not exported.
func groupEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.pool()
	for _, v := range []string{"a", "b", "c"} {
		e.do("POST", "/v1/volumes", obj{"name": v, "size_bytes": 4096}, 201)
	}
	e.do("POST", "/v1/ports", obj{"id": 1, "address": "127.0.0.1"}, 201)
	e.do("POST", "/v1/subsystems", obj{"nqn": groupSub, "allow_any_host": true, "ports": []int{1}}, 201)
	e.do("POST", "/v1/subsystems/"+groupSub+"/namespaces", obj{"nsid": 1, "volume": "a"}, 201)
	e.do("POST", "/v1/subsystems/"+groupSub+"/namespaces", obj{"nsid": 2, "volume": "b"}, 201)
	return e
}

// anaGroups is what the kernel has each namespace of the group subsystem in.
func (e *env) anaGroups() string {
	var out []string
	for _, nsid := range []string{"1", "2"} {
		v, _ := e.fs.Attr("/nvmet/subsystems/" + groupSub + "/namespaces/" + nsid + "/ana_grpid")
		out = append(out, v)
	}
	return strings.Join(out, " ")
}

func (e *env) pauseGroupThere() bool {
	return slices.Contains(e.fs.Paths(), "/nvmet/ports/1/ana_groups/2")
}

// Every snapshot of a group is taken while hosts are paused, which is what makes them consistent with each other.
func TestASnapshotGroupPausesHostsWhileItsTaken(t *testing.T) {
	e := groupEnv(t)
	var during []string
	e.pools.snapshotting = func(string) error {
		during = append(during, e.anaGroups())
		return nil
	}
	g := e.do("POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"c", "a", "b"}}, 201)
	if !slices.Equal(during, []string{"2 2", "2 2", "2 2"}) {
		t.Errorf("namespaces' ANA groups as each snapshot was taken: %v, want both paused for all three", during)
	}
	if got := e.anaGroups(); got != "1 1" || e.pauseGroupThere() {
		t.Errorf("after: groups %q, pause group still there: %v", got, e.pauseGroupThere())
	}
	if fmt.Sprint(g["snapshots"]) != "[g-1 g-2 g-3]" || fmt.Sprint(g["volumes"]) != "[c a b]" {
		t.Errorf("group came back as %v", g)
	}
	if _, ok := g["paused_ms"]; !ok {
		t.Error("no paused_ms")
	}
	if snap := e.do("GET", "/v1/snapshots/g-2", nil, 200); snap["volume"] != "a" || snap["group"] != "g" {
		t.Errorf("g-2 came back as %v", snap)
	}

	// CSI retries: the same again is the same group, and nothing is taken again.
	during = nil
	if again := e.do("POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"c", "a", "b"}}, 200); fmt.Sprint(again) != fmt.Sprint(g) {
		t.Errorf("a retry gave %v", again)
	}
	if len(during) != 0 {
		t.Error("a retry took snapshots again")
	}
	e.do("POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"a"}}, 409)

	e.do("DELETE", "/v1/snapshots/g-1", nil, 409)
	e.do("DELETE", "/v1/snapshot-groups/g", nil, 204)
	for _, n := range []string{"g-1", "g-2", "g-3"} {
		e.do("GET", "/v1/snapshots/"+n, nil, 404)
	}
	e.do("GET", "/v1/snapshot-groups/g", nil, 404)
	e.consistent()
}

// A group whose volumes aren't exported has nothing to pause.
func TestASnapshotGroupOfVolumesNotExported(t *testing.T) {
	e := groupEnv(t)
	e.pools.snapshotting = func(string) error {
		if e.pauseGroupThere() {
			t.Error("paused for volumes that no host can reach")
		}
		return nil
	}
	if g := e.do("POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"c"}}, 201); g["paused_ms"] != float64(0) {
		t.Errorf("paused for %v ms", g["paused_ms"])
	}
}

// If any snapshot fails, the others are removed, hosts are resumed, and nothing is kept.
func TestASnapshotGroupThatFailsLeavesNothing(t *testing.T) {
	e := groupEnv(t)
	n := 0
	e.pools.snapshotting = func(string) error {
		if n++; n == 2 {
			return fmt.Errorf("lvcreate: %w", syscall.ENOSPC)
		}
		return nil
	}
	e.do("POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"a", "b", "c"}}, http.StatusInsufficientStorage)
	e.do("GET", "/v1/snapshot-groups/g", nil, 404)
	e.do("GET", "/v1/snapshots/g-1", nil, 404)
	if got := e.anaGroups(); got != "1 1" || e.pauseGroupThere() {
		t.Errorf("hosts weren't resumed: %q", got)
	}
	e.consistent()
	// And it can be tried again.
	e.pools.snapshotting = nil
	e.do("POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"a", "b", "c"}}, 201)
	e.consistent()
}

// Hosts are kept waiting no longer than maxPause: past it, the group is undone. And one group is taken at a time.
func TestAPauseIsBoundedAndOneAtATime(t *testing.T) {
	e := groupEnv(t)
	defer func(was time.Duration) { maxPause = was }(maxPause)
	maxPause = 50 * time.Millisecond
	first := true
	e.pools.snapshotting = func(string) error {
		if first {
			first = false
			// Meanwhile, another group has to wait its turn, and the volumes can't change.
			e.do("POST", "/v1/snapshot-groups", obj{"name": "h", "volumes": []string{"c"}}, 503)
			e.do("DELETE", "/v1/volumes/b", nil, 503)
			time.Sleep(2 * maxPause)
		}
		return nil
	}
	body := e.do("POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"a", "b"}}, 503)
	if !strings.Contains(fmt.Sprint(body["error"]), "paused for more than") {
		t.Errorf("error %v", body["error"])
	}
	if got := e.anaGroups(); got != "1 1" {
		t.Errorf("hosts weren't resumed: %q", got)
	}
	e.do("GET", "/v1/snapshot-groups/g", nil, 404)
	e.consistent()
}

func TestSnapshotGroupRequestsAreChecked(t *testing.T) {
	e := groupEnv(t)
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprint("v", i)
	}
	for name, req := range map[string]obj{
		"no volumes":    {"name": "g", "volumes": []string{}},
		"too many":      {"name": "g", "volumes": many},
		"one twice":     {"name": "g", "volumes": []string{"a", "a"}},
		"no such":       {"name": "g", "volumes": []string{"a", "nope"}},
		"a long name":   {"name": strings.Repeat("g", 121), "volumes": []string{"a"}},
		"a bad name":    {"name": "-g", "volumes": []string{"a"}},
		"a name with /": {"name": "g/h", "volumes": []string{"a"}},
	} {
		if body := e.do("POST", "/v1/snapshot-groups", req, 422); body["error"] == nil {
			t.Errorf("%s: %v", name, body)
		}
	}
	// A snapshot already has a member's name.
	e.do("POST", "/v1/snapshots", obj{"name": "g-2", "volume": "c"}, 201)
	e.do("POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"a", "b"}}, 409)
	e.do("GET", "/v1/snapshots/g-1", nil, 404)
	e.consistent()
}

func TestScopedTokensAndSnapshotGroups(t *testing.T) {
	e := newScopedEnv(t)
	e.as(e.lab, "POST", "/v1/volumes", obj{"name": "lab-a", "size_bytes": 4096}, 201)
	e.as(e.other, "POST", "/v1/volumes", obj{"name": "other-a", "size_bytes": 4096}, 201)
	e.as(e.other, "POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"lab-a", "other-a"}}, 403)
	g := e.as(e.lab, "POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"lab-a"}}, 201)
	if g["owner"] != "lab" {
		t.Errorf("owner %v", g["owner"])
	}
	if snap := e.as(e.lab, "GET", "/v1/snapshots/g-1", nil, 200); snap["owner"] != "lab" {
		t.Errorf("the group's snapshot is owned by %v", snap["owner"])
	}
	e.as(e.lab, "POST", "/v1/snapshot-groups", obj{"name": "h", "volumes": []string{"lab-a"}, "owner": "other"}, 403)
	e.as(e.other, "DELETE", "/v1/snapshot-groups/g", nil, 403)
	e.as(e.lab, "DELETE", "/v1/snapshot-groups/g", nil, 204)
}

// A crash mid-pause leaves hosts paused until nvweaver starts again, and a pause is never saved, so starting ends it.
// Whatever storage was made, and not saved, goes, as with any create.
func TestCrashDuringASnapshotGroup(t *testing.T) {
	for _, point := range []string{"paused", "storage"} {
		t.Run(point, func(t *testing.T) {
			e := groupEnv(t)
			before := len(e.pools.files)
			req := obj{"name": "g", "volumes": []string{"a", "b"}}
			e.crash(point, func() { e.do("POST", "/v1/snapshot-groups", req, 201) })
			if got := e.anaGroups(); got != "1 1" || e.pauseGroupThere() {
				t.Errorf("still paused after a restart: %q", got)
			}
			e.do("GET", "/v1/snapshot-groups/g", nil, 404)
			if len(e.pools.files) != before {
				t.Errorf("storage left behind: %v", e.pools.files)
			}
			e.consistent()
			e.do("POST", "/v1/snapshot-groups", req, 201)
			e.consistent()
		})
	}
	t.Run("saved", func(t *testing.T) {
		e := groupEnv(t)
		req := obj{"name": "g", "volumes": []string{"a", "b"}}
		e.crash("saved", func() { e.do("POST", "/v1/snapshot-groups", req, 201) })
		e.do("GET", "/v1/snapshot-groups/g", nil, 200)
		e.consistent()
		e.do("POST", "/v1/snapshot-groups", req, 200)
	})
}

// Deleting a group carries on where a failed delete stopped. As with any delete, storage that couldn't be removed keeps
// its intent, and the next start removes it.
func TestDeletingASnapshotGroupCarriesOn(t *testing.T) {
	e := groupEnv(t)
	e.do("POST", "/v1/snapshot-groups", obj{"name": "g", "volumes": []string{"a", "b"}}, 201)
	e.pools.deleteErr = errors.New("lvremove: busy")
	e.do("DELETE", "/v1/snapshot-groups/g", nil, 500)
	if g := e.do("GET", "/v1/snapshot-groups/g", nil, 200); fmt.Sprint(g["snapshots"]) != "[g-2]" {
		t.Errorf("after one went: %v", g["snapshots"])
	}
	e.pools.deleteErr = nil
	e.do("DELETE", "/v1/snapshot-groups/g", nil, 204)
	e.do("GET", "/v1/snapshots/g-1", nil, 404)
	e.do("GET", "/v1/snapshots/g-2", nil, 404)
	e.start()
	e.consistent()
}
