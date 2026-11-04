package api

import (
	"fmt"
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
