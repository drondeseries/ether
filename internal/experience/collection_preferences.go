package experience

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	defaultListLimit = 50
	maxListLimit     = 500
)

func sliceBounds(total, offset, limit int) (int, int) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	start := min(max(offset, 0), total)
	end := total
	if limit > 0 && limit < total-start {
		end = start + limit
	}
	return start, end
}

// Collections filters member visibility before calculating totals and paging.
func (s *Service) Collections(ctx context.Context, viewer *core.Viewer, _ core.Device, q frontend.CollectionQuery) (frontend.CollectionPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.CollectionPage{}, err
	}
	if s.d.Collections == nil {
		return frontend.CollectionPage{}, core.ErrNotAvailable
	}
	if q.ViewID != "" {
		return s.viewCollections(ctx, v, q)
	}
	requested := map[core.MediaType]bool{}
	for _, typ := range q.Types {
		if typ != core.TypeMovie && typ != core.TypeShow {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
		requested[typ] = true
	}
	complete, ok := s.d.Collections.(interface {
		ListComplete(context.Context, *core.User, core.MediaType) ([]core.Collection, error)
	})
	if !ok {
		return frontend.CollectionPage{}, core.ErrNotAvailable
	}
	published := map[core.MediaType]bool{}
	if s.d.PluginLibraries != nil {
		views, e := s.storedViews(ctx, v)
		if e != nil {
			return frontend.CollectionPage{}, e
		}
		for _, view := range views {
			if view.KindKey == "" {
				published[view.Type] = true
			}
		}
	}

	seenTypes := map[core.MediaType]bool{}
	seenKeys := map[core.RatingKey]bool{}
	var visible []core.Collection
	for _, lib := range s.libraries() {
		typ := lib.Type
		if (s.d.PluginLibraries != nil && !published[typ]) || (typ != core.TypeMovie && typ != core.TypeShow) || !SectionAllowed(v, lib.ID) || (len(requested) > 0 && !requested[typ]) || seenTypes[typ] {
			continue
		}
		seenTypes[typ] = true
		if q.ExistsOnly {
			finder, ok := s.d.Collections.(interface {
				FindVisible(context.Context, *core.User, core.MediaType, func(core.Collection) (bool, error)) (bool, error)
			})
			if !ok {
				return frontend.CollectionPage{}, core.ErrNotAvailable
			}
			found, e := finder.FindVisible(ctx, v, typ, func(c core.Collection) (bool, error) {
				if c.Type != typ || !s.d.Visibility.CollectionAllowed(v, &c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
					return false, nil
				}
				return s.collectionHasVisibleMember(ctx, v, c)
			})
			if e != nil {
				return frontend.CollectionPage{}, e
			}
			if found {
				return frontend.CollectionPage{Total: 1}, nil
			}
			continue
		}
		collections, e := complete.ListComplete(ctx, v, typ)
		if e != nil {
			return frontend.CollectionPage{}, e
		}
		for _, c := range collections {
			if (q.Query != "" && !strings.Contains(strings.ToLower(c.Title), strings.ToLower(q.Query))) || c.Type != typ || seenKeys[c.Key] || !s.d.Visibility.CollectionAllowed(v, &c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
				continue
			}
			members, e := s.collectionItems(ctx, v, c.Key)
			if e != nil {
				return frontend.CollectionPage{}, e
			}
			if len(members) == 0 {
				continue
			}
			c.ChildCount = len(members)
			seenKeys[c.Key] = true
			visible = append(visible, c)
		}
	}
	start, end := sliceBounds(len(visible), q.Offset, q.Limit)
	return frontend.CollectionPage{Items: visible[start:end], Total: len(visible)}, nil
}

