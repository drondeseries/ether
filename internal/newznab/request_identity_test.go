package newznab

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestPreviewMasksAPIKeyAndShapesQuery(t *testing.T) {
	cfg := config.Indexer{
		URL: "https://idx.example.com", APIKey: "supersecretkey",
		RequestIdentity: &config.IndexerRequestIdentity{Profile: "prowlarr"},
	}
	p, err := PreviewRequestIdentity(cfg, config.ClientIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	blob := p.Search.RequestLine + p.Grab.RequestLine
	for _, h := range append(p.Search.Headers, p.Grab.Headers...) {
		blob += h.Name + h.Value
	}
	if strings.Contains(blob, "supersecretkey") {
		t.Fatalf("preview leaks api key: %s", blob)
	}
	if !strings.Contains(p.Search.RequestLine, "apikey=%E2%80%A2%E2%80%A2%E2%80%A2%E2%80%A2") && !strings.Contains(p.Search.RequestLine, "apikey=••••") {
		t.Fatalf("preview key not masked: %s", p.Search.RequestLine)
	}
	if !strings.Contains(p.Search.RequestLine, "season=00") || !strings.Contains(p.Search.RequestLine, "offset=0") {
		t.Fatalf("prowlarr shape missing in preview: %s", p.Search.RequestLine)
	}
	if p.GrabProfile != "sabnzbd" {
		t.Fatalf("grab profile = %q", p.GrabProfile)
	}
}
