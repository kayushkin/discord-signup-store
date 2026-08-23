package discordsignup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The auth-store resolve URL carries two operator-supplied values in two
// different positions — provider as a path segment, account as a query
// parameter — and the repairs for those positions are different. These pins
// drive the resolver against a fake auth-store and assert on what that server
// saw, so they are independent of how the URL was composed.
//
// The probe carries no space: net/http refuses to parse a URL containing one,
// which fails the request outright instead of corrupting it. That loud case is
// pinned separately at the foot of the file so it cannot stand in for the
// quiet one.
const resolveHostileProbe = "acc+ount&injected=1"

// resolveAgainstFakeAuthStore runs the resolver against a fake auth-store and
// returns the path and query that server received.
func resolveAgainstFakeAuthStore(t *testing.T, provider, account string) (path string, query url.Values) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath, not Path: Path is already decoded, so an escaped
		// slash and a real one are the same string there and the pin
		// cannot tell a repair from its absence.
		path, query = r.URL.EscapedPath(), r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]string{"api_key": "a-bot-token"})
	}))
	defer srv.Close()

	token, err := AuthStoreTokenResolver(srv.URL, "", provider, account)()
	if err != nil {
		t.Fatalf("resolve never reached the fake auth-store: %v", err)
	}
	if token != "a-bot-token" {
		t.Fatalf("resolver returned %q, want the token the fake served", token)
	}
	return path, query
}

func TestAnAccountCarryingReservedCharactersStaysOneParameter(t *testing.T) {
	_, query := resolveAgainstFakeAuthStore(t, "discord", resolveHostileProbe)

	if got := query.Get("account"); got != resolveHostileProbe {
		t.Errorf("account was mangled in transit:\n got %q\nwant %q", got, resolveHostileProbe)
	}
	if query.Has("injected") {
		t.Errorf("account injected a parameter the composer never wrote: injected=%q", query.Get("injected"))
	}
	if n := len(query); n != 1 {
		t.Errorf("want exactly the account parameter; got %d: %v", n, query)
	}
}

// A provider sits in a PATH segment, where url.Values is the wrong repair and
// a slash is the character that matters: it does not need escaping to be
// dangerous, it silently becomes a route.
func TestAProviderStaysOnePathSegment(t *testing.T) {
	const hostileProvider = "discord/../../admin"
	path, _ := resolveAgainstFakeAuthStore(t, hostileProvider, "default")

	const prefix = "/api/resolve/"
	if !strings.HasPrefix(path, prefix) {
		t.Fatalf("request did not reach %s at all: path %q", prefix, path)
	}
	if segment := strings.TrimPrefix(path, prefix); strings.Contains(segment, "/") {
		t.Errorf("provider became %d path segments on the wire; want 1:\n got %q",
			strings.Count(segment, "/")+1, path)
	}
}

// QueryEscape is the plausible wrong repair for a path segment: it encodes a
// space as "+", which inside a path is a literal plus sign and not a space, so
// the provider that arrives is not the provider that was asked for. PathEscape
// writes "%20" and the space survives.
func TestAProviderWithASpaceArrivesCarryingThatSpace(t *testing.T) {
	const spacedProvider = "discord staging"
	path, _ := resolveAgainstFakeAuthStore(t, spacedProvider, "default")

	segment, err := url.PathUnescape(strings.TrimPrefix(path, "/api/resolve/"))
	if err != nil {
		t.Fatalf("provider segment does not unescape: %v", err)
	}
	if segment != spacedProvider {
		t.Errorf("provider was mangled in transit:\n got %q\nwant %q", segment, spacedProvider)
	}
}

// A provider must not be able to reach the query either — the two positions
// are adjacent on one line and a repair aimed at one can spill into the other.
func TestAProviderCannotAddAQueryParameter(t *testing.T) {
	_, query := resolveAgainstFakeAuthStore(t, "discord?account=substituted", "default")

	if got := query.Get("account"); got != "default" {
		t.Errorf("provider overrode account: server read %q, composer wrote %q", got, "default")
	}
}

// This is the case the probe above deliberately excludes. A space makes
// net/http refuse the URL, so the failure is "the request was never sent" —
// loud and attributable, and nothing to do with a value being rewritten.
func TestASpaceInTheAccountIsNotWhatThesePinsAreAbout(t *testing.T) {
	if _, err := http.NewRequest(http.MethodGet, "http://example.invalid/x?account=a b", nil); err == nil {
		t.Skip("net/http now accepts a space in a URL; this pin's premise is gone")
	}
}
