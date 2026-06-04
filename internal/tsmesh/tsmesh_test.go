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

func TestSplitTags(t *testing.T) {
	got := splitTags("tag:ci-distcc, tag:extra")
	if len(got) != 2 || got[0] != "tag:ci-distcc" || got[1] != "tag:extra" {
		t.Fatalf("got %+v", got)
	}
	if splitTags("") != nil {
		t.Fatal("empty should be nil")
	}
}
