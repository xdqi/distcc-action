package distccrun

import "testing"

func TestBuildHosts(t *testing.T) {
	got := BuildHosts([]string{"3701", "3702"}, 4, true, false)
	want := "127.0.0.1:3701/4,lzo 127.0.0.1:3702/4,lzo"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBuildHostsNoLZO(t *testing.T) {
	got := BuildHosts([]string{"3701"}, 2, false, false)
	want := "127.0.0.1:3701/2"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestTotalJ(t *testing.T) {
	if j := TotalJ(3, 4); j != 12 {
		t.Errorf("j=%d want 12", j)
	}
}
