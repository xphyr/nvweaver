package driver

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"testing"

	"nvweaver/internal/client"
)

// janitorAPI is nvweaver's subsystems and hosts, as the janitor sees them, and what it did to them.
type janitorAPI struct {
	subs     map[string]client.Subsystem
	hosts    map[string]*client.Host
	notMine  map[string]bool // hosts the driver's token doesn't own
	did      []string
	listFail error
}

func (a *janitorAPI) ListSubsystems(context.Context) ([]client.Subsystem, error) {
	if a.listFail != nil {
		return nil, a.listFail
	}
	return slices.Collect(maps.Values(a.subs)), nil
}
func (a *janitorAPI) DeleteSubsystem(_ context.Context, nqn string) error {
	a.did = append(a.did, "delete subsystem "+nqn)
	delete(a.subs, nqn)
	return nil
}
func (a *janitorAPI) GetHost(_ context.Context, nqn string) (*client.Host, error) {
	if h, ok := a.hosts[nqn]; ok {
		c := *h
		c.Labels = maps.Clone(h.Labels)
		return &c, nil
	}
	return nil, &client.Error{Status: http.StatusNotFound}
}
func (a *janitorAPI) UpdateHost(_ context.Context, nqn string, in client.HostUpdate) (*client.Host, error) {
	if a.notMine[nqn] {
		return nil, &client.Error{Status: http.StatusForbidden, Message: "belongs to no scoped token"}
	}
	a.did = append(a.did, "label "+nqn+" "+(*in.Labels)[labelNode])
	a.hosts[nqn].Labels = maps.Clone(*in.Labels)
	return a.hosts[nqn], nil
}
func (a *janitorAPI) DeleteHost(_ context.Context, nqn string) error {
	if a.notMine[nqn] {
		return &client.Error{Status: http.StatusForbidden}
	}
	a.did = append(a.did, "delete host "+nqn)
	delete(a.hosts, nqn)
	return nil
}

func janitorFixture() (*janitorAPI, map[string]string) {
	a := &janitorAPI{subs: map[string]client.Subsystem{}, hosts: map[string]*client.Host{}, notMine: map[string]bool{}}
	for _, h := range []string{nodeA, nodeB} {
		a.hosts[h] = &client.Host{NQN: h, Labels: map[string]string{labelCluster: "lab"}}
		sub := testCfg.nodeSubsystem(h)
		a.subs[sub] = client.Subsystem{NQN: sub, AllowedHosts: []string{h}}
	}
	// Another cluster's node, which this one must never touch.
	other := "nqn.2026-11.io.github.xphyr.nvweaver:csi:prod:node-0123456789abcdef"
	a.subs[other] = client.Subsystem{NQN: other, AllowedHosts: []string{"nqn.2014-08.org.nvmexpress:uuid:prod"}}
	return a, map[string]string{nodeA: "worker-a", nodeB: "worker-b"}
}

func TestTheJanitorLabelsHostsWithTheirNodes(t *testing.T) {
	a, nodes := janitorFixture()
	j := NewJanitor(testNodeCfg(), a, func(context.Context) (map[string]string, error) { return nodes, nil })
	j.Check(context.Background())
	slices.Sort(a.did)
	if want := []string{"label " + nodeA + " worker-a", "label " + nodeB + " worker-b"}; !slices.Equal(a.did, want) {
		t.Errorf("did %v, want %v", a.did, want)
	}
	if got := a.hosts[nodeA].Labels; got[labelCluster] != "lab" || got[labelNode] != "worker-a" {
		t.Errorf("labels %v", got)
	}
	// Already right: nothing to do.
	a.did = nil
	j.Check(context.Background())
	if len(a.did) != 0 {
		t.Errorf("labelled again: %v", a.did)
	}
	// A host the driver doesn't own, made before hosts had owners, is reported and left.
	a.hosts[nodeA].Labels = nil
	a.notMine[nodeA] = true
	j.Check(context.Background())
	if a.hosts[nodeA].Labels != nil {
		t.Errorf("a host it doesn't own was changed: %v", a.hosts[nodeA].Labels)
	}
}

func TestTheJanitorForgetsNodesThatHaveLeft(t *testing.T) {
	a, nodes := janitorFixture()
	j := NewJanitor(testNodeCfg(), a, func(context.Context) (map[string]string, error) { return nodes, nil })
	j.Check(context.Background())
	a.did = nil
	delete(nodes, nodeB) // Node B leaves the cluster.

	j.Check(context.Background())
	if len(a.did) != 0 {
		t.Errorf("acted on the first miss, which could be a node registering again: %v", a.did)
	}
	j.Check(context.Background())
	subB := testCfg.nodeSubsystem(nodeB)
	if want := []string{"delete subsystem " + subB, "delete host " + nodeB}; !slices.Equal(a.did, want) {
		t.Errorf("did %v, want %v", a.did, want)
	}
	if _, ok := a.subs[testCfg.nodeSubsystem(nodeA)]; !ok {
		t.Error("the node that's still here lost its subsystem")
	}
	if len(a.subs) != 2 {
		t.Errorf("another cluster's subsystem was touched: %v", slices.Collect(maps.Keys(a.subs)))
	}
}

func TestTheJanitorKeepsANodeWithVolumes(t *testing.T) {
	a, nodes := janitorFixture()
	subB := testCfg.nodeSubsystem(nodeB)
	s := a.subs[subB]
	s.Namespaces = []client.Namespace{{NSID: 1}}
	a.subs[subB] = s
	delete(nodes, nodeB)
	j := NewJanitor(testNodeCfg(), a, func(context.Context) (map[string]string, error) { return nodes, nil })
	for range 3 {
		j.Check(context.Background())
	}
	if _, ok := a.subs[subB]; !ok || slices.ContainsFunc(a.did, func(d string) bool { return d != "label "+nodeA+" worker-a" }) {
		t.Errorf("a departed node with volumes still exported was changed: %v", a.did)
	}
	// Once Kubernetes unpublishes them, it goes.
	s.Namespaces = nil
	a.subs[subB] = s
	j.Check(context.Background())
	if _, ok := a.subs[subB]; ok {
		t.Errorf("not forgotten once empty: %v", a.did)
	}
}

func TestTheJanitorDoesNothingWhenItCantTell(t *testing.T) {
	for name, nodes := range map[string]func(context.Context) (map[string]string, error){
		"the list fails": func(context.Context) (map[string]string, error) { return nil, errors.New("forbidden") },
		"no node has it": func(context.Context) (map[string]string, error) { return map[string]string{}, nil },
	} {
		a, _ := janitorFixture()
		j := NewJanitor(testNodeCfg(), a, nodes)
		for range 3 {
			j.Check(context.Background())
		}
		if len(a.did) != 0 || len(a.subs) != 3 {
			t.Errorf("%s: did %v", name, a.did)
		}
	}
}
