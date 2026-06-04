package config

import "testing"

func TestLoadCoordinatorDefaults(t *testing.T) {
	env := map[string]string{
		"INPUT_MODE":             "coordinator",
		"INPUT_OAUTH_CLIENT_ID":  "id",
		"INPUT_OAUTH_SECRET":     "sec",
		"INPUT_EXPECTED_WORKERS": "3",
		"GITHUB_RUN_ID":          "999",
	}
	c, err := loadFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.Mode != "coordinator" {
		t.Errorf("mode=%q", c.Mode)
	}
	if c.ExpectedWorkers != 3 {
		t.Errorf("expected=%d", c.ExpectedWorkers)
	}
	if c.MinWorkers != 3 { // defaults to expected
		t.Errorf("min defaulted wrong: %d", c.MinWorkers)
	}
	if c.RunPrefix != "999" {
		t.Errorf("prefix=%q", c.RunPrefix)
	}
	if c.Tags != "tag:ci-distcc" {
		t.Errorf("tags default=%q", c.Tags)
	}
	if c.WaitTimeout.Seconds() != 300 {
		t.Errorf("wait-timeout default=%v", c.WaitTimeout)
	}
	if !c.LZO {
		t.Errorf("lzo should default true")
	}
	if c.Pump {
		t.Errorf("pump should default false")
	}
}

func TestMissingOAuthFails(t *testing.T) {
	env := map[string]string{"INPUT_MODE": "worker"}
	if _, err := loadFrom(func(k string) string { return env[k] }); err == nil {
		t.Fatal("expected error for missing oauth")
	}
}
