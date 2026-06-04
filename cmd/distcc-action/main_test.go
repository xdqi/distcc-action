package main

import "testing"

func TestPhaseFromArgs(t *testing.T) {
	if phaseFromArgs([]string{"distcc-action"}) != "main" {
		t.Error("default should be main")
	}
	if phaseFromArgs([]string{"distcc-action", "--teardown"}) != "teardown" {
		t.Error("--teardown should be teardown")
	}
}
