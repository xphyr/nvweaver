package api

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"nvweaver/internal/storage"
)

func ranges(t *testing.T, page map[string]any) [][2]int64 {
	t.Helper()
	var out [][2]int64
	for _, r := range page["ranges"].([]any) {
		m := r.(map[string]any)
		out = append(out, [2]int64{int64(m["offset"].(float64)), int64(m["length"].(float64))})
	}
	return out
}

func TestSnapshotAllocatedRanges(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 1 << 20}, 201)
	snap := e.do("POST", "/v1/snapshots", obj{"name": "s", "volume": "v"}, 201)
	const k = 64 << 10
	// The last range runs past the snapshot's end, as a pool's block can.
	e.pools.blocks = storage.Blocks{BlockSize: k, Ranges: []storage.Range{{Offset: 0, Length: k}, {Offset: 4 * k, Length: 2 * k}, {Offset: 15 * k, Length: 2 * k}}}

	page := e.do("GET", "/v1/snapshots/s/allocated", nil, 200)
	if page["block_size"] != float64(k) || page["size_bytes"] != float64(1<<20) || page["next_offset"] != nil {
		t.Errorf("page %v", page)
	}
	if got := ranges(t, page); !slices.Equal(got, [][2]int64{{0, k}, {4 * k, 2 * k}, {15 * k, k}}) {
		t.Errorf("ranges %v", got)
	}
	if !slices.Equal(e.pools.asked, []string{snap["internal_name"].(string)}) {
		t.Errorf("asked about %v", e.pools.asked)
	}

	// A page at a time, and from an offset, which cuts the range that holds it.
	page = e.do("GET", "/v1/snapshots/s/allocated?limit=1", nil, 200)
	if got := ranges(t, page); len(got) != 1 || page["next_offset"] != float64(4*k) {
		t.Errorf("first page: %v, next %v", got, page["next_offset"])
	}
	page = e.do("GET", fmt.Sprintf("/v1/snapshots/s/allocated?offset=%d&limit=1", 5*k), nil, 200)
	if got := ranges(t, page); !slices.Equal(got, [][2]int64{{5 * k, k}}) || page["next_offset"] != float64(15*k) {
		t.Errorf("from an offset: %v, next %v", got, page["next_offset"])
	}
	page = e.do("GET", fmt.Sprintf("/v1/snapshots/s/allocated?offset=%d", 1<<20), nil, 200)
	if got := ranges(t, page); len(got) != 0 || page["next_offset"] != nil {
		t.Errorf("past the end: %v", page)
	}

	e.do("GET", "/v1/snapshots/s/allocated?offset=-1", nil, 422)
	e.do("GET", "/v1/snapshots/s/allocated?limit=0", nil, 422)
	e.do("GET", "/v1/snapshots/s/allocated?limit=10001", nil, 422)
	e.do("GET", "/v1/snapshots/nope/allocated", nil, 404)
	e.pools.blocksErr = errors.New("thin_dump: no such device")
	e.do("GET", "/v1/snapshots/s/allocated", nil, 500)
}

func TestSnapshotChanges(t *testing.T) {
	e := newEnv(t)
	e.pool()
	e.do("POST", "/v1/volumes", obj{"name": "v", "size_bytes": 1 << 20}, 201)
	e.do("POST", "/v1/volumes", obj{"name": "w", "size_bytes": 1 << 20}, 201)
	s1 := e.do("POST", "/v1/snapshots", obj{"name": "s1", "volume": "v"}, 201)
	s2 := e.do("POST", "/v1/snapshots", obj{"name": "s2", "volume": "v"}, 201)
	e.do("POST", "/v1/snapshots", obj{"name": "other", "volume": "w"}, 201)
	e.pools.blocks = storage.Blocks{BlockSize: 4096, Ranges: []storage.Range{{Offset: 8192, Length: 4096}}}

	page := e.do("GET", "/v1/snapshots/s2/changes?base=s1", nil, 200)
	if got := ranges(t, page); !slices.Equal(got, [][2]int64{{8192, 4096}}) || page["block_size"] != float64(4096) {
		t.Errorf("page %v", page)
	}
	// The base first, then the snapshot asked about.
	if !slices.Equal(e.pools.asked, []string{s1["internal_name"].(string), s2["internal_name"].(string)}) {
		t.Errorf("asked about %v", e.pools.asked)
	}
	e.do("GET", "/v1/snapshots/s1/changes?base=s2", nil, 200) // Either may be the older.

	e.pools.asked = nil
	e.do("GET", "/v1/snapshots/s2/changes", nil, 422)
	e.do("GET", "/v1/snapshots/s2/changes?base=nope", nil, 404)
	e.do("GET", "/v1/snapshots/nope/changes?base=s1", nil, 404)
	e.do("GET", "/v1/snapshots/s2/changes?base=other", nil, 422) // Another volume's.
	if e.pools.asked != nil {
		t.Errorf("the storage was asked about %v for a refused request", e.pools.asked)
	}
	e.pools.blocksErr = errors.New("thin_delta: bad")
	e.do("GET", "/v1/snapshots/s2/changes?base=s1", nil, 500)
}

// Reading is open to scoped tokens, as all reads are: decision 46.
func TestScopedTokensReadChanges(t *testing.T) {
	e := newScopedEnv(t)
	e.as(e.lab, "POST", "/v1/volumes", obj{"name": "lab-a", "size_bytes": 4096}, 201)
	e.as(e.lab, "POST", "/v1/snapshots", obj{"name": "lab-s1", "volume": "lab-a"}, 201)
	e.as(e.lab, "POST", "/v1/snapshots", obj{"name": "lab-s2", "volume": "lab-a"}, 201)
	e.as(e.other, "GET", "/v1/snapshots/lab-s2/changes?base=lab-s1", nil, 200)
	e.as(e.lab, "GET", "/v1/snapshots/lab-s1/allocated", nil, 200)
}
