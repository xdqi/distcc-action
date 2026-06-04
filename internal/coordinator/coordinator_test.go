package coordinator

import (
	"os"
	"strings"
	"testing"
)

func TestWriteGithubEnv(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "env")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeGithubEnv(f.Name(), "127.0.0.1:3701,lzo", 8, 2); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Name())
	s := string(b)
	for _, want := range []string{
		"DISTCC_HOSTS=127.0.0.1:3701,lzo",
		"DISTCC_J=8",
		"DISTCC_WORKERS_ONLINE=2",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestWriteOutputs(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "out")
	if err := writeOutputs(f.Name(), "H", 8, 2); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Name())
	if !strings.Contains(string(b), "distcc-hosts=H") || !strings.Contains(string(b), "workers-online=2") {
		t.Errorf("outputs wrong:\n%s", b)
	}
}
