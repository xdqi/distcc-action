package tsmesh

import "testing"

func TestFilterOnlineByPrefix(t *testing.T) {
	peers := []Peer{
		{Host: "run9-worker-1", Online: true, IP: "100.0.0.1"},
		{Host: "run9-worker-2", Online: false, IP: "100.0.0.2"},
		{Host: "run9-coordinator", Online: true, IP: "100.0.0.9"},
		{Host: "someones-laptop", Online: true, IP: "100.0.0.5"},
	}
	got := FilterOnline(peers, "run9-worker-")
	if len(got) != 1 || got[0].IP != "100.0.0.1" {
		t.Fatalf("got %+v", got)
	}
}

func TestPeerPresentOnline(t *testing.T) {
	peers := []Peer{{Host: "run9-coordinator", Online: true, IP: "100.0.0.9"}}
	if !PresentOnline(peers, "run9-coordinator") {
		t.Fatal("should be present+online")
	}
	if PresentOnline(peers, "run9-missing") {
		t.Fatal("missing should not be present")
	}
}

func TestWorkersReady(t *testing.T) {
	// Before timeout: only ready once we reach the FULL expected count, so slow
	// workers can still join the bus instead of being left out.
	if workersReady(1, 2, 1, false) {
		t.Error("1/2 online before timeout: should keep waiting (min is a floor, not a trigger)")
	}
	if !workersReady(2, 2, 1, false) {
		t.Error("2/2 online before timeout: should be ready")
	}
	// After timeout: accept the min-workers floor.
	if !workersReady(1, 2, 1, true) {
		t.Error("1/2 online after timeout with min=1: should proceed")
	}
	if workersReady(0, 2, 1, true) {
		t.Error("0 online after timeout with min=1: should NOT proceed (fail)")
	}
	// min == expected: timeout doesn't lower the bar below min.
	if workersReady(1, 2, 2, true) {
		t.Error("1/2 after timeout with min=2: should NOT proceed")
	}
}

func TestSplitTags(t *testing.T) {
	got := splitTags("tag:ci-distcc, tag:extra")
	if len(got) != 2 || got[0] != "tag:ci-distcc" || got[1] != "tag:extra" {
		t.Fatalf("got %+v", got)
	}
	if splitTags("") != nil {
		t.Fatal("empty should be nil")
	}
}
