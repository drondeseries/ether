package plex

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/homefilter"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func isVideoSection(sec Section) bool {
	return sec.PluginID == "" && (sec.Type == frontend.TypeMovie || sec.Type == frontend.TypeShow)
}

func isKindMusicSection(sec Section) bool {
	return isKindSection(sec) && string(sec.Type) == plexBaseArtist
}

func parseContentDirectories(raw string) ([]int, bool) {
	var ids []int
	playlists := false
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(token)
		if token == "playlists" {
			playlists = true
			continue
		}
		if n, err := strconv.Atoi(token); err == nil && n > 0 {
			ids = append(ids, n)
		}
	}
	return ids, playlists
}

func (s *Server) allowedContentSections(u *frontend.User, ids []int) []Section {
	var out []Section
	seen := map[int]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		if sec, ok := s.sectionByID(id); ok && (isVideoSection(sec) || isKindMusicSection(sec)) && s.canSeeSection(u, id) {
			seen[id] = true
			out = append(out, sec)
		}
	}
	return out
}

func (s *Server) allowedSectionIDs(u *frontend.User, ids []int) []int {
	secs := s.allowedContentSections(u, ids)
	out := make([]int, len(secs))
	for i, sec := range secs {
		out[i] = sec.ID
	}
	return out
}

// firstPinnedSectionID resolves the single library section on Home that owns
// cross-library Continue Watching / On Deck when excludeContinueWatching is
// not set. Unknown, non-video, or unauthorized directory tokens are skipped;
// when pinned names none, the viewer's first accessible video section wins.
func (s *Server) firstPinnedSectionID(u *frontend.User, pinned string) int {
	for _, token := range strings.Split(pinned, ",") {
		id, err := strconv.Atoi(strings.TrimSpace(token))
		if err != nil || id <= 0 {
			continue
		}
		if sec, ok := s.sectionByID(id); ok && isVideoSection(sec) && s.canSeeSection(u, id) {
			return sec.ID
		}
	}
	for _, sec := range s.userSections(u) {
		if isVideoSection(sec) {
			return sec.ID
		}
	}
	return 0
}

// sectionHubID scopes a promoted row's hubIdentifier to sec so two pinned
// sections on Home never emit the same hubIdentifier (including section views
// of mixed discovery/collection rows, or custom libraries sharing a media type).
func sectionHubID(id string, sec Section) string {
	switch id {
	case "", "home.continue", "home.ondeck", "home.playlists":
		return id
	}
	prefix := "movie."
	if sec.Type == frontend.TypeShow {
		prefix = "tv."
	}
	out := id
	if !strings.HasPrefix(out, "movie.") && !strings.HasPrefix(out, "tv.") {
		out = prefix + out
	}
	if isKindSection(sec) {
		suffix := "." + strconv.Itoa(sec.ID)
		if !strings.HasSuffix(out, suffix) {
			out += suffix
		}
	}
	return out
}

// candidateHubIDs returns id followed by its un-scoped producer identifiers
// when sections names a single library, so /hubs/items continuations still
// resolve after emittedHubs expires or the server restarts.
func (s *Server) candidateHubIDs(id string, sections []int) []string {
	out := []string{id}
	add := func(v string) {
		if v == "" {
			return
		}
		for _, existing := range out {
			if existing == v {
				return
			}
		}
		out = append(out, v)
	}
	if len(sections) != 1 {
		return out
	}
	sec, ok := s.sectionByID(sections[0])
	if !ok {
		return out
	}
	cur := id
	if isKindSection(sec) {
		if trimmed, ok := strings.CutSuffix(cur, "."+strconv.Itoa(sec.ID)); ok {
			cur = trimmed
			add(cur)
		}
	}
	for _, prefix := range []string{"movie.", "tv."} {
		if trimmed, ok := strings.CutPrefix(cur, prefix); ok {
			add(trimmed)
		}
	}
	return out
}

func matchHubID(hID string, candidates []string) bool {
	for _, c := range candidates {
		if hID == c {
			return true
		}
	}
	return false
}

