package satoken

import (
	"context"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/satoken/satokentest"
)

const testAudience = "glovebox-login-broker-powerschool"

func TestTokenReviewerAcceptsOnlyAnAllowListedServiceAccountWithThisAudience(t *testing.T) {
	api := satokentest.New(t)
	api.Allow("connector", "system:serviceaccount:glovebox:glovebox-powerschool", testAudience)
	api.Allow("other-sa", "system:serviceaccount:glovebox:glovebox-rss", testAudience)
	api.Allow("other-namespace", "system:serviceaccount:default:glovebox-powerschool", testAudience)
	api.Allow("wrong-audience", "system:serviceaccount:glovebox:glovebox-powerschool", "glovebox-login-broker-schoology")
	api.Allow("no-audience", "system:serviceaccount:glovebox:glovebox-powerschool")
	api.Allow("a-user", "operator", testAudience)
	api.Allow("a-node", "system:node:worker-1", testAudience)
	api.Allow("odd-name", "system:serviceaccount:glovebox:glovebox-powerschool:extra", testAudience)
	api.Deny("not-authenticated", testAudience)

	rv, err := NewTokenReviewer(api.Access(), testAudience, []string{"glovebox/glovebox-powerschool"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	caller, err := rv.Authenticate(ctx, "connector")
	if err != nil || caller.String() != "glovebox/glovebox-powerschool" {
		t.Fatalf("the connector: %v, %v", caller, err)
	}
	for token, want := range map[string]error{
		"other-sa":          ErrForbidden,
		"other-namespace":   ErrForbidden,
		"a-user":            ErrForbidden,
		"a-node":            ErrForbidden,
		"odd-name":          ErrForbidden,
		"wrong-audience":    ErrUnauthenticated,
		"no-audience":       ErrUnauthenticated,
		"not-authenticated": ErrUnauthenticated,
		"never-issued":      ErrUnauthenticated,
		"":                  ErrUnauthenticated,
	} {
		if _, err := rv.Authenticate(ctx, token); err != want {
			t.Errorf("token %q: %v, want %v", token, err, want)
		}
	}
	requests, bearers, audiences := api.Requests()
	for i, req := range requests {
		if req != "POST /apis/authentication.k8s.io/v1/tokenreviews" || bearers[i] != "Bearer "+satokentest.OwnToken {
			t.Errorf("request %d = %s as %q", i, req, bearers[i])
		}
	}
	for _, a := range audiences {
		if len(a) != 1 || a[0] != testAudience {
			t.Errorf("review asked for audiences %v", a)
		}
	}
	if len(requests) != 10 { // the empty token never leaves the process
		t.Errorf("reviews sent = %d, want 10", len(requests))
	}
}

func TestTokenReviewerFailsClosedWhenTheAPIServerDoesNotAnswer(t *testing.T) {
	api := satokentest.New(t)
	api.Allow("connector", "system:serviceaccount:glovebox:glovebox-powerschool", testAudience)
	rv, err := NewTokenReviewer(api.Access(), testAudience, []string{"glovebox/glovebox-powerschool"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{403, 500, 302} {
		api.FailWith(status)
		_, err := rv.Authenticate(context.Background(), "connector")
		if err == nil || !strings.Contains(err.Error(), ErrAuthUnavailable.Error()) {
			t.Errorf("API status %d: %v", status, err)
		}
		if err != nil && strings.Contains(err.Error(), "connector") {
			t.Errorf("the error quotes the token: %v", err)
		}
	}
	api.Close()
	if _, err := rv.Authenticate(context.Background(), "connector"); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("API gone: %v", err)
	}
}

func TestTokenReviewerConfiguration(t *testing.T) {
	api := satokentest.New(t)
	for name, c := range map[string]struct {
		audience string
		allowed  []string
	}{
		"no audience":         {"", []string{"glovebox/a"}},
		"two audiences":       {"a,b", []string{"glovebox/a"}},
		"nobody allowed":      {testAudience, nil},
		"only blanks":         {testAudience, []string{" ", ""}},
		"a wildcard":          {testAudience, []string{"glovebox/*"}},
		"no namespace":        {testAudience, []string{"glovebox-powerschool"}},
		"the API's long form": {testAudience, []string{"system:serviceaccount:glovebox:a"}},
		"three parts":         {testAudience, []string{"a/b/c"}},
	} {
		if _, err := NewTokenReviewer(api.Access(), c.audience, c.allowed); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	plain := api.Access()
	plain.BaseURL = "http://10.0.0.1:6443"
	if _, err := NewTokenReviewer(plain, testAudience, []string{"glovebox/a"}); err == nil {
		t.Error("a plaintext API address was accepted")
	}
	rv, err := NewTokenReviewer(api.Access(), testAudience, []string{" glovebox/b ", "glovebox/a"})
	if err != nil || strings.Join(rv.Allowed(), ",") != "glovebox/a,glovebox/b" || rv.Audience() != testAudience {
		t.Errorf("reviewer = %v, %v", rv, err)
	}
}

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer abc":     "abc",
		"bearer abc":     "abc",
		"  Bearer  abc ": "abc",
		"Basic abc":      "",
		"Bearer":         "",
		"Bearer a b":     "",
		"":               "",
	} {
		got, ok := BearerToken(header)
		if got != want || ok != (want != "") {
			t.Errorf("BearerToken(%q) = %q, %v", header, got, ok)
		}
	}
	if _, ok := BearerToken("Bearer " + strings.Repeat("x", maxTokenBytes+1)); ok {
		t.Error("an oversized token was accepted")
	}
}

func TestSafeName(t *testing.T) {
	if got := SafeName("glovebox-a.b"); got != "glovebox-a.b" {
		t.Errorf("SafeName kept %q", got)
	}
	if got := SafeName("A b\n/c"); got != "__b__c" {
		t.Errorf("SafeName = %q", got)
	}
	if got := SafeName(strings.Repeat("a", 200)); len(got) != 80 {
		t.Errorf("SafeName length %d", len(got))
	}
}
