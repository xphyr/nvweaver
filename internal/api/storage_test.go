package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"nvweaver/internal/target"
	"nvweaver/internal/target/targettest"
)

func (e *env) pool() {
	e.t.Helper()
	e.do("POST", "/v1/pools", obj{"name": "p", "type": "reflink", "path": "/pool"}, 201)
}

func TestAdoptingAPool(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/v1/pools", obj{"name": "p", "type": "lvm-thin", "path": "/x"}, 422)
	e.do("POST", "/v1/pools", obj{"name": "../p", "type": "reflink", "path": "/x"}, 422)
	e.do("POST", "/v1/pools", obj{"name": "p", "type": "reflink", "path": "/x", "warning_percent": 101}, 422)
	e.pools.checkErr = target.Invalid("no reflink here")
	got := e.do("POST", "/v1/pools", obj{"name": "p", "type": "reflink", "path": "/x"}, 422)
	if !strings.Contains(fmt.Sprint(got["error"]), "no reflink") {
		t.Errorf("error = %v", got["error"])
	}
	e.pools.checkErr = nil
	e.pool()
	e.do("POST", "/v1/pools", obj{"name": "p", "type": "reflink", "path": "/pool"}, 200)
	e.do("POST", "/v1/pools", obj{"name": "p", "type": "reflink", "path": "/other"}, 409)
	e.do("POST", "/v1/pools", obj{"name": "q", "type": "reflink", "path": "/pool"}, 409)
}

func TestPoolReportsProblems(t *testing.T) {
	e := newEnv(t)
	e.pool()
	got := e.do("GET", "/v1/pools/p", nil, 200)
	if got["state"] != "online" {
		t.Errorf("state = %v", got["state"])
	}
	if !strings.Contains(fmt.Sprint(got["warnings"]), "90.0% full") {
		t.Errorf("should warn at 90%% against the default of 80%%: %v", got["warnings"])
	}
	e.do("PATCH", "/v1/pools/p", obj{"warning_percent": 95}, 200)
	if got := e.do("GET", "/v1/pools/p", nil, 200); fmt.Sprint(got["warnings"]) != "[]" {
		t.Errorf("warnings = %v", got["warnings"])
	}
	e.pools.files[target.InternalPrefix+"stray"] = 1
	if got := e.do("GET", "/v1/pools/p", nil, 200); !strings.Contains(fmt.Sprint(got["warnings"]), "stray") {
		t.Errorf("should report a stray: %v", got["warnings"])
	}
}

func TestCreateVolumeIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 422) // No pool yet.
	e.pool()
	first := e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	again := e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 200)
	if first["uuid"] != again["uuid"] || first["nguid"] != again["nguid"] || first["internal_name"] != again["internal_name"] {
		t.Errorf("a retry should find the same volume: %v, %v", first, again)
	}
	if len(e.pools.files) != 1 {
		t.Errorf("a retry allocated more storage: %v", e.pools.files)
	}
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 8192}, 409)
	if !strings.HasPrefix(fmt.Sprint(first["internal_name"]), target.InternalPrefix) {
		t.Errorf("internal name = %v", first["internal_name"])
	}
}

func TestPoolIsRequiredUnlessThereIsOnlyOne(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "a", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/pools", obj{"name": "q", "type": "reflink", "path": "/q"}, 201)
	e.do("POST", "/v1/volumes", obj{"name": "b", "size_bytes": 4096}, 422)
	got := e.do("POST", "/v1/volumes", obj{"name": "b", "pool": "q", "size_bytes": 4096}, 201)
	if got["pool"] != "q" {
		t.Errorf("pool = %v", got["pool"])
	}
	e.do("POST", "/v1/volumes", obj{"name": "c", "pool": "none", "size_bytes": 4096}, 422)
}

func TestVolumeRequests(t *testing.T) {
	e := newEnv(t)
	e.pool()
	for name, body := range map[string]obj{
		"bad name":    {"name": "a/b", "size_bytes": 4096},
		"hidden name": {"name": ".a", "size_bytes": 4096},
		"size":        {"name": "a", "size_bytes": 4097},
		"too small":   {"name": "a"},
	} {
		t.Run(name, func(t *testing.T) {
			e.t = t
			e.do("POST", "/v1/volumes", body, 422)
		})
	}
	e.t = t
	if len(e.pools.files) != 0 {
		t.Errorf("a refused request allocated storage: %v", e.pools.files)
	}
}