func (s *Server) handleSectionHubs(w http.ResponseWriter, r *http.Request) {
	s.sectionHubs(w, r, false)
}

func (s *Server) handlePromotedSectionHubs(w http.ResponseWriter, r *http.Request) {
	s.sectionHubs(w, r, true)
}

func (s *Server) sectionHubs(w http.ResponseWriter, r *http.Request, promotedOnly bool) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	route := "hubs/sections/" + strconv.Itoa(sec.ID)
	if promotedOnly {
		route += "/promoted"
	}
	s.serveHome(w, r, route, func(_ context.Context, r *http.Request) homeBuild {
		return s.buildSectionHubs(r, sec, promotedOnly)
	})
}

func (s *Server) kindMusicArtistsHub(ctx context.Context, pr *Request, sec Section, visibleCount int) (model.Hub, bool) {
	exp := s.exp()
	if exp == nil || visibleCount <= 0 {
		return model.Hub{}, false
	}
	page, err := exp.LibraryItems(ctx, pr.User, plexDevice(pr), sec.IDString(), frontend.ItemQuery{Limit: visibleCount + 1})
	if err != nil {
		s.log.Warn("music section hubs", "section", sec.ID, "err", err)
		return model.Hub{}, false
	}
	items := page.Items
	hasExtra := len(items) > visibleCount
	if hasExtra {
		items = items[:visibleCount]
	}
	byKey := make(map[string]*frontend.Title, len(items))
	for i := range items {
		byKey[items[i].Key.String()] = &items[i]
	}
	secCopy := sec
	vis := s.sectionVisibility(sec)
	md := s.renderTitles(ctx, pr, items, renderOpts{media: true, storedOnly: true, vis: &vis, section: &secCopy})
	if len(md) == 0 {
		return model.Hub{}, false
	}
	keys := make([]string, 0, len(md))
	for i := range md {
		md[i].Type = kindTypeForBase(string(sec.Type), md[i].Type, byKey[md[i].RatingKey])
		keys = append(keys, md[i].RatingKey)
	}
	id := sec.IDString()
	total := max(page.Total, len(md))
	if hasExtra && total <= len(md) {
		total = len(md) + 1
	}
	return model.Hub{
		HubKey:        "/library/metadata/" + strings.Join(keys, ","),
		Key:           "/library/sections/" + id + "/all?type=" + strconv.Itoa(typeNumArtist),
		Title:         "Artists",
		Type:          plexBaseArtist,
		HubIdentifier: "music.artists." + id,
		Context:       "hub.music.artists",
		Size:          len(md),
		TotalSize:     model.Ptr(total),
		More:          hasExtra || page.Total > len(md),
		Style:         "shelf",
		Promoted:      model.Ptr(true),
		Metadata:      md,
	}, true
}