func (s *Service) viewCollections(ctx context.Context, v *core.User, q frontend.CollectionQuery) (frontend.CollectionPage, error) {
	id, err := strconv.Atoi(q.ViewID)
	if err != nil {
		return frontend.CollectionPage{}, core.ErrNotFound
	}
	var typ core.MediaType
	var stored *core.Library
	if s.d.PluginLibraries != nil {
		lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id))
		if err != nil {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
		stored = &lib
		mt := filterMediaType(lib)
		if lib.Source != core.LibraryCollection {
			var ok bool
			mt, _, ok = s.publishedLibrary(ctx, lib)
			if !ok {
				return frontend.CollectionPage{}, core.ErrNotFound
			}
		} else if lib.Hidden {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
		if !v.AllowsLibrary(lib.ID) {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
		if lib.Source == core.LibraryPlugin {
			return frontend.CollectionPage{Items: []core.Collection{}, Total: 0}, nil
		}
		typ = mt
	} else {
		var found bool
		for _, lib := range s.libraries() {
			if lib.ID == id {
				typ = lib.Type
				found = true
				break
			}
		}
		if !found || !SectionAllowed(v, id) {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
	}
	if typ != core.TypeMovie && typ != core.TypeShow {
		return frontend.CollectionPage{Items: []core.Collection{}, Total: 0}, nil
	}
	for _, r := range q.Types {
		if r != core.TypeMovie && r != core.TypeShow {
			return frontend.CollectionPage{}, core.ErrNotFound
		}
	}
	if len(q.Types) > 0 {
		match := false
		for _, r := range q.Types {
			if r == typ {
				match = true
				break
			}
		}
		if !match {
			return frontend.CollectionPage{Items: []core.Collection{}, Total: 0}, nil
		}
	}
	complete, ok := s.d.Collections.(interface {
		ListComplete(context.Context, *core.User, core.MediaType) ([]core.Collection, error)
	})
	if !ok {
		return frontend.CollectionPage{}, core.ErrNotAvailable
	}
	collections, err := complete.ListComplete(ctx, v, typ)
	if err != nil {
		return frontend.CollectionPage{}, err
	}
	type candidate struct {
		col     core.Collection
		members []core.Title
	}
	var candidates []candidate
	var allKeys []core.RatingKey
	seenMemberKey := map[core.RatingKey]bool{}
	seenColKey := map[core.RatingKey]bool{}
	for _, c := range collections {
		if (q.Query != "" && !strings.Contains(strings.ToLower(c.Title), strings.ToLower(q.Query))) || c.Type != typ || seenColKey[c.Key] || !s.d.Visibility.CollectionAllowed(v, &c) || (c.OwnerUserID != 0 && c.OwnerUserID != v.ID) {
			continue
		}
		members, err := s.collectionItems(ctx, v, c.Key)
		if err != nil {
			return frontend.CollectionPage{}, err
		}
		if len(members) == 0 {
			continue
		}
		seenColKey[c.Key] = true
		candidates = append(candidates, candidate{col: c, members: members})
		for _, m := range members {
			if !seenMemberKey[m.Key] {
				seenMemberKey[m.Key] = true
				allKeys = append(allKeys, m.Key)
			}
		}
	}
	var allowedInLib map[core.RatingKey]bool
	scopedToLib := false
	if stored != nil && stored.ID != core.LibraryMovies && stored.ID != core.LibraryShows && len(allKeys) > 0 {
		if s.d.LibraryMembers != nil {
			var ok bool
			allowedInLib, _, ok, err = s.d.LibraryMembers(ctx, stored.ID, allKeys)
			if err != nil {
				return frontend.CollectionPage{}, err
			}
			scopedToLib = ok
		}
	}
	var visible []core.Collection
	for _, cand := range candidates {
		count := 0
		for _, m := range cand.members {
			if m.Type != typ {
				continue
			}
			if scopedToLib && !allowedInLib[m.Key] {
				continue
			}
			count++
		}
		if count == 0 {
			continue
		}
		c := cand.col
		c.ChildCount = count
		visible = append(visible, c)
		if q.ExistsOnly {
			return frontend.CollectionPage{Total: 1}, nil
		}
	}
	if q.ExistsOnly {
		return frontend.CollectionPage{Total: 0}, nil
	}
	start, end := sliceBounds(len(visible), q.Offset, q.Limit)
	return frontend.CollectionPage{Items: visible[start:end], Total: len(visible)}, nil
}

func (s *Service) libraryCollectionItems(ctx context.Context, v *core.User, device core.Device, library *Library, stored *core.Library, q frontend.ItemQuery) (core.Page, error) {
	detail, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Device: device, Key: q.CollectionKey})
	if err != nil || detail.Collection == nil {
		if err != nil {
			return core.Page{}, err
		}
		return core.Page{}, core.ErrNotFound
	}
	items, err := s.collectionItems(ctx, v, q.CollectionKey)
	if err != nil {
		return core.Page{}, err
	}
	var allowedInLib map[core.RatingKey]bool
	scopedToLib := false
	if stored != nil && stored.ID != core.LibraryMovies && stored.ID != core.LibraryShows && len(items) > 0 && s.d.LibraryMembers != nil {
		keys := make([]core.RatingKey, len(items))
		for i := range items {
			keys[i] = items[i].Key
		}
		var ok bool
		allowedInLib, _, ok, err = s.d.LibraryMembers(ctx, stored.ID, keys)
		if err != nil {
			return core.Page{}, err
		}
		scopedToLib = ok
	}
	// Strip library-level CatalogueSources/ExtraRules/CollectionMembers already
	// enforced by LibraryMembers, while keeping user query predicates.
	userQuery := q
	userQuery.Filter.CatalogueSources = nil
	userQuery.Filter.ExtraRules = nil
	userQuery.Filter.ExtraFallback = ""
	userQuery.Filter.CollectionMembers = 0
	filtered := make([]core.Title, 0, len(items))
	for _, title := range items {
		if title.Type != library.Type {
			continue
		}
		if scopedToLib && !allowedInLib[title.Key] {
			continue
		}
		ok, err := s.matchesUserQuery(ctx, v, title, userQuery)
		if err != nil {
			return core.Page{}, err
		}
		if ok {
			filtered = append(filtered, title)
		}
	}
	if err := s.sortTitles(ctx, filtered, q); err != nil {
		return core.Page{}, err
	}
	if q.AdjacentTo != 0 {
		return adjacentTitles(filtered, q)
	}
	if q.StartAt != 0 {
		return startingTitles(filtered, q)
	}
	total := len(filtered)
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	return core.Page{Items: filtered[start:end], Total: total}, nil
}

