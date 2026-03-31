package connector

import (
	"testing"
)

func TestIdentity_BothNilReturnsNil(t *testing.T) {
	result := MergeIdentity(nil, nil)
	if result != nil {
		t.Fatalf("expected nil, got %+v", result)
	}
}

func TestIdentity_ConfigOnlyReturnsIdentityFromConfig(t *testing.T) {
	cfg := &ConfigIdentity{
		Provider:   "github",
		AuthMethod: "oauth",
		Tenant:     "steve",
	}
	result := MergeIdentity(cfg, nil)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Provider != "github" {
		t.Errorf("provider: got %q, want %q", result.Provider, "github")
	}
	if result.AuthMethod != "oauth" {
		t.Errorf("auth_method: got %q, want %q", result.AuthMethod, "oauth")
	}
	if result.Tenant != "steve" {
		t.Errorf("tenant: got %q, want %q", result.Tenant, "steve")
	}
	if result.AccountID != "" {
		t.Errorf("account_id: got %q, want empty", result.AccountID)
	}
	if len(result.Scopes) != 0 {
		t.Errorf("scopes: got %v, want empty", result.Scopes)
	}
}

func TestIdentity_ItemOnlyReturnsCopy(t *testing.T) {
	item := &Identity{
		AccountID:  "steve@github",
		Provider:   "github",
		AuthMethod: "pat",
		Scopes:     []string{"repo", "read:org"},
		Tenant:     "steve",
	}
	result := MergeIdentity(nil, item)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result == item {
		t.Fatal("expected a copy, got same pointer")
	}
	if result.AccountID != "steve@github" {
		t.Errorf("account_id: got %q, want %q", result.AccountID, "steve@github")
	}
	if result.Provider != "github" {
		t.Errorf("provider: got %q, want %q", result.Provider, "github")
	}
	if result.AuthMethod != "pat" {
		t.Errorf("auth_method: got %q, want %q", result.AuthMethod, "pat")
	}
	if result.Tenant != "steve" {
		t.Errorf("tenant: got %q, want %q", result.Tenant, "steve")
	}
	if len(result.Scopes) != 2 || result.Scopes[0] != "repo" || result.Scopes[1] != "read:org" {
		t.Errorf("scopes: got %v, want [repo read:org]", result.Scopes)
	}
	// Mutating the copy should not affect the original.
	result.Scopes[0] = "mutated"
	if item.Scopes[0] == "mutated" {
		t.Fatal("copy shares underlying scopes slice with original")
	}
}

func TestIdentity_MergeConfigAndItem(t *testing.T) {
	cfg := &ConfigIdentity{
		Provider:   "github",
		AuthMethod: "oauth",
		Tenant:     "steve",
	}
	item := &Identity{
		AccountID: "steve@github",
		Scopes:    []string{"repo", "read:org"},
	}
	result := MergeIdentity(cfg, item)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Provider != "github" {
		t.Errorf("provider: got %q, want %q", result.Provider, "github")
	}
	if result.AuthMethod != "oauth" {
		t.Errorf("auth_method: got %q, want %q", result.AuthMethod, "oauth")
	}
	if result.Tenant != "steve" {
		t.Errorf("tenant: got %q, want %q", result.Tenant, "steve")
	}
	if result.AccountID != "steve@github" {
		t.Errorf("account_id: got %q, want %q", result.AccountID, "steve@github")
	}
	if len(result.Scopes) != 2 || result.Scopes[0] != "repo" {
		t.Errorf("scopes: got %v, want [repo read:org]", result.Scopes)
	}
}

func TestIdentity_ItemOverridesConfig(t *testing.T) {
	cfg := &ConfigIdentity{
		Provider:   "github",
		AuthMethod: "oauth",
		Tenant:     "steve",
	}
	item := &Identity{
		Provider: "gitlab",
	}
	result := MergeIdentity(cfg, item)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Provider != "gitlab" {
		t.Errorf("provider: got %q, want %q (item should override config)", result.Provider, "gitlab")
	}
	// Config fields that item did not override should carry through.
	if result.AuthMethod != "oauth" {
		t.Errorf("auth_method: got %q, want %q", result.AuthMethod, "oauth")
	}
	if result.Tenant != "steve" {
		t.Errorf("tenant: got %q, want %q", result.Tenant, "steve")
	}
}

func TestIdentity_ConfigAccountIDPopulatesWhenItemEmpty(t *testing.T) {
	cfg := &ConfigIdentity{
		AccountID:  "default-account",
		Provider:   "imap",
		AuthMethod: "app_password",
	}
	item := &Identity{
		Scopes: []string{"INBOX"},
	}
	result := MergeIdentity(cfg, item)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.AccountID != "default-account" {
		t.Errorf("account_id: got %q, want %q (config should fill empty item)", result.AccountID, "default-account")
	}
}
