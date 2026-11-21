package plex

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Search shares the indexed, policy-filtered Live TV search with other apps.
func (s *Server) handleLiveChannelSearch(w http.ResponseWriter, r *http.Request) {
	limit, ok := searchLimit(w, r, 10, 50)
	if !ok {
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	if utf8.RuneCountInString(query) > 256 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	mc := &model.MediaContainer{Identifier: frontend.LiveTVProvider}
	if query == "" {
		s.write(w, r, mc)
		return
	}
	search, ok := s.svc().liveTV.(frontend.LiveTVSearch)
	if !ok {
		s.write(w, r, mc)
		return
	}
	results, err := search.SearchLiveTV(r.Context(), FromContext(r.Context()).User, query, limit)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	for _, c := range results.Channels {
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: 1, Channel: s.liveSearchChannel(r, c)})
	}
	parents := map[string]frontend.LiveTVChannel{}
	for _, c := range results.ProgrammeChannels {
		parents[c.ID] = c
	}
	now := s.now()
	for _, p := range results.Programmes {
		c := parents[p.ChannelID]
		md := s.liveProgrammeMetadata(c, p)
		s.applyGuideChannelThumb(r, &md, c)
		if !now.Before(p.Start) && !now.After(p.End) && len(md.Media) > 0 {
			md.Media[0].Extra = append(md.Media[0].Extra, model.A("onAir", true))
		}
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: 0.9, Metadata: &md})
	}
	mc.Size = len(mc.SearchResult)
	s.write(w, r, mc)
}

func (s *Server) liveSearchChannel(r *http.Request, c frontend.LiveTVChannel) *model.SearchChannel {
	categories := model.StringList{}
	if c.Group != "" {
		categories = append(categories, c.Group)
	}
	logo := s.liveGuideLogo(r, c)
	return &model.SearchChannel{
		ID:                 c.ID,
		Identifier:         c.ID,
		ChannelIdentifier:  c.ID,
		Slug:               c.ID,
		Key:                liveProviderPath + "/channels/" + url.PathEscape(c.ID),
		Type:               "channel",
		GridKey:            c.ID,
		VCN:                c.Number,
		Title:              c.Name,
		ShortTitle:         c.Name,
		CallSign:           c.Name,
		Thumb:              logo,
		CoverPoster:        logo,
		Language:           "eng",
		ProviderIdentifier: frontend.LiveTVProvider,
		Source:             "server://" + s.identity().MachineIdentifier + "/" + frontend.LiveTVProvider,
		Categories:         categories,
	}
}

func (s *Server) appendLiveSearchHubs(r *http.Request, mc *model.MediaContainer, query string, limit int) {
	if !s.liveTVEnabled() || r.URL.Query().Get("sectionId") != "" {
		return
	}
	viewer := FromContext(r.Context()).User
	if !s.svc().liveTV.VisibleTo(r.Context(), viewer) {
		return
	}
	search, ok := s.svc().liveTV.(frontend.LiveTVSearch)
	if !ok {
		return
	}
	results, err := search.SearchLiveTV(r.Context(), viewer, query, limit)
	if err != nil {
		s.log.Warn("live search unavailable", "error", err)
		return
	}
	source := "server://" + s.identity().MachineIdentifier + "/" + frontend.LiveTVProvider
	var liveHubs []model.Hub
	if len(results.Channels) > 0 {
		hub := model.Hub{
			Title:         "Live TV",
			Type:          "clip",
			HubIdentifier: "livetv",
			Context:       "hub.livetv",
			Style:         "shelf",
			Source:        source,
			Identifier:    frontend.LiveTVProvider,
		}
		for i, c := range results.Channels {
			md := s.liveChannelMetadata(r, c)
			md.Score = strconv.FormatFloat(reportedScore(searchScore(i, len(results.Channels))), 'f', 5, 64)
			md.Extra = append(md.Extra, model.A("source", source))
			hub.Metadata = append(hub.Metadata, md)
		}
		hub.Size = len(hub.Metadata)
		liveHubs = append(liveHubs, hub)
	}
	parents := map[string]frontend.LiveTVChannel{}
	for _, c := range results.ProgrammeChannels {
		parents[c.ID] = c
	}
	if len(results.Programmes) > 0 {
		now := s.now()
		hub := model.Hub{
			Title:         "On TV",
			Type:          "episode",
			HubIdentifier: "tv.plex.provider.epg",
			Context:       "hub.tv.plex.provider.epg",
			Style:         "shelf",
			Source:        source,
			Identifier:    frontend.LiveTVProvider,
		}
		for i, p := range results.Programmes {
			c := parents[p.ChannelID]
			md := s.liveProgrammeMetadata(c, p)
			s.applyGuideChannelThumb(r, &md, c)
			if !now.Before(p.Start) && !now.After(p.End) && len(md.Media) > 0 {
				md.Media[0].Extra = append(md.Media[0].Extra, model.A("onAir", true))
			}
			md.Score = strconv.FormatFloat(reportedScore(searchScore(i, len(results.Programmes))), 'f', 5, 64)
			md.Extra = append(md.Extra, model.A("source", source))
			hub.Metadata = append(hub.Metadata, md)
		}
		hub.Size = len(hub.Metadata)
		liveHubs = append(liveHubs, hub)
	}
	if len(liveHubs) == 0 {
		return
	}
	insertAt := 0
	for i, h := range mc.Hub {
		if h.Size > 0 {
			insertAt = i + 1
		}
	}
	merged := make([]model.Hub, 0, len(mc.Hub)+len(liveHubs))
	merged = append(merged, mc.Hub[:insertAt]...)
	merged = append(merged, liveHubs...)
	merged = append(merged, mc.Hub[insertAt:]...)
	mc.Hub = merged
}