func (s *Server) buildSectionHubs(r *http.Request, sec Section, promotedOnly bool) homeBuild {
	batch := &shelfBatch{}
	ctx := withStoredPolicy(context.WithValue(r.Context(), shelfBatchKey{}, batch))
	defer func() { s.enqueueShelves(batch.records) }()
	pr := FromContext(ctx)
	visibleCount := countParam(r, 12)
	count := 200
	mc := hubContainer()
	mc.LibrarySectionID = model.Ptr(sec.ID)
	mc.LibrarySectionTitle = sec.Title
	mc.LibrarySectionUUID = sec.UUID
	if isKindMusicSection(sec) {
		if mh, ok := s.kindMusicArtistsHub(ctx, pr, sec, visibleCount); ok {
			mc.Hub = []model.Hub{mh}
		}
		mc.Size = len(mc.Hub)
		return homeBuild{mc: mc, section: sec.ID}
	}
	if !isVideoSection(sec) {
		return homeBuild{mc: mc, section: sec.ID}
	}
	secCopy := sec
	hc := hubCtx{section: sec.ID, visible: visibleCount, renderSec: &secCopy}
	pc := s.hubContext(pr, sec.ID)
	vis := s.sectionVisibility(sec)
	served := map[string]frontend.Hub{}
	var layoutHubs []frontend.Hub
	var layoutNeutral []frontend.NeutralHomeRow
	add := func(hs hubSpec) {
		mh := s.renderHubWithVisibility(ctx, pr, hs, hc, vis)
		mc.Hub = append(mc.Hub, mh)
		hub, neutral := homeLayoutFacts(mh)
		layoutHubs = append(layoutHubs, hub)
		layoutNeutral = append(layoutNeutral, neutral)
		served[mh.HubIdentifier] = hs.Hub
	}
	if !promotedOnly && !metadataOptionOn(r.URL.Query().Get("excludeContinueWatching")) && !isKindSection(sec) {
		add(s.continueHub(ctx, pr, cwSection, sec, visibleCount))
	}
	if !isKindSection(sec) {
		if ra, err := s.recentlyAddedHub(ctx, pr, sec, count); err == nil {
			add(ra)
		} else {
			s.log.Warn("recently added hub", "section", sec.ID, "err", err)
		}
	}
	var sectionPlaylist *model.Hub
	if !promotedOnly && r.URL.Query().Get("includeLibraryPlaylists") == "1" {
		if h, ok := s.sectionPlaylistsHub(ctx, pr, sec.ID, visibleCount); ok {
			sectionPlaylist = &h
		}
	}
	provisional := false
	if s.recommender() != nil {
		hubs, err := s.recommendSectionFor(ctx, pr, sec, pc, s.saferFetchCount(pr.User, count))
		if err != nil {
			s.log.Warn("section hubs", "section", sec.ID, "err", err)
		}
		provisional = !hasRecommendedRows(hubs)
		for _, h := range hubs {
			if view, ok := frontend.SectionView(h, sec.Type); ok {
				if promotedOnly {
					view.Promoted = true
				}
				add(hubSpec{Hub: view})
			}
		}
	}
	if !isKindSection(sec) {
		// Mixed discovery rows (watchlist, Rediscover) keep only this section's titles.
		for _, hs := range s.discoveryHubs(ctx, pr, count) {
			if h, ok := frontend.SectionView(hs.Hub, sec.Type); ok {
				if promotedOnly {
					h.Promoted = true
				}
				hs.Hub = h
				add(hs)
			}
		}
	}
	sectionName := "library:" + strconv.Itoa(sec.ID)
	mc.Hub = stableHomeHubs(s.arrangeHomeRows(ctx, pr, pc, sectionName, mc.Hub, layoutHubs, layoutNeutral))
	if promotedOnly {
		for i := range mc.Hub {
			oldID := mc.Hub[i].HubIdentifier
			newID := sectionHubID(oldID, sec)
			if newID == oldID {
				continue
			}
			mc.Hub[i].HubIdentifier = newID
			if h, ok := served[oldID]; ok {
				served[newID] = h
				rem := h
				rem.ID = newID
				s.rememberHub(ctx, pr, hubSpec{Hub: rem}, hc)
				if strings.HasPrefix(mc.Hub[i].Key, "/hubs/items/") {
					mc.Hub[i].Key = hubItemsKey(rem, hubCtx{section: sec.ID})
				}
			}
		}
	}
	if sectionPlaylist != nil {
		at := 0
		for i, h := range mc.Hub {
			if strings.Contains(h.HubIdentifier, ".recentlyadded.") {
				at = i + 1
				break
			}
		}
		mc.Hub = append(mc.Hub, model.Hub{})
		copy(mc.Hub[at+1:], mc.Hub[at:])
		mc.Hub[at] = *sectionPlaylist
	}

	s.log.Debug("section hubs rendered", "section", sec.ID, "user", pr.User.ID, "count", count, "rows", len(mc.Hub))
	mc.Size = len(mc.Hub)
	return homeBuild{mc: mc, served: served, section: sec.ID, provisional: provisional}
}

