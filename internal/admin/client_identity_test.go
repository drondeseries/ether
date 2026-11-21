package admin_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
)

func TestClientIdentitySection(t *testing.T) {
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error { seededConfig(c); c.SetupComplete = true; return nil }); err != nil {
		t.Fatal(err)
	}
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	put := func(body string) *httptest.ResponseRecorder {
		r := req(http.MethodPut, "/ether/api/config/clientIdentity", body)
		r.Header.Set("X-Plex-Token", ownerToken)
		return serve(h, r)
	}

	rec := put(`{"userAgent":"CustomClient/2.0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT clientIdentity: %d %s", rec.Code, rec.Body)
	}
	if got := store.Get().ClientIdentity; got.UserAgent != "CustomClient/2.0" {
		t.Fatalf("policy not saved: %+v", got)
	}

	// An invalid user agent is rejected on clientIdentity.userAgent.
	rec = put(`{"userAgent":"bad\u0007ua"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("control char user agent: %d %s", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Fields []admin.FieldError `json:"fields"`
		} `json:"error"`
	}
	decode(t, rec, &e)
	found := false
	for _, f := range e.Error.Fields {
		found = found || f.Field == "clientIdentity.userAgent"
	}
	if !found {
		t.Fatalf("no field error for userAgent: %+v", e.Error.Fields)
	}
	if got := store.Get().ClientIdentity.UserAgent; got != "CustomClient/2.0" {
		t.Fatalf("rejected value overwrote stored value: %q", got)
	}

	// Custom header values are sealed at rest and redacted in responses.
	rec = put(`{"userAgent":"MyClient/1.0","headers":{"X-Api-Client":"s3cret"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("custom identity: %d %s", rec.Code, rec.Body)
	}
	if got := store.Get().ClientIdentity.Headers["X-Api-Client"]; got != "s3cret" {
		t.Fatalf("header not stored: %q", got)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cret") {
		t.Fatalf("header value written in plaintext:\n%s", raw)
	}

	r := req(http.MethodGet, "/ether/api/config/clientIdentity", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	rec = serve(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET clientIdentity: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cret") || !strings.Contains(rec.Body.String(), "••••") {
		t.Fatalf("section GET leaks the header: %s", rec.Body)
	}
	var section struct {
		Value            json.RawMessage `json:"value"`
		DefaultUserAgent string          `json:"defaultUserAgent"`
	}
	decode(t, rec, &section)
	if section.DefaultUserAgent == "" || section.DefaultUserAgent != config.DefaultUserAgent() {
		t.Fatalf("defaultUserAgent = %q, want %q", section.DefaultUserAgent, config.DefaultUserAgent())
	}
	// Sending the redacted form back keeps the stored value.
	if rec = put(string(section.Value)); rec.Code != http.StatusOK {
		t.Fatalf("re-PUT redacted section: %d %s", rec.Code, rec.Body)
	}
	if got := store.Get().ClientIdentity.Headers["X-Api-Client"]; got != "s3cret" {
		t.Fatalf("redacted round-trip lost the header: %q", got)
	}
}

func TestIndexerUserAgentOverrideValidation(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	h := admin.NewHandler(admin.Deps{Config: store, IsOwnerToken: storedOwner(store)})
	body := `[{"id":"i1","name":"Idx","url":"https://idx.example.com","apiKey":"ix-key","dailyLimit":0,"dailyGrabs":0,"priority":0,"enabled":true,"movieCategories":null,"tvCategories":null,"userAgent":"bad\u0007ua"}]`
	r := req(http.MethodPut, "/ether/api/config/indexers", body)
	r.Header.Set("X-Plex-Token", ownerToken)
	rec := serve(h, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid userAgent override: %d %s", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Fields []admin.FieldError `json:"fields"`
		} `json:"error"`
	}
	decode(t, rec, &e)
	found := false
	for _, f := range e.Error.Fields {
		found = found || f.Field == "indexers[0].userAgent"
	}
	if !found {
		t.Fatalf("no field error for the override: %+v", e.Error.Fields)
	}
}