func TestFullPool(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.pools.createErr = fmt.Errorf("create volume file: %w", syscall.ENOSPC)
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 507)
	e.do("GET", "/v1/volumes/v", nil, 404)
}

func TestMappingUsesTheVolumesIdentity(t *testing.T) {
	e := newEnv(t)
	e.pool()
	vol := e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	for _, s := range []string{"a", "b"} {
		e.do("POST", "/v1/subsystems", obj{"nqn": subNQN + s}, 201)
	}
	nsPath := "/v1/subsystems/" + subNQN + "a/namespaces"
	e.do("POST", nsPath, obj{"volume": "none"}, 422)
	e.do("POST", nsPath, obj{"volume": "v", "uuid": "11111111-1111-4111-8111-111111111111"}, 422)
	e.do("POST", nsPath, obj{"volume": "v", "device_path": "/x"}, 422)
	ns := e.do("POST", nsPath, obj{"volume": "v"}, 201)
	for _, f := range []string{"uuid", "nguid", "device_path"} {
		if ns[f] != vol[f] {
			t.Errorf("namespace %s = %v, volume has %v", f, ns[f], vol[f])
		}
	}
	prefix := "/nvmet/subsystems/" + subNQN + "a/namespaces/1/"
	if got, _ := e.fs.Attr(prefix + "device_path"); got != vol["device_path"] {
		t.Errorf("the target is exporting %q", got)
	}
	if got, _ := e.fs.Attr(prefix + "device_uuid"); got != vol["uuid"] {
		t.Errorf("the target's UUID is %q", got)
	}
	got := e.do("GET", "/v1/volumes/v", nil, 200)
	if m, _ := got["mapped_to"].(map[string]any); m["subsystem"] != subNQN+"a" {
		t.Errorf("mapped_to = %v", got["mapped_to"])
	}
	e.do("POST", "/v1/subsystems/"+subNQN+"b/namespaces", obj{"volume": "v"}, 409)
	e.do("PATCH", nsPath+"/1", obj{"device_path": "/x"}, 422)
	e.do("DELETE", nsPath+"/1?delete_file=true", nil, 422)

	// Moving it to another subsystem must keep what hosts recognise it by.
	e.do("DELETE", "/v1/volumes/v", nil, 409)
	e.do("DELETE", nsPath+"/1", nil, 204)
	moved := e.do("POST", "/v1/subsystems/"+subNQN+"b/namespaces", obj{"volume": "v"}, 201)
	if moved["uuid"] != vol["uuid"] || moved["nguid"] != vol["nguid"] {
		t.Errorf("identity changed: %v, %v", moved, vol)
	}
	if len(e.pools.files) != 1 {
		t.Errorf("unmapping should keep the volume's storage: %v", e.pools.files)
	}
}

func TestDeletingASubsystemKeepsItsVolumes(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"volume": "v"}, 201)
	e.do("DELETE", "/v1/subsystems/"+subNQN, nil, 204)
	got := e.do("GET", "/v1/volumes/v", nil, 200)
	if got["mapped_to"] != nil {
		t.Errorf("mapped_to = %v", got["mapped_to"])
	}
	e.do("DELETE", "/v1/volumes/v", nil, 204)
	if len(e.pools.files) != 0 {
		t.Errorf("deleting a volume left %v", e.pools.files)
	}
	e.do("DELETE", "/v1/pools/p", nil, 204)
}

func TestForgettingAPoolWithVolumesIsRefused(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.do("DELETE", "/v1/pools/p", nil, 409)
	e.do("DELETE", "/v1/volumes/v", nil, 204)
	e.do("DELETE", "/v1/pools/p", nil, 204)
	e.do("DELETE", "/v1/pools/p", nil, 404)
}

func TestGrowingAVolume(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 8192}, 201)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"volume": "v"}, 201)
	e.do("PATCH", "/v1/volumes/v", obj{"size_bytes": 4096}, 422)
	e.do("PATCH", "/v1/volumes/v", obj{"size_bytes": 8193}, 422)
	got := e.do("PATCH", "/v1/volumes/v", obj{"size_bytes": 16384}, 200)
	if got["size_bytes"] != float64(16384) {
		t.Errorf("size_bytes = %v", got["size_bytes"])
	}
	// Hosts are only told about the new size through the target.
	attr := "/nvmet/subsystems/" + subNQN + "/namespaces/1/revalidate_size"
	if v, ok := e.fs.Attr(attr); !ok || v != "1" {
		t.Errorf("target wasn't told: %q, %v", v, ok)
	}
	e.do("PATCH", "/v1/volumes/none", obj{"size_bytes": 16384}, 404)
}

