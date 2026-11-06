package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestActorForAddressCountsIPv4WholeAndIPv6ByItsBlock(t *testing.T) {
	cases := map[string]string{
		"203.0.113.9":                  "ipv4:203.0.113.9",
		"203.0.113.9:51234":            "ipv4:203.0.113.9",
		"::ffff:203.0.113.9":           "ipv4:203.0.113.9",
		"2001:db8:85a3::8a2e:370:7334": "ipv6:2001:0db8:85a3:0000::/64",
		"[2001:db8:85a3:1::1]:443":     "ipv6:2001:0db8:85a3:0001::/64",
		"fe80::1%eth0":                 "ipv6:fe80:0000:0000:0000::/64",
		"not an address":               "",
		"":                             "",
	}
	for in, want := range cases {
		if got := ActorForAddress(in); got != want {
			t.Errorf("ActorForAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeControlPlane answers every authentication for one account and keeps the headers it was sent.
type fakeControlPlane struct {
	headers http.Header
}

func (f *fakeControlPlane) DoJSON(_ context.Context, _ string, _ string, _ any, output any, headers http.Header) error {
	f.headers = headers.Clone()
	raw, _ := json.Marshal(map[string]any{"accountId": "acct_1", "apiKeyId": "key_1", "concurrency": 4, "accountConcurrency": 8})
	return json.Unmarshal(raw, output)
}

func TestTheRemoteAuthenticatorHandsTheClientAddressToTheControlPlane(t *testing.T) {
	plane := &fakeControlPlane{}
	authenticator := NewRemoteAuthenticator(plane)
	principal, err := authenticator.Authenticate(WithClientActor(context.Background(), "ipv4:203.0.113.9"), "Bearer leb_live_abc")
	if err != nil || principal.AccountID != "acct_1" || principal.APIKeyID != "key_1" {
		t.Fatalf("principal %+v, err %v", principal, err)
	}
	if got := plane.headers.Get(ClientActorHeader); got != "ipv4:203.0.113.9" {
		t.Fatalf("the client's address must reach the control plane, got %q", got)
	}
	// a request the platform's edge admitted names no client: the edge counted it already
	plane.headers = nil
	if _, err := authenticator.Authenticate(context.Background(), "Bearer leb_live_abc"); err != nil {
		t.Fatal(err)
	}
	if got := plane.headers.Get(ClientActorHeader); got != "" {
		t.Fatalf("an edge request must name no client, got %q", got)
	}
	if _, err := authenticator.Authenticate(WithClientActor(context.Background(), ""), "Bearer leb_live_abc"); err != nil {
		t.Fatal(err)
	}
	if got := plane.headers.Get(ClientActorHeader); got != "" {
		t.Fatalf("an unknown address names no client, got %q", got)
	}
}
