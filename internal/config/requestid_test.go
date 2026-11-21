package config

import (
	"net/url"
	"strings"
	"testing"
)

func TestProwlarrProfileHeaderOrderAndQueryShape(t *testing.T) {
	ix := Indexer{RequestIdentity: &IndexerRequestIdentity{Profile: "prowlarr"}}
	id := ClientIdentity{}.ResolveIndexer(ix, false)
	var names []string
	for _, h := range id.Ordered {
		names = append(names, h.Name)
	}
	want := []string{"User-Agent", "Accept", "Accept-Encoding", "traceparent"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("header order = %v, want %v", names, want)
	}
	q := id.ShapeQuery(url.Values{
		"t": {"tvsearch"}, "season": {"0"}, "ep": {"3"}, "year": {"2021"},
		"q": {"Example Show"}, "apikey": {"KEY"}, "limit": {"100"},
	})
	if !strings.HasPrefix(q, "t=tvsearch&") {
		t.Fatalf("query does not start with t: %s", q)
	}
	for _, frag := range []string{"offset=0", "season=00", "q=Example+Show+2021"} {
		if !strings.Contains(q, frag) {
			t.Fatalf("query %s missing %s", q, frag)
		}
	}
	if strings.Contains(q, "year=") {
		t.Fatalf("year param not moved into q: %s", q)
	}
	if strings.Index(q, "apikey=") > strings.Index(q, "offset=") {
		t.Fatalf("apikey must precede offset in prowlarr order: %s", q)
	}
}

func TestRequestIdentityOverridePrecedence(t *testing.T) {
	ix := Indexer{RequestIdentity: &IndexerRequestIdentity{
		Profile: "sonarr",
		Headers: []KVOverride{
			{Name: "Accept", Value: "text/xml"},
			{Name: "traceparent", Remove: true},
			{Name: "X-Extra", Value: "1"},
		},
		Params: []KVOverride{
			{Name: "extended", Remove: true},
			{Name: "cat", Value: "5070"},
			{Name: "apikey", Remove: true},
		},
	}}
	id := ClientIdentity{}.ResolveIndexer(ix, false)
	byName := map[string]string{}
	for _, h := range id.Ordered {
		byName[strings.ToLower(h.Name)] = h.Value
	}
	if byName["accept"] != "text/xml" || byName["x-extra"] != "1" {
		t.Fatalf("header overrides not applied: %v", byName)
	}
	if _, ok := byName["traceparent"]; ok {
		t.Fatal("traceparent override remove ignored")
	}
	q := id.ShapeQuery(url.Values{"t": {"search"}, "extended": {"1"}, "apikey": {"K"}, "cat": {"5000"}})
	if strings.Contains(q, "extended=") || !strings.Contains(q, "cat=5070") || !strings.Contains(q, "apikey=K") {
		t.Fatalf("param overrides wrong (apikey must survive): %s", q)
	}
}

func TestGrabIdentityDefaultsToPairedDownloader(t *testing.T) {
	ix := Indexer{RequestIdentity: &IndexerRequestIdentity{Profile: "prowlarr"}}
	grab := ClientIdentity{}.ResolveIndexer(ix, true)
	if grab.Profile != "sabnzbd" || !strings.HasPrefix(grab.UserAgent, "SABnzbd/") {
		t.Fatalf("grab identity = %+v", grab)
	}
	ix.RequestIdentity.GrabProfile = "nzbget"
	grab = ClientIdentity{}.ResolveIndexer(ix, true)
	if grab.Profile != "nzbget" {
		t.Fatalf("grab profile override ignored: %+v", grab)
	}
}

func TestRequestIdentityMigrationFromUserAgent(t *testing.T) {
	doc := map[string]any{"indexers": []any{
		map[string]any{"id": "a", "userAgent": "SABnzbd/5.1.3"},
		map[string]any{"id": "b", "userAgent": "MyOwn/9"},
		map[string]any{"id": "c"},
	}}
	if !upgradeClientIdentity(doc) {
		t.Fatal("migration reported no change")
	}
	ixs := doc["indexers"].([]any)
	a := ixs[0].(map[string]any)["requestIdentity"].(map[string]any)
	if a["profile"] != "sabnzbd" {
		t.Fatalf("known UA migrated to %v", a)
	}
	b := ixs[1].(map[string]any)["requestIdentity"].(map[string]any)
	if b["profile"] != "custom" {
		t.Fatalf("custom UA migrated to %v", b)
	}
	if _, ok := ixs[2].(map[string]any)["requestIdentity"]; ok {
		t.Fatal("indexer without UA gained an identity")
	}
}

func TestRequestIdentityValidateRejectsProtectedParams(t *testing.T) {
	ri := &IndexerRequestIdentity{
		Profile: "nope",
		Params:  []KVOverride{{Name: "t", Remove: true}},
	}
	if len(ri.Validate()) < 2 {
		t.Fatalf("expected errors, got %v", ri.Validate())
	}
}