// breakSaving makes saving the state fail, even as root: nothing can be renamed over a directory with something in it.
func (e *env) breakSaving() (restore func()) {
	e.t.Helper()
	state := filepath.Join(e.dir, "state.json")
	if err := os.Rename(state, state+".real"); err != nil {
		e.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "in-the-way"), 0o700); err != nil {
		e.t.Fatal(err)
	}
	return func() {
		if err := os.RemoveAll(state); err != nil {
			e.t.Fatal(err)
		}
		if err := os.Rename(state+".real", state); err != nil {
			e.t.Fatal(err)
		}
	}
}

// A grow that couldn't be saved keeps its intent, so that the next start finishes it.
func TestAGrowThatCantBeSavedIsFinishedLater(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 8192}, 201)
	restore := e.breakSaving()
	e.do("PATCH", "/v1/volumes/v", obj{"size_bytes": 16384}, 500)
	restore()
	if n := e.intents(); n != 1 {
		t.Fatalf("%d intents, after storage grew and the size wasn't saved", n)
	}
	e.start()
	if got := e.do("GET", "/v1/volumes/v", nil, 200); got["size_bytes"] != float64(16384) {
		t.Errorf("after a restart, size_bytes = %v", got["size_bytes"])
	}
	e.consistent()
}

// A grow whose rescan failed keeps its intent, and the same request again tells hosts.
func TestAGrowIsRetriedUntilHostsAreTold(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 8192}, 201)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"volume": "v"}, 201)
	attr := "/nvmet/subsystems/" + subNQN + "/namespaces/1/revalidate_size"
	e.fs.Fail["/namespaces/1/revalidate_size"] = syscall.EIO
	e.do("PATCH", "/v1/volumes/v", obj{"size_bytes": 16384}, 500)
	if n := e.intents(); n != 1 {
		t.Errorf("%d intents, after hosts weren't told", n)
	}
	delete(e.fs.Fail, "/namespaces/1/revalidate_size")
	if v, _ := e.fs.Attr(attr); v == "1" {
		t.Fatal("the fake recorded a write that failed")
	}
	e.do("PATCH", "/v1/volumes/v", obj{"size_bytes": 16384}, 200)
	if v, ok := e.fs.Attr(attr); !ok || v != "1" {
		t.Errorf("the retry didn't rescan: %q, %v", v, ok)
	}
	e.consistent()
}

func TestPagingThroughVolumes(t *testing.T) {
	e := newEnv(t)
	e.pool()
	for i := range 5 {
		e.do("POST", "/v1/volumes", obj{"name": fmt.Sprintf("v%d", i), "size_bytes": 4096}, 201)
	}
	var seen []string
	after := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging didn't end")
		}
		page := e.do("GET", "/v1/volumes?limit=2&after="+after, nil, 200)
		for _, it := range page["items"].([]any) {
			seen = append(seen, it.(map[string]any)["name"].(string))
		}
		next, ok := page["next_after"].(string)
		if !ok {
			break
		}
		after = next
	}
	if strings.Join(seen, ",") != "v0,v1,v2,v3,v4" {
		t.Errorf("saw %v", seen)
	}
	for _, q := range []string{"limit=0", "limit=1001", "limit=x"} {
		e.do("GET", "/v1/volumes?"+q, nil, 422)
	}
	e.do("GET", "/v1/volumes?pool=none", nil, 404)
	if got := e.do("GET", "/v1/volumes?pool=p", nil, 200); len(got["items"].([]any)) != 5 {
		t.Errorf("filtered to %d", len(got["items"].([]any)))
	}
}

func TestVolumesSurviveARestart(t *testing.T) {
	e := newEnv(t)
	e.pool()
	vol := e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"volume": "v"}, 201)
	e.fs = targettest.New()
	e.start()
	got := e.do("GET", "/v1/volumes/v", nil, 200)
	if got["uuid"] != vol["uuid"] || got["internal_name"] != vol["internal_name"] {
		t.Errorf("identity changed: %v, %v", got, vol)
	}
	if v, _ := e.fs.Attr("/nvmet/subsystems/" + subNQN + "/namespaces/1/enable"); v != "1" {
		t.Errorf("namespace not restored: %q", v)
	}
}

