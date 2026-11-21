package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPendingBudgetBodyAndPartJobIdentity(t *testing.T) {
	var p PendingReply
	if err := json.Unmarshal(pendingBudgetBody(4200), &p); err != nil {
		t.Fatalf("unmarshal pendingBudgetBody(4200): %v", err)
	}
	if !p.Pending || p.RetryInMS != 4200 {
		t.Fatalf("pendingBudgetBody(4200) = %+v, want Pending=true RetryInMS=4200", p)
	}
	if got := string(pendingBudgetBody(0)); got != `{"pending":true}` {
		t.Fatalf("pendingBudgetBody(0) = %s, want {\"pending\":true}", got)
	}

	// Concurrent pending /resolve calls for different partIndex values of the
	// same item must launch distinct background jobs rather than colliding.
	env := newTestEnv(t)
	rec := env.installHello(t, "1.0.0")
	defer env.m.cancelActionJobs(rec.ID)

	var (
		mu       sync.Mutex
		donePart = map[int]bool{}
		jobParts []int
	)
	path := "/ether/v1/kinds/recording/resolve"
	env.fake.routes[path] = func(req Request) (Response, error) {
		var body struct {
			PartIndex *int `json:"partIndex"`
		}
		_ = json.Unmarshal(req.Body, &body)
		p := -1
		if body.PartIndex != nil {
			p = *body.PartIndex
		}
		mu.Lock()
		ready := donePart[p]
		mu.Unlock()
		if !ready {
			return Response{Status: 200, Body: json.RawMessage(`{"pending":true,"retryInMs":5000,"job":"/ether/job/resolve"}`)}, nil
		}
		return Response{Status: 200, Body: json.RawMessage(`{"sources":[{"id":"ok"}]}`)}, nil
	}
	env.fake.routes["/ether/job/resolve"] = func(req Request) (Response, error) {
		var body struct {
			PartIndex *int `json:"partIndex"`
		}
		_ = json.Unmarshal(req.Body, &body)
		p := -1
		if body.PartIndex != nil {
			p = *body.PartIndex
		}
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		donePart[p] = true
		jobParts = append(jobParts, p)
		mu.Unlock()
		return Response{Status: 200, Body: json.RawMessage(`{"done":true}`)}, nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, part := range []int{0, 1} {
		wg.Add(1)
		go func(part int) {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{
				"item":      map[string]any{"ref": "plug:recording:set-1", "id": "set-1"},
				"partIndex": part,
			})
			resp, err := env.m.Call(context.Background(), rec.ID, Request{
				Path: path, Method: "POST", Body: raw,
			})
			if err != nil {
				errs <- err
				return
			}
			if resp.Status != 200 {
				errs <- errors.New("unexpected status")
			}
		}(part)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent part Call failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(jobParts) != 2 || !donePart[0] || !donePart[1] {
		t.Fatalf("expected separate jobs for parts 0 and 1, got jobParts=%v donePart=%v", jobParts, donePart)
	}
}

func TestHostStreamPartURL(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	man := contentManifest()
	man.Ether.Contributes.MediaKinds[0].Sync = KindSyncDecl{Mode: KindSyncPush}
	man.Ether.Capabilities = append(man.Ether.Capabilities, CapFrontendAdapter)
	man.Ether.Contributes.FrontendAdapters = []FrontendAdapterMeta{
		{Key: "feed", Label: "Records feed", ProtocolHint: "opds", PathPrefix: "/opds", Libraries: []string{"recording"}},
	}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	p, err := env.m.PreviewInstall(ctx, Source{Kind: "studio", Manifest: raw, Files: map[string][]byte{"dist/plugin.js": []byte("// library")}})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := env.m.Install(ctx, p, InstallOptions{AcceptAddedPermissions: true})
	if err != nil {
		t.Fatal(err)
	}
	h := env.m.HostFor(rec.ID)

	var gotViewer int64
	var gotItem string
	var gotPart *int
	env.m.SetKindBackends(KindBackends{
		StreamPartURL: func(_ context.Context, pluginID string, viewerID int64, itemRef string, partIndex *int) (string, error) {
			if pluginID != rec.ID {
				t.Errorf("backend plugin scope: %s", pluginID)
			}
			gotViewer, gotItem, gotPart = viewerID, itemRef, partIndex
			return "/ether/v1/kind-media/ticket-part-2", nil
		},
	})

	viewerCtx := WithViewer(ctx, 9)
	part2 := 2
	badPart := -1
	if _, err := h.StreamPartURL(ctx, "pk:recording:trk-1", &part2); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("StreamPartURL without viewer: %v", err)
	}
	if _, err := h.StreamPartURL(viewerCtx, "pk:recording:trk-1", &badPart); err == nil {
		t.Fatal("negative partIndex accepted")
	}
	u, err := h.StreamPartURL(viewerCtx, "pk:recording:trk-1", &part2)
	if err != nil || u != "/ether/v1/kind-media/ticket-part-2" {
		t.Fatalf("StreamPartURL = %q, err = %v", u, err)
	}
	if gotViewer != 9 || gotItem != "pk:recording:trk-1" || gotPart == nil || *gotPart != 2 {
		t.Fatalf("backend got viewer=%d item=%q part=%v", gotViewer, gotItem, gotPart)
	}
}
