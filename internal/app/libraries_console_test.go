package app

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/sessions"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
)

func TestLibrariesConsoleCRUDAndFilterBrowse(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	for _, title := range []core.Title{
		{Key: core.MovieKey(101), Type: core.TypeMovie, Title: "Alpha Drama", Year: 2001, Released: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2001, 6, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Drama"}},
		{Key: core.MovieKey(102), Type: core.TypeMovie, Title: "Beta Laughs", Year: 2002, Released: time.Date(2002, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2002, 6, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Comedy"}},
	} {
		if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	c := &librariesConsole{app: a}
	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// Built-ins are seeded and listed.
	w := call("GET", "/", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"key":"movies"`) {
		t.Fatalf("list %d %s", w.Code, w.Body)
	}

	// Create a console-only filter library (spec §15.3).
	w = call("POST", "/", `{"name":"Dramas","mediaType":"movie","rules":[{"action":"show","mediaType":"movie","match":"all","rules":[{"field":"genre","op":"in","value":["Drama"]}]}],"visibilityDefault":"hide"}`)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var lib libraryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &lib); err != nil {
		t.Fatal(err)
	}
	if lib.Source != core.LibraryCatalog || lib.Present.Vela != nil || lib.Present.Plex != nil || lib.Present.MediaBrowser != nil {
		t.Fatalf("new filter library must be console-only: %+v", lib.Present)
	}
	id := strconv.FormatInt(int64(lib.ID), 10)

	// Browsing it shows only the filtered set.
	w = call("GET", "/"+id+"/items", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Alpha Drama") || strings.Contains(w.Body.String(), "Beta Laughs") {
		t.Fatalf("filter browse %d %s", w.Code, w.Body)
	}

	// Rename + hide + show-in-apps (Vela + Plex) through PATCH; reject invalid Vela type.
	if bad := call("PATCH", "/"+id, `{"present":{"vela":{"type":"bad"}}}`); bad.Code != 400 {
		t.Fatalf("invalid vela type = %d %s, want 400", bad.Code, bad.Body)
	}
	w = call("PATCH", "/"+id, `{"name":"Serious films","hidden":true,"present":{"vela":{"type":"movies"},"plex":{"base":"movie"}}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Serious films") || !strings.Contains(w.Body.String(), `"hidden":true`) || !strings.Contains(w.Body.String(), `"vela":{"type":"movies"}`) {
		t.Fatalf("patch %d %s", w.Code, w.Body)
	}

	// The saved filter can be replaced under the same id.
	if w := call("PATCH", "/"+id, `{"rules":[],"visibilityDefault":"show"}`); w.Code != 200 {
		t.Fatalf("filter patch %d %s", w.Code, w.Body)
	}

	// Both preseeded and owner-created libraries can be deleted.
	if w := call("DELETE", "/1", ""); w.Code != 204 {
		t.Fatalf("builtin delete %d", w.Code)
	}
	if w := call("DELETE", "/"+id, ""); w.Code != 204 {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/"+id, ""); w.Code != 404 {
		t.Fatalf("get deleted %d", w.Code)
	}
}

func TestLibrariesConsoleCollectionSource(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	for _, title := range []core.Title{
		{Key: core.MovieKey(201), Type: core.TypeMovie, Title: "Listed Film", Year: 2005, Released: time.Date(2005, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2005, 6, 1, 0, 0, 0, 0, time.UTC)},
		{Key: core.MovieKey(202), Type: core.TypeMovie, Title: "Unlisted Film", Year: 2006, Released: time.Date(2006, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2006, 6, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	// Persist a collection whose members are exactly the listed film.
	now := time.Now().UTC()
	for _, stmt := range []string{
		`INSERT INTO collections_specs(id,slug,builtin,owner_user_id,spec_json,enabled,created_at,updated_at,last_built_at,build_hash,changed_at)
		 VALUES(31,'',0,0,'{}',1,?,?,?,'built',?)`,
		`INSERT INTO collections_members(spec_id,pos,key,shared) VALUES(31,0,?,1)`,
	} {
		args := []any{now.Unix(), now.Unix(), now.Unix(), now.Unix()}
		if strings.Contains(stmt, "collections_members") {
			args = []any{int64(core.MovieKey(201))}
		}
		if _, err := a.db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatal(err)
		}
	}

	c := &librariesConsole{app: a}
	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := call("GET", "/collection-sources", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":31`) {
		t.Fatalf("collection sources %d %s", w.Code, w.Body)
	}

	w = call("POST", "/", `{"name":"MDBList picks","collectionId":31,"mediaType":"movie"}`)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var lib libraryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &lib); err != nil {
		t.Fatal(err)
	}
	if lib.Source != core.LibraryCollection || lib.CollectionID != 31 || lib.MediaType != core.TypeMovie {
		t.Fatalf("created library = %+v", lib)
	}
	if lib.Present.Vela != nil || lib.Present.Plex != nil || lib.Present.MediaBrowser != nil {
		t.Fatalf("new collection library must be console-only: %+v", lib.Present)
	}
	id := strconv.FormatInt(int64(lib.ID), 10)
	w = call("GET", "/"+id+"/items", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Listed Film") || strings.Contains(w.Body.String(), "Unlisted Film") {
		t.Fatalf("collection browse %d %s", w.Code, w.Body)
	}

	// Deleting the collection leaves the library empty, never a 500.
	if _, err := a.db.ExecContext(ctx, `DELETE FROM collections_members WHERE spec_id=31`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM collections_specs WHERE id=31`); err != nil {
		t.Fatal(err)
	}
	w = call("GET", "/"+id+"/items", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "Listed Film") {
		t.Fatalf("deleted collection browse %d %s", w.Code, w.Body)
	}
	if w = call("GET", "/"+id, ""); w.Code != 200 {
		t.Fatalf("deleted collection decorate %d %s", w.Code, w.Body)
	}
}

func TestLibrariesConsolePluginItemMatchPinClear(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	lib, err := a.libraries.UpsertPluginLibrary(ctx, "demo", "book", libraries.PluginLibraryDecl{Name: "Books"})
	if err != nil {
		t.Fatal(err)
	}
	c := &librariesConsole{app: a}
	items, _, err := c.services()
	if err != nil {
		t.Fatal(err)
	}
	decl := plugins.MediaKindMeta{
		Key: "book", Label: "Books",
		Library: plugins.KindLibraryDecl{Name: "Books"},
		Levels:  []plugins.KindLevel{{Key: "book", Label: "Book", Playable: true}},
		Fields: []plugins.KindField{
			{Key: "author", Type: "string", Label: "Author", Indexed: true},
		},
		Browse: plugins.KindBrowseDecl{Facets: []string{"author"}, Search: true},
		Sync:   plugins.KindSyncDecl{Mode: plugins.KindSyncPush},
	}
	if _, err := items.UpsertKind(ctx, "demo", lib.ID, decl, false); err != nil {
		t.Fatal(err)
	}
	if _, err := items.UpsertItems(ctx, "demo", "book", []core.KindItem{{
		ID: "b1", Level: "book", Title: "Dune", Playable: true,
		Fields: map[string]json.RawMessage{"author": json.RawMessage(`"Herbert"`)},
	}}); err != nil {
		t.Fatal(err)
	}
	ref := string(core.NewItemRef("demo", "book", "b1"))
	base := "/" + strconv.FormatInt(int64(lib.ID), 10)
	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", base+"/items", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Dune") {
		t.Fatalf("items %d %s", w.Code, w.Body)
	}
	if w := call("GET", base+"/facets/author", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Herbert") {
		t.Fatalf("facets %d %s", w.Code, w.Body)
	}
	if w := call("PUT", base+"/item/"+ref+"/match", `{"candidateRef":"file-dune-epub"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"chosen":true`) {
		t.Fatalf("pin %d %s", w.Code, w.Body)
	}
	if w := call("DELETE", base+"/item/"+ref+"/match", ""); w.Code != 200 || strings.Contains(w.Body.String(), "file-dune-epub") {
		t.Fatalf("clear %d %s", w.Code, w.Body)
	}
	// Owner deletion removes publication, while explicit restoration keeps items.
	if w := call("DELETE", base, ""); w.Code != 204 {
		t.Fatalf("plugin delete %d", w.Code)
	}
	if _, err := a.libraries.UpsertPluginLibrary(ctx, "demo", "book", libraries.PluginLibraryDecl{Name: "Books"}); err != libraries.ErrDeleted {
		t.Fatalf("automatic resurrection: %v", err)
	}
	if _, err := a.libraries.RestorePluginLibrary(ctx, "demo", "book", libraries.PluginLibraryDecl{Name: "Books"}); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", base+"/items", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Dune") {
		t.Fatalf("restored items: %d %s", w.Code, w.Body)
	}
	// Cache view is available for plugin libraries only.
	if w := call("GET", base+"/cache", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"maxBytes":2147483648`) {
		t.Fatalf("cache %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/1/cache", ""); w.Code != 404 {
		t.Fatalf("builtin cache %d", w.Code)
	}
	_ = pluginitems.DefaultMatchThreshold
}

// This exercises the durable declaration, policy cache and already-running
// protocol experiences together; deleting a section never deletes shared data.
func TestLibraryDeletionProtocolLifecycle(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(991), Type: core.TypeMovie, Title: "Kept", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if err := a.state.Scrobble(ctx, owner.ID, title.Key); err != nil {
		t.Fatal(err)
	}
	before, err := a.state.Get(ctx, owner.ID, title.Key)
	if err != nil {
		t.Fatal(err)
	}
	experiences := map[core.Protocol]frontend.Experience{}
	for _, protocol := range []core.Protocol{"plex", "jellyfin", "emby", "stremio", "silo", "vela", "xtream", "dlna", "webdav", "m3u"} {
		exp, err := a.newSurfaceExperience(protocol)
		if err != nil {
			t.Fatal(err)
		}
		experiences[protocol] = exp
		views, err := exp.Views(ctx, owner)
		if err != nil || len(views) != 2 {
			t.Fatalf("%s seed views: %+v %v", protocol, views, err)
		}
	}
	srv := plex.NewServer(plex.Deps{Host: frontend.Host{Experience: experiences["plex"], Sessions: sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{})}, DisableEnrichment: true})
	defer srv.Close()
	if err := a.libraries.SetRules(ctx, 1, nil, "hide"); err != nil {
		t.Fatal(err)
	}
	if err := a.refreshLibraryPolicies(ctx); err != nil {
		t.Fatal(err)
	}
	h := (&librariesConsole{app: a}).handler()
	for _, id := range []string{"1", "2"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("DELETE", "/"+id, nil))
		if w.Code != 204 {
			t.Fatalf("delete %s: %d %s", id, w.Code, w.Body)
		}
	}
	if len(srv.LibrarySections()) != 0 {
		t.Fatal("Plex retained phantom sections")
	}
	for protocol, exp := range experiences {
		views, err := exp.Views(ctx, owner)
		if err != nil || len(views) != 0 {
			t.Fatalf("%s deleted views: %+v %v", protocol, views, err)
		}
		if _, err := exp.LibraryItems(ctx, owner, core.Device{}, "1", frontend.ItemQuery{}); err == nil {
			t.Fatalf("%s retained deleted route", protocol)
		}
		rows, err := exp.Rows(ctx, frontend.RowRequest{Viewer: owner, Device: core.Device{ID: "lifecycle"}, Placement: frontend.PlacementHome, Limit: 10})
		if err != nil {
			t.Fatalf("%s empty Home: %v", protocol, err)
		}
		for _, row := range rows {
			if len(row.Items) > 0 || len(row.Entries) > 0 {
				t.Fatalf("%s empty Home has titles: %+v", protocol, row)
			}
		}
	}
	if _, err := a.catalog.StoredTitle(ctx, title.Key); err != nil {
		t.Fatalf("media lost: %v", err)
	}
	after, err := a.state.Get(ctx, owner.ID, title.Key)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("history changed: %+v %+v %v", before, after, err)
	}
	var present core.Presentations
	if err := json.Unmarshal([]byte(`{"plex":{"base":"movie"}}`), &present); err != nil {
		t.Fatal(err)
	}
	lib, err := a.libraries.Create(ctx, "New films", core.TypeMovie, nil, "show", present)
	if err != nil || lib.ID <= 2 {
		t.Fatalf("recreation: %+v %v", lib, err)
	}
	page, err := experiences["plex"].LibraryItems(ctx, owner, core.Device{}, strconv.Itoa(int(lib.ID)), frontend.ItemQuery{Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("recreated library inherits deleted policy: %+v %v", page, err)
	}
	if velaViews, err := experiences["vela"].Views(ctx, owner); err != nil || len(velaViews) != 0 {
		t.Fatalf("plex-only library leaked into Vela: %+v %v", velaViews, err)
	}
	if sections := srv.LibrarySections(); len(sections) != 1 || sections[0].ID != int(lib.ID) {
		t.Fatalf("recreated sections: %+v", sections)
	}
}