func TestFailedMappingLeavesTheVolumeUnmapped(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.fs.Fail["/namespaces/1/enable"] = syscall.EINVAL
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"nsid": 1, "volume": "v"}, 422)
	delete(e.fs.Fail, "/namespaces/1/enable")
	if got := e.do("GET", "/v1/volumes/v", nil, 200); got["mapped_to"] != nil {
		t.Errorf("mapped_to = %v", got["mapped_to"])
	}
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"nsid": 1, "volume": "v"}, 201)
}

func TestAVolumesFileCantBeExportedDirectly(t *testing.T) {
	e := newEnv(t)
	e.pool()
	vol := e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/subsystems", obj{"nqn": subNQN}, 201)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"device_path": vol["device_path"]}, 409)
	e.do("POST", "/v1/subsystems/"+subNQN+"/namespaces", obj{"volume": "v"}, 201)
}

func TestSnapshotsAndClones(t *testing.T) {
	e := newEnv(t)
	e.pool()
	vol := e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 8192}, 201)
	if vol["source"] != nil {
		t.Errorf("a new volume has no source: %v", vol["source"])
	}

	e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "nope"}, 422)
	e.do("POST", "/v1/snapshots", obj{"name": "../s", "volume": "v"}, 422)
	snap := e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 201)
	if snap["pool"] != "p" || snap["size_bytes"] != float64(8192) || snap["state"] != "online" {
		t.Errorf("snapshot = %v", snap)
	}
	again := e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 200)
	if again["internal_name"] != snap["internal_name"] {
		t.Errorf("a retry should find the same snapshot: %v, %v", snap, again)
	}
	e.do("POST", "/v1/volumes", obj{"name": "other", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "other"}, 409)
	files := len(e.pools.files)

	// A clone's source: exactly one, which exists, in its own pool, at no less than its size.
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{}}, 422)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s", "volume": "v"}}, 422)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "nope"}}, 422)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"volume": "nope"}}, 422)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}, "size_bytes": 4096}, 422)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}, "size_bytes": 8193}, 422)
	e.do("POST", "/v1/pools", obj{"name": "q", "type": "reflink", "path": "/q"}, 201)
	e.do("POST", "/v1/volumes", obj{"name": "c", "pool": "q", "source": obj{"snapshot": "s"}}, 422)
	if len(e.pools.files) != files {
		t.Errorf("a refused clone allocated storage: %v", e.pools.files)
	}

	clone := e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}}, 201)
	if clone["size_bytes"] != float64(8192) || clone["pool"] != "p" {
		t.Errorf("a clone defaults to its source's size and pool: %v", clone)
	}
	if clone["uuid"] == vol["uuid"] || clone["nguid"] == vol["nguid"] || clone["internal_name"] == vol["internal_name"] {
		t.Errorf("a clone needs its own identity, or hosts reject it as a duplicate: %v, %v", clone, vol)
	}
	if fmt.Sprint(clone["source"]) != "map[snapshot:s]" {
		t.Errorf("source = %v", clone["source"])
	}
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}}, 200)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}, "size_bytes": 8192}, 200)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}, "size_bytes": 16384}, 409)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"volume": "v"}}, 409)
	e.do("POST", "/v1/volumes", obj{"name": "c", "pool": "p", "size_bytes": 8192}, 409)
	big := e.do("POST", "/v1/volumes", obj{"name": "big", "source": obj{"volume": "v"}, "size_bytes": 16384}, 201)
	if big["size_bytes"] != float64(16384) {
		t.Errorf("a clone may be larger: %v", big)
	}
}

func TestSnapshotOutlivesItsVolume(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 201)
	e.do("DELETE", "/v1/volumes/v", nil, 204)

	got := e.do("GET", "/v1/snapshots/s", nil, 200)
	if got["volume"] != "v" {
		t.Errorf("snapshot = %v", got)
	}
	// CSI may retry after the volume has gone.
	e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 200)
	e.do("POST", "/v1/snapshots", obj{"name": "t", "volume": "v"}, 422)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}}, 201)

	// Forgetting a pool would lose track of the snapshot's data.
	e.do("DELETE", "/v1/volumes/c", nil, 204)
	if got := e.do("DELETE", "/v1/pools/p", nil, 409); !strings.Contains(fmt.Sprint(got["error"]), "snapshots") {
		t.Errorf("error = %v", got["error"])
	}
	// A clone also keeps working after its snapshot goes, and retrying it finds it.
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}}, 201)
	e.do("DELETE", "/v1/snapshots/s", nil, 204)
	e.do("DELETE", "/v1/snapshots/s", nil, 404)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}}, 200)
	e.do("POST", "/v1/volumes", obj{"name": "d", "source": obj{"snapshot": "s"}}, 422)
	e.do("DELETE", "/v1/volumes/c", nil, 204)
	if len(e.pools.files) != 0 {
		t.Errorf("left on disk: %v", e.pools.files)
	}
	e.do("DELETE", "/v1/pools/p", nil, 204)
}