// collectionHasVisibleMember reads bounded pages and stops at the first member
// the viewer can see, without calculating a child count.
func (s *Service) collectionHasVisibleMember(ctx context.Context, v *core.User, c core.Collection) (bool, error) {
	for offset := 0; ; {
		page, err := s.d.Collections.Items(ctx, v, c.Key, offset, 32)
		if err != nil {
			return false, err
		}
		var batch []core.Title
		if c.Upcoming {
			batch = s.d.Visibility.UpcomingItems(ctx, v, page.Items, time.Now)
		} else {
			batch, err = s.allowTitles(ctx, v, page.Items)
			if err != nil {
				return false, err
			}
		}
		if len(batch) > 0 {
			return true, nil
		}
		offset += len(page.Items)
		if offset >= page.Total {
			return false, nil
		}
		if len(page.Items) == 0 {
			return false, errors.New("experience: incomplete collection page")
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
}

func (s *Service) notInterested(ctx context.Context, v *core.User) ([]frontend.NotInterestedEntry, error) {
	if s.d.NotInterestedSource == nil {
		return nil, core.ErrNotAvailable
	}
	entries, err := s.d.NotInterestedSource(ctx, v)
	if err != nil {
		return nil, err
	}
	result := make([]frontend.NotInterestedEntry, 0, len(entries))
	for _, entry := range entries {
		if _, err := s.item(ctx, v, entry.Key); err == nil {
			result = append(result, entry)
		}
	}
	return result, nil
}
func (s *Service) NotInterested(ctx context.Context, viewer *core.Viewer, _ core.Device, q frontend.PageQuery) (frontend.NotInterestedPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.NotInterestedPage{}, err
	}
	entries, err := s.notInterested(ctx, v)
	if err != nil {
		return frontend.NotInterestedPage{}, err
	}
	start, end := sliceBounds(len(entries), q.Offset, q.Limit)
	return frontend.NotInterestedPage{Items: entries[start:end], Total: len(entries)}, nil
}
func (s *Service) NotInterestedItem(ctx context.Context, viewer *core.Viewer, _ core.Device, key core.RatingKey) (frontend.NotInterestedEntry, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.NotInterestedEntry{}, err
	}
	if _, err = s.item(ctx, v, key); err != nil {
		return frontend.NotInterestedEntry{}, core.ErrNotFound
	}
	entries, err := s.notInterested(ctx, v)
	if err != nil {
		return frontend.NotInterestedEntry{}, err
	}
	for _, entry := range entries {
		if entry.Key == key {
			return entry, nil
		}
	}
	return frontend.NotInterestedEntry{}, core.ErrNotFound
}
