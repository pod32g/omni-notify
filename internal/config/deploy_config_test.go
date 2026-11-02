package config

import "testing"

// The config chizu runs (config.docker.yml) must load: CI tests before it
// deploys, so a broken file never reaches the server.
func TestDeployConfigLoads(t *testing.T) {
	t.Setenv("OMNI_NOTIFY_API_TOKEN", "test-token")
	t.Setenv("OMNI_NOTIFY_ENCRYPTION_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	t.Setenv("OMNI_LOGGING_API_KEY", "test-key")
	c, err := Load("../../config.docker.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Voice.Enabled || c.Voice.APNS.Provider != "oncall-push" || len(c.Voice.Users) != 1 {
		t.Fatalf("voice = %+v", c.Voice)
	}
}