func TestListingSnapshots(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/volumes", obj{"name": "w", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/snapshots", obj{"name": "a", "volume": "v"}, 201)
	e.do("POST", "/v1/snapshots", obj{"name": "b", "volume": "w"}, 201)
	e.do("POST", "/v1/snapshots", obj{"name": "c", "volume": "v"}, 201)

	names := func(path string) string {
		got := e.do("GET", path, nil, 200)
		var out []string
		for _, item := range got["items"].([]any) {
			out = append(out, item.(map[string]any)["name"].(string))
		}
		return fmt.Sprintf("%v next=%v", out, got["next_after"])
	}
	for path, want := range map[string]string{
		"/v1/snapshots":                  "[a b c] next=<nil>",
		"/v1/snapshots?limit=2":          "[a b] next=b",
		"/v1/snapshots?limit=2&after=b":  "[c] next=<nil>",
		"/v1/snapshots?volume=v":         "[a c] next=<nil>",
		"/v1/snapshots?volume=v&limit=1": "[a] next=a",
		"/v1/snapshots?volume=gone":      "[] next=<nil>",
	} {
		if got := names(path); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	e.do("GET", "/v1/snapshots?limit=0", nil, 422)

	pool := e.do("GET", "/v1/pools/p", nil, 200)
	if pool["snapshots"] != float64(3) || pool["volumes"] != float64(2) {
		t.Errorf("pool = %v", pool)
	}
	if strings.Contains(fmt.Sprint(pool["warnings"]), "no volume refers") {
		t.Errorf("snapshots aren't strays: %v", pool["warnings"])
	}
	if got := e.do("GET", "/v1/system", nil, 200); got["snapshots"] != float64(3) {
		t.Errorf("system = %v", got)
	}
}

func TestSnapshotFileCantBeExportedDirectly(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/subsystems", obj{"nqn": "nqn.2026-11.test:a"}, 201)
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	snap := e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 201)
	got := e.do("POST", "/v1/subsystems/nqn.2026-11.test:a/namespaces", obj{"device_path": snap["device_path"]}, 409)
	if !strings.Contains(fmt.Sprint(got["error"]), "snapshot s") {
		t.Errorf("error = %v", got["error"])
	}
}

func TestSnapshotsAndClonesFullPool(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	e.pools.createErr = fmt.Errorf("clone: %w", syscall.ENOSPC)
	e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 507)
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"volume": "v"}}, 507)
	e.pools.createErr = nil
	if got := e.do("GET", "/v1/system", nil, 200); got["snapshots"] != float64(0) || got["volumes"] != float64(1) {
		t.Errorf("a failed copy must leave nothing behind: %v", got)
	}
}

func TestSnapshotsSurviveARestart(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	snap := e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 201)
	clone := e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"snapshot": "s"}}, 201)
	e.start()
	if got := e.do("GET", "/v1/snapshots/s", nil, 200); got["internal_name"] != snap["internal_name"] {
		t.Errorf("snapshot = %v", got)
	}
	if got := e.do("GET", "/v1/volumes/c", nil, 200); fmt.Sprint(got["source"]) != "map[snapshot:s]" || got["nguid"] != clone["nguid"] {
		t.Errorf("clone = %v", got)
	}
}

func TestPortTransport(t *testing.T) {
	e := newEnv(t)
	got := e.do("POST", "/v1/ports", obj{"id": 1, "address": "127.0.0.1", "service": 4420}, 201)
	if got["transport"] != "tcp" {
		t.Errorf("transport defaults to tcp: %v", got)
	}
	e.do("POST", "/v1/ports", obj{"id": 2, "address": "127.0.0.1", "service": 4421, "transport": "tcp"}, 201)
	// A setting that can't be honoured is refused, never ignored.
	e.do("POST", "/v1/ports", obj{"id": 3, "address": "127.0.0.1", "service": 4422, "transport": "rdma"}, 422)
	e.do("PATCH", "/v1/ports/1", obj{"transport": "rdma"}, 422)
	if got := e.do("GET", "/v1/ports/1", nil, 200); got["transport"] != "tcp" {
		t.Errorf("a refused change must not stick: %v", got)
	}
	caps := e.do("GET", "/v1/system", nil, 200)["capabilities"].(map[string]any)
	if fmt.Sprint(caps["transports"]) != "[tcp]" {
		t.Errorf("capabilities = %v", caps)
	}
}

