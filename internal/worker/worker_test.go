package worker

import "testing"

func TestGuardExitsAfterThreshold(t *testing.T) {
	g := &guard{threshold: 3}
	for i := 0; i < 5; i++ {
		if g.observe(true) {
			t.Fatal("should not exit while present")
		}
	}
	if g.observe(false) || g.observe(false) {
		t.Fatal("should not exit before threshold")
	}
	if !g.observe(false) {
		t.Fatal("should exit at threshold")
	}
}

func TestGuardResetsOnRecovery(t *testing.T) {
	g := &guard{threshold: 3}
	g.observe(false)
	g.observe(false)
	g.observe(true) // recovery resets
	if g.observe(false) {
		t.Fatal("counter should have reset on recovery")
	}
}
