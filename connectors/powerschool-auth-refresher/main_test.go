package main

import (
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// minimalEnv is what the chart always provides.
func minimalEnv() map[string]string {
	return map[string]string{
		"POWERSCHOOL_HOSTNAME": "portal.example.org",
		"POWERSCHOOL_USERNAME": testUsername,
		"POWERSCHOOL_PASSWORD": testPassword,
		"VAULT_ADDR":           "https://vault.vault.svc.cluster.local:8200",
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, problems := loadConfig(envOf(minimalEnv()))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if cfg.BaseURL != "https://portal.example.org" || cfg.Host != "portal.example.org" {
		t.Errorf("portal = %q / %q", cfg.BaseURL, cfg.Host)
	}
	if cfg.VaultK8sRole != "glovebox-powerschool-refresher" {
		t.Errorf("role = %q", cfg.VaultK8sRole)
	}
	if cfg.VaultKVMount != "secret" || cfg.VaultSessionPath != "glovebox/powerschool/default/session" {
		t.Errorf("vault location = %q / %q", cfg.VaultKVMount, cfg.VaultSessionPath)
	}
	if cfg.Blackout.String() != "16:00-21:00" || cfg.Blackout.loc.String() != "America/Los_Angeles" {
		t.Errorf("blackout = %s %s", cfg.Blackout, cfg.Blackout.loc)
	}
	if cfg.MinSessionAge != 10*time.Hour {
		t.Errorf("min session age = %s, want 10h", cfg.MinSessionAge)
	}
	if cfg.SessionLifetime != 36*time.Hour {
		t.Errorf("session lifetime = %s", cfg.SessionLifetime)
	}
	if cfg.ChromeNoSandbox {
		t.Error("no-sandbox defaults on")
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	env := minimalEnv()
	delete(env, "POWERSCHOOL_HOSTNAME")
	env["POWERSCHOOL_HOST"] = "https://Portal.Example.org/public/home.html"
	env["VAULT_K8S_ROLE"] = "other-role"
	env["VAULT_KV_MOUNT"] = "/kv/"
	env["VAULT_SESSION_PATH"] = "/a/b/session/"
	env["VAULT_CACERT"] = "/etc/vault-ca/tls-ca-bundle.pem"
	env["POWERSCHOOL_BLACKOUT_START"] = "15:30"
	env["POWERSCHOOL_BLACKOUT_END"] = "22:00"
	env["POWERSCHOOL_BLACKOUT_TZ"] = "America/Denver"
	env["POWERSCHOOL_MIN_SESSION_AGE"] = "30h"
	env["POWERSCHOOL_SESSION_LIFETIME"] = "48h"
	env["POWERSCHOOL_CHROME_NO_SANDBOX"] = "true"

	cfg, problems := loadConfig(envOf(env))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if cfg.BaseURL != "https://portal.example.org" {
		t.Errorf("base URL = %q", cfg.BaseURL)
	}
	if cfg.VaultK8sRole != "other-role" || cfg.VaultKVMount != "kv" || cfg.VaultSessionPath != "a/b/session" {
		t.Errorf("vault = %q %q %q", cfg.VaultK8sRole, cfg.VaultKVMount, cfg.VaultSessionPath)
	}
	if cfg.VaultCACert != "/etc/vault-ca/tls-ca-bundle.pem" {
		t.Errorf("CA = %q", cfg.VaultCACert)
	}
	if cfg.Blackout.String() != "15:30-22:00" || cfg.Blackout.loc.String() != "America/Denver" {
		t.Errorf("blackout = %s %s", cfg.Blackout, cfg.Blackout.loc)
	}
	if cfg.MinSessionAge != 30*time.Hour || cfg.SessionLifetime != 48*time.Hour || !cfg.ChromeNoSandbox {
		t.Errorf("ages = %s %s, no-sandbox = %v", cfg.MinSessionAge, cfg.SessionLifetime, cfg.ChromeNoSandbox)
	}
}

func TestLoadConfigProblems(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(map[string]string)
		want   string
	}{
		"no host":             {func(e map[string]string) { delete(e, "POWERSCHOOL_HOSTNAME") }, "POWERSCHOOL_HOSTNAME"},
		"host with userinfo":  {func(e map[string]string) { e["POWERSCHOOL_HOSTNAME"] = "https://u:p@portal.example.org" }, "POWERSCHOOL_HOSTNAME"},
		"no username":         {func(e map[string]string) { delete(e, "POWERSCHOOL_USERNAME") }, "POWERSCHOOL_USERNAME"},
		"no password":         {func(e map[string]string) { delete(e, "POWERSCHOOL_PASSWORD") }, "POWERSCHOOL_PASSWORD"},
		"no vault address":    {func(e map[string]string) { delete(e, "VAULT_ADDR") }, "VAULT_ADDR"},
		"plaintext vault":     {func(e map[string]string) { e["VAULT_ADDR"] = "http://vault.vault.svc.cluster.local:8200" }, "https"},
		"skip verify":         {func(e map[string]string) { e["VAULT_SKIP_VERIFY"] = "true" }, "VAULT_SKIP_VERIFY"},
		"skip verify as 1":    {func(e map[string]string) { e["VAULT_SKIP_VERIFY"] = "1" }, "VAULT_SKIP_VERIFY"},
		"empty blackout":      {func(e map[string]string) { e["POWERSCHOOL_BLACKOUT_END"] = "16:00" }, "POWERSCHOOL_BLACKOUT"},
		"bad blackout zone":   {func(e map[string]string) { e["POWERSCHOOL_BLACKOUT_TZ"] = "Pacific" }, "POWERSCHOOL_BLACKOUT"},
		"bad minimum age":     {func(e map[string]string) { e["POWERSCHOOL_MIN_SESSION_AGE"] = "ten hours" }, "POWERSCHOOL_MIN_SESSION_AGE"},
		"negative min age":    {func(e map[string]string) { e["POWERSCHOOL_MIN_SESSION_AGE"] = "-1h" }, "POWERSCHOOL_MIN_SESSION_AGE"},
		"lifetime too short":  {func(e map[string]string) { e["POWERSCHOOL_SESSION_LIFETIME"] = "5m" }, "POWERSCHOOL_SESSION_LIFETIME"},
		"lifetime not a span": {func(e map[string]string) { e["POWERSCHOOL_SESSION_LIFETIME"] = "36" }, "POWERSCHOOL_SESSION_LIFETIME"},
	} {
		t.Run(name, func(t *testing.T) {
			env := minimalEnv()
			c.mutate(env)
			_, problems := loadConfig(envOf(env))
			if len(problems) == 0 {
				t.Fatal("no problem reported")
			}
			joined := strings.Join(problems, "; ")
			if !strings.Contains(joined, c.want) {
				t.Errorf("problems %q do not mention %q", joined, c.want)
			}
		})
	}
}

// The problems list is logged. It names settings, never their values: a
// password pasted into the wrong field must not come out in the log.
func TestLoadConfigProblemsNeverQuoteValues(t *testing.T) {
	env := map[string]string{
		"POWERSCHOOL_HOSTNAME":         "https://" + testUsername + ":" + testPassword + "@portal.example.org",
		"POWERSCHOOL_USERNAME":         "",
		"POWERSCHOOL_PASSWORD":         "",
		"VAULT_ADDR":                   testPassword,
		"VAULT_SKIP_VERIFY":            "true",
		"POWERSCHOOL_MIN_SESSION_AGE":  testPassword,
		"POWERSCHOOL_SESSION_LIFETIME": testPassword,
	}
	_, problems := loadConfig(envOf(env))
	if len(problems) < 6 {
		t.Fatalf("expected a problem per bad setting, got %v", problems)
	}
	joined := strings.Join(problems, "; ")
	if strings.Contains(joined, testPassword) || strings.Contains(joined, testUsername) {
		t.Errorf("a value appears in the problems list: %s", joined)
	}
}

func TestIsTruthy(t *testing.T) {
	for v, want := range map[string]bool{
		"true": true, "TRUE": true, "1": true, "t": true, " true ": true,
		"": false, "false": false, "0": false, "yes": false, "on": false,
	} {
		if got := isTruthy(v); got != want {
			t.Errorf("isTruthy(%q) = %v, want %v", v, got, want)
		}
	}
}