func TestPortsSavedBeforeTransportsExistedAreTCP(t *testing.T) {
	st := &target.State{Ports: map[uint16]*target.Port{1: {ID: 1, Address: "127.0.0.1", Service: 4420}}}
	st.Normalize()
	if st.Ports[1].Transport != target.TransportTCP {
		t.Errorf("transport = %q", st.Ports[1].Transport)
	}
}

func TestSizesThePoolRoundsUp(t *testing.T) {
	e := newEnv(t)
	e.pools.alignTo = 4194304
	e.pool()
	got := e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	if got["size_bytes"] != float64(4194304) {
		t.Errorf("a volume reports its real size: %v", got["size_bytes"])
	}
	// CSI retries with what it first asked for, which must still match.
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 200)
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4194304}, 200)
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 8388608}, 409)
	// A clone's size is rounded before comparing it with the source.
	e.do("POST", "/v1/volumes", obj{"name": "c", "source": obj{"volume": "v"}, "size_bytes": 4096}, 201)
	// Growing within an extent changes nothing, and beyond it rounds up.
	if got := e.do("PATCH", "/v1/volumes/v", obj{"size_bytes": 4194304}, 200); got["size_bytes"] != float64(4194304) {
		t.Errorf("size_bytes = %v", got["size_bytes"])
	}
	if got := e.do("PATCH", "/v1/volumes/v", obj{"size_bytes": 4198400}, 200); got["size_bytes"] != float64(8388608) {
		t.Errorf("size_bytes = %v", got["size_bytes"])
	}
	e.consistent()
}

// A volume or snapshot whose storage can't be found says so, as a pool does.
func TestMissingStorageIsReported(t *testing.T) {
	e := newEnv(t)
	e.pool()
	vol := e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 201)
	snap := e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 201)
	if vol["state"] != "online" || snap["state"] != "online" {
		t.Fatalf("states: %v, %v", vol["state"], snap["state"])
	}
	for _, path := range []any{vol["device_path"], snap["device_path"]} {
		if err := os.Rename(path.(string), path.(string)+".away"); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.do("GET", "/v1/volumes/v", nil, 200)["state"]; got != "missing" {
		t.Errorf("volume = %v", got)
	}
	if got := e.do("GET", "/v1/snapshots/s", nil, 200)["state"]; got != "missing" {
		t.Errorf("snapshot = %v", got)
	}
	for _, list := range []string{"/v1/volumes", "/v1/snapshots"} {
		items := e.do("GET", list, nil, 200)["items"].([]any)
		if got := items[0].(map[string]any)["state"]; got != "missing" {
			t.Errorf("%s = %v", list, got)
		}
	}
	for _, path := range []any{vol["device_path"], snap["device_path"]} {
		if err := os.Rename(path.(string)+".away", path.(string)); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.do("GET", "/v1/volumes/v", nil, 200)["state"]; got != "online" {
		t.Errorf("volume back = %v", got)
	}
}

// Two names for one directory are one pool, and so are a symlink to it and a path spelt another way.
func TestOneDirectoryIsOnePool(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	e.do("POST", "/v1/pools", obj{"name": "p", "type": "reflink", "path": dir}, 201)
	for _, path := range []string{link, dir + "/", dir + "/.", filepath.Dir(dir) + "/../" + filepath.Base(filepath.Dir(dir)) + "/" + filepath.Base(dir)} {
		e.do("POST", "/v1/pools", obj{"name": "q", "type": "reflink", "path": path}, 409)
	}
	e.do("POST", "/v1/pools", obj{"name": "p", "type": "reflink", "path": link}, 200)
	e.do("POST", "/v1/pools", obj{"name": "q", "type": "reflink", "path": t.TempDir()}, 201)
}

// Creating a volume again with other labels is a different request, not a retry.
func TestARetryMustHaveTheSameLabels(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096, "labels": obj{"app": "a"}}, 201)
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096, "labels": obj{"app": "a"}}, 200)
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096, "labels": obj{"app": "b"}}, 409)
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 4096}, 409)
	e.do("POST", "/v1/volumes", obj{"name": "w", "size_bytes": 4096}, 201)
	e.do("POST", "/v1/volumes", obj{"name": "w", "size_bytes": 4096, "labels": obj{}}, 200) // none and empty are the same
}
