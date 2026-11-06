package target

import (
	"encoding/json"
	"reflect"
	"testing"
)

// fill sets every field it reaches, so that a field added later that Clone misses shows up here.
func fill(v reflect.Value, n *int) {
	*n++
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), n)
	case reflect.Struct:
		for i := range v.NumField() {
			fill(v.Field(i), n)
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key := reflect.New(v.Type().Key()).Elem()
		fill(key, n)
		elem := reflect.New(v.Type().Elem()).Elem()
		fill(elem, n)
		v.SetMapIndex(key, elem)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), n)
	case reflect.String:
		v.SetString("x" + reflect.ValueOf(*n).String())
	case reflect.Int, reflect.Int64:
		v.SetInt(int64(*n))
	case reflect.Uint16, reflect.Uint32:
		v.SetUint(uint64(*n))
	case reflect.Bool:
		v.SetBool(true)
	default:
		panic("fill doesn't handle " + v.Kind().String())
	}
}

// references collects every pointer, map and slice, to find any that two values share.
func references(v reflect.Value, out map[uintptr]string, path string) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			out[v.Pointer()] = path
			references(v.Elem(), out, path)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			references(v.Field(i), out, path+"."+v.Type().Field(i).Name)
		}
	case reflect.Map:
		if !v.IsNil() {
			out[v.Pointer()] = path
			for _, k := range v.MapKeys() {
				references(v.MapIndex(k), out, path+"[]")
			}
		}
	case reflect.Slice:
		if !v.IsNil() {
			out[v.Pointer()] = path
			for i := range v.Len() {
				references(v.Index(i), out, path+"[]")
			}
		}
	}
}

func TestCloneCopiesEverything(t *testing.T) {
	st := &State{}
	n := 0
	fill(reflect.ValueOf(st).Elem(), &n)
	// Normalize sets a default, which is the one thing Clone can change.
	st.Normalize()
	c := st.Clone()
	want, _ := json.Marshal(st)
	got, _ := json.Marshal(c)
	if string(want) != string(got) {
		t.Errorf("Clone lost something:\n got %s\nwant %s", got, want)
	}
	if !reflect.DeepEqual(st, c) {
		t.Error("Clone isn't deeply equal")
	}
	theirs, ours := map[uintptr]string{}, map[uintptr]string{}
	references(reflect.ValueOf(st).Elem(), theirs, "state")
	references(reflect.ValueOf(c).Elem(), ours, "state")
	if len(theirs) == 0 {
		t.Fatal("found no references, so this checks nothing")
	}
	for p, path := range ours {
		if theirs[p] != "" {
			t.Errorf("Clone shares %s with the original", path)
		}
	}
}

func TestCloneOfAnEmptyStateIsUsable(t *testing.T) {
	c := (&State{}).Clone()
	c.Volumes["v"] = &Volume{}
	c.Snapshots["s"] = &Snapshot{}
	c.Ports[1] = &Port{}
}
