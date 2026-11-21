package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientIdentityResolve(t *testing.T) {
	ether := DefaultUserAgent()
	if !strings.HasPrefix(ether, "Ether/") || !strings.HasSuffix(ether, "(+https://github.com/braedonsaunders/ether)") {
		t.Fatalf("DefaultUserAgent() = %q", ether)
	}
	headers := map[string]string{"X-Api-Client": "ether"}

	cases := []struct {
		name     string
		policy   ClientIdentity
		override string
		want     string
	}{
		{"empty policy uses ether default", ClientIdentity{}, "", ether},
		{"blank policy uses ether default", ClientIdentity{UserAgent: "   "}, "", ether},
		{"global user agent", ClientIdentity{UserAgent: "MyClient/2.0"}, "", "MyClient/2.0"},
		{"indexer override wins over empty policy", ClientIdentity{}, "IndexerUA/1.0", "IndexerUA/1.0"},
		{"indexer override wins over global", ClientIdentity{UserAgent: "GlobalUA/1.0"}, "IndexerUA/1.0", "IndexerUA/1.0"},
		{"blank indexer override inherits global", ClientIdentity{UserAgent: "GlobalUA/1.0"}, "  ", "GlobalUA/1.0"},
	}
	for _, tc := range cases {
		p := tc.policy
		p.Headers = headers
		got := p.Resolve(tc.override)
		if got.UserAgent != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got.UserAgent, tc.want)
		}
		if got.Headers["X-Api-Client"] != "ether" {
			t.Errorf("%s: headers not preserved: %+v", tc.name, got.Headers)
		}
	}
}

func TestClientIdentityValidate(t *testing.T) {
	bad := []struct {
		name   string
		policy ClientIdentity
		field  string
	}{
		{"line break in user agent", ClientIdentity{UserAgent: "a\r\nb"}, "userAgent"},
		{"control char in user agent", ClientIdentity{UserAgent: "bad\x07ua"}, "userAgent"},
		{"overlong user agent", ClientIdentity{UserAgent: strings.Repeat("a", 513)}, "userAgent"},
		{"line break in a header", ClientIdentity{Headers: map[string]string{"X-Test": "a\nb"}}, "headers"},
		{"control char in a header", ClientIdentity{Headers: map[string]string{"X-Test": "a\x7fb"}}, "headers"},
		{"transport header", ClientIdentity{Headers: map[string]string{"Accept-Encoding": "gzip"}}, "headers"},
		{"host header", ClientIdentity{Headers: map[string]string{"Host": "other"}}, "headers"},
		{"user agent header", ClientIdentity{Headers: map[string]string{"User-Agent": "x"}}, "headers"},
		{"bad header name", ClientIdentity{Headers: map[string]string{"X Test": "v"}}, "headers"},
		{"duplicate header names", ClientIdentity{Headers: map[string]string{"X-Test": "v", "x-test": "w"}}, "headers"},
	}
	for _, tc := range bad {
		errs := tc.policy.Validate()
		found := false
		for _, e := range errs {
			if e.Field == tc.field {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected error on %q, got %+v", tc.name, tc.field, errs)
		}
	}
}

func TestClientIdentityValidateAccepts(t *testing.T) {
	ok := ClientIdentity{
		UserAgent: "CustomClient/1.2 (+https://example.com)",
		Headers:   map[string]string{"X-Custom": "ok"},
	}
	if errs := ok.Validate(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if errs := (ClientIdentity{}).Validate(); len(errs) != 0 {
		t.Fatalf("empty policy rejected: %+v", errs)
	}
}

func TestClientIdentityUpgradeMigration(t *testing.T) {
	t.Run("movieSearch radarr and indexer sonarr upgrade to exact UA strings", func(t *testing.T) {
		dir := t.TempDir()
		raw := `{
			"clientIdentity": {
				"tvSearch": "ether",
				"movieSearch": "radarr",
				"tvGrab": "nzbget",
				"headers": {"X-Owner": "kept"}
			},
			"indexers": [
				{"id": "ix-sonarr", "name": "SonarrIdx", "url": "https://a.example", "clientIdentity": "sonarr"},
				{"id": "ix-ether", "name": "EtherIdx", "url": "https://b.example", "clientIdentity": "ether"},
				{"id": "ix-custom", "name": "CustomIdx", "url": "https://c.example", "clientIdentity": "custom"},
				{"id": "ix-unset", "name": "UnsetIdx", "url": "https://d.example"}
			]
		}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg := store.Get()
		if got, want := cfg.ClientIdentity.UserAgent, "Radarr/6.4.4.10685 (ubuntu 26.04)"; got != want {
			t.Fatalf("global userAgent = %q, want %q", got, want)
		}
		if got := cfg.ClientIdentity.Headers["X-Owner"]; got != "kept" {
			t.Fatalf("headers lost on upgrade: %+v", cfg.ClientIdentity.Headers)
		}
		wantIx := map[string]string{
			"ix-sonarr": "Sonarr/4.0.20.3014 (ubuntu 26.04)",
			"ix-ether":  "",
			"ix-custom": "",
			"ix-unset":  "",
		}
		for _, ix := range cfg.Indexers {
			if got, want := ix.UserAgent, wantIx[ix.ID]; got != want {
				t.Fatalf("indexer %s userAgent = %q, want %q", ix.ID, got, want)
			}
		}
		// Retired keys must be removed from the rewritten config.json.
		saved, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, retired := range []string{`"tvSearch"`, `"movieSearch"`, `"tvGrab"`, `"movieGrab"`} {
			if strings.Contains(string(saved), retired) {
				t.Fatalf("retired field %s still in config.json:\n%s", retired, saved)
			}
		}
	})

	t.Run("ether and unset stay empty", func(t *testing.T) {
		cfg, changed, err := decodeStartupConfig(Defaults(t.TempDir()), []byte(`{
			"clientIdentity": {"tvSearch": "ether", "movieSearch": ""},
			"indexers": [{"id": "i1", "name": "Idx", "url": "https://a.example", "clientIdentity": "ether"}]
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("expected retired fields to report changed=true")
		}
		if cfg.ClientIdentity.UserAgent != "" {
			t.Fatalf("ether upgraded to non-empty userAgent %q", cfg.ClientIdentity.UserAgent)
		}
		if len(cfg.Indexers) != 1 || cfg.Indexers[0].UserAgent != "" {
			t.Fatalf("ether indexer override not cleared: %+v", cfg.Indexers)
		}
	})

	t.Run("slot order wins and custom keeps existing userAgent", func(t *testing.T) {
		cfg, _, err := decodeStartupConfig(Defaults(t.TempDir()), []byte(`{
			"clientIdentity": {"tvGrab": "sabnzbd", "movieGrab": "nzbget"}
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := cfg.ClientIdentity.UserAgent, "SABnzbd/5.1.3"; got != want {
			t.Fatalf("slot order userAgent = %q, want %q", got, want)
		}

		customCfg, _, err := decodeStartupConfig(Defaults(t.TempDir()), []byte(`{
			"clientIdentity": {"tvSearch": "custom", "movieSearch": "radarr", "userAgent": "MyOwn/3.1"}
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := customCfg.ClientIdentity.UserAgent; got != "MyOwn/3.1" {
			t.Fatalf("custom userAgent overwritten: %q", got)
		}
	})
}