// buildScopedHomeHubs builds PMS-shaped Home responses when contentDirectoryID
// scopes /hubs or /hubs/promoted to one or more pinned directories. Each
// library contributes only its own rows and titles of its own media type and
// visibility; cross-library Continue Watching / On Deck appears at most once
// on the first pinned library when excludeContinueWatching is not set.
func (s *Server) buildScopedHomeHubs(r *http.Request, promotedOnly bool) homeBuild {
	ctx := withStoredPolicy(r.Context())
	pr := FromContext(ctx)
	q := r.URL.Query()
	count := countParam(r, 12)
	rawDir := strings.TrimSpace(q.Get("contentDirectoryID"))
	dirIDs, wantPlaylists := parseContentDirectories(rawDir)
	allowedSecs := s.allowedContentSections(pr.User, dirIDs)
	if len(allowedSecs) == 0 && !wantPlaylists {
		return homeBuild{mc: hubContainer()}
	}
	perDirectory := !strings.Contains(rawDir, ",")
	shelves := promotedOnly && perDirectory && len(allowedSecs) == 1 && isVideoSection(allowedSecs[0]) && !wantPlaylists && s.wantsCollectionShelves(pr)
	pinned := q.Get("pinnedContentDirectoryID")
	if shelves && strings.TrimSpace(pinned) == "" {
		pinned = s.defaultPinnedDirectories(pr.User)
	}
	var selected map[string]bool
	if ids := q["identifier"]; len(ids) > 0 {
		selected = map[string]bool{}
		for _, v := range ids {
			for _, id := range strings.Split(v, ",") {
				selected[id] = true
			}
		}
	}
	firstSec := s.firstPinnedSectionID(pr.User, pinned)
	includeContinue := false
	if !metadataOptionOn(q.Get("excludeContinueWatching")) && firstSec != 0 {
		for _, sec := range allowedSecs {
			if sec.ID == firstSec {
				includeContinue = true
				break
			}
		}
	}
	if includeContinue {
		if layout, ok := s.recommender().(frontend.HomeRowLayoutGate); ok && !layout.HomeRowEnabled(ctx, pr.User, s.hubContext(pr, 0), "home.continue") {
			includeContinue = false
		}
	}
	needDiscovery := false
	for _, sec := range allowedSecs {
		if !isKindSection(sec) {
			needDiscovery = true
			break
		}
	}
	var sharedDisc []frontend.Hub
	if needDiscovery {
		disc, filter, err := s.sharedDiscovery(ctx, pr, s.hubContext(pr, 0), rawDir, pinned, count)
		if filter != nil {
			ctx = homefilter.With(ctx, filter)
		}
		if err != nil {
			s.log.Warn("home discovery hubs", "err", err)
		}
		sharedDisc = disc
	}

	served := map[string]frontend.Hub{}
	var allRows []model.Hub
	provisional := false
	for _, sec := range allowedSecs {
		if isKindMusicSection(sec) {
			if mh, ok := s.kindMusicArtistsHub(ctx, pr, sec, count); ok {
				if selected == nil || selected[mh.HubIdentifier] {
					allRows = append(allRows, mh)
				}
			}
			continue
		}
		secCopy := sec
		renderHC := hubCtx{visible: count, renderSec: &secCopy}
		pc := s.hubContext(pr, sec.ID)
		vis := s.sectionVisibility(sec)
		var secRows []model.Hub
		var layoutHubs []frontend.Hub
		var layoutNeutral []frontend.NeutralHomeRow
		origByID := map[string]frontend.Hub{}
		appendRow := func(mh model.Hub, src frontend.Hub, record bool) {
			scoped := sectionHubID(mh.HubIdentifier, sec)
			if selected != nil && !selected[mh.HubIdentifier] && !selected[scoped] {
				return
			}
			secRows = append(secRows, mh)
			hFact, nFact := homeLayoutFacts(mh)
			layoutHubs = append(layoutHubs, hFact)
			layoutNeutral = append(layoutNeutral, nFact)
			if record {
				origByID[mh.HubIdentifier] = src
			}
		}
		if includeContinue && sec.ID == firstSec {
			_, ondeckHub, cwSpec, cwRendered := s.prepareHomeContinueAndOnDeck(ctx, pr, pc, nil, selected, true, count)
			appendRow(cwRendered, cwSpec.Hub, true)
			if ondeckHub != nil {
				appendRow(*ondeckHub, frontend.Hub{ID: "home.ondeck", Title: ondeckHub.Title, Type: frontend.TypeEpisode, Promoted: true}, true)
			}
		}
		if !isKindSection(sec) {
			if ra, err := s.recentlyAddedHubPage(ctx, pr, sec, count, true); err == nil {
				mh := s.renderHubWithVisibility(ctx, pr, ra, renderHC, vis)
				appendRow(mh, ra.Hub, true)
			}
		}
		if s.recommender() != nil {
			sectionHubs, err := s.recommendSectionFor(ctx, pr, sec, pc, s.saferFetchCount(pr.User, count))
			if err != nil {
				s.log.Warn("section home hubs", "section", sec.ID, "err", err)
			}
			if !hasRecommendedRows(sectionHubs) {
				provisional = true
			}
			for _, h := range sectionHubs {
				view, ok := frontend.SectionView(h, sec.Type)
				if !ok {
					continue
				}
				if promotedOnly {
					view.Promoted = true
				}
				scopedID := sectionHubID(view.ID, sec)
				rem := view
				rem.ID = scopedID
				s.rememberHub(ctx, pr, hubSpec{Hub: rem}, hubCtx{section: sec.ID})
				spec := hubSpec{Hub: view, key: hubItemsKey(rem, hubCtx{section: sec.ID})}
				mh := s.renderHubWithVisibility(ctx, pr, spec, renderHC, vis)
				appendRow(mh, view, true)
			}
		}
		if !isKindSection(sec) {
			for _, dh := range sharedDisc {
				view, ok := frontend.SectionView(dh, sec.Type)
				if !ok {
					continue
				}
				if promotedOnly {
					view.Promoted = true
				}
				scopedID := sectionHubID(view.ID, sec)
				rem := view
				rem.ID = scopedID
				s.rememberHub(ctx, pr, hubSpec{Hub: rem}, hubCtx{section: sec.ID})
				spec := hubSpec{Hub: view, key: hubItemsKey(rem, hubCtx{section: sec.ID})}
				mh := s.renderHubWithVisibility(ctx, pr, spec, renderHC, vis)
				appendRow(mh, view, true)
			}
		}
		secRows = s.arrangeHomeRows(ctx, pr, pc, "home", secRows, layoutHubs, layoutNeutral)
		for _, mh := range secRows {
			oldID := mh.HubIdentifier
			mh.HubIdentifier = sectionHubID(oldID, sec)
			if mh.HubIdentifier != "home.continue" && strings.EqualFold(mh.Style, "hero") {
				mh.Style = "shelf"
			}
			if src, ok := origByID[oldID]; ok {
				served[mh.HubIdentifier] = src
			}
			allRows = append(allRows, mh)
		}
	}
	if wantPlaylists {
		if plHub, _, ok := s.playlistsHubProjection(ctx, pr, count); ok && (selected == nil || selected[plHub.HubIdentifier]) {
			hFact, nFact := homeLayoutFacts(plHub)
			arranged := s.arrangeHomeRows(ctx, pr, s.hubContext(pr, 0), "home", []model.Hub{plHub}, []frontend.Hub{hFact}, []frontend.NeutralHomeRow{nFact})
			for _, mh := range arranged {
				if strings.EqualFold(mh.Style, "hero") {
					mh.Style = "shelf"
				}
				allRows = append(allRows, mh)
			}
		}
	}

	mc := hubContainer()
	mc.Hub = stableHomeHubs(allRows)
	if shelves {
		secID := allowedSecs[0].ID
		var renamed map[string]shelfRef
		mc.Hub, renamed = collectionShelves(mc.Hub, secID)
		s.rememberShelves(renamed)
		for id, ref := range renamed {
			if h, ok := served[ref.hub]; ok {
				served[id] = h
			}
		}
	}
	mc.Size = len(mc.Hub)
	buildSec := 0
	if len(allowedSecs) == 1 && !wantPlaylists {
		buildSec = allowedSecs[0].ID
	}
	return homeBuild{mc: mc, served: served, section: buildSec, provisional: provisional}
}
