package app

import (
	"context"
	"errors"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// releaseTitleIDs resolves the release's own name, never the requested title's
// ID. A first-ranked search hit alone is not enough: every result/page counts
// toward ambiguity. Existing catalogue aliases are checked before this lookup.
func (a *App) releaseTitleIDs(ctx context.Context, kind core.MediaType, title string) (core.IDs, error) {
	if a.tmdb == nil {
		return core.IDs{}, errors.New("metadata unavailable")
	}
	if strings.TrimSpace(title) == "" {
		return core.IDs{}, nil
	}
	switch kind {
	case core.TypeMovie:
		found, err := a.tmdb.SearchMovies(ctx, title, 1)
		if err != nil {
			return core.IDs{}, err
		}
		if found.TotalResults != 1 || found.TotalPages != 1 || len(found.Results) != 1 || found.Results[0].ID <= 0 {
			return core.IDs{}, nil
		}
		id := found.Results[0].ID
		detail, err := a.tmdb.MovieDetails(ctx, id)
		if err != nil {
			return core.IDs{}, err
		}
		if detail.ID != id {
			return core.IDs{}, errors.New("metadata detail identifier mismatch")
		}
		ids := core.IDs{TMDB: id, IMDB: detail.IMDBID}
		if detail.ExternalIDs != nil {
			if ids.IMDB == "" {
				ids.IMDB = detail.ExternalIDs.IMDBID
			}
			ids.TVDB = detail.ExternalIDs.TVDBID
		}
		return ids, nil
	case core.TypeShow:
		found, err := a.tmdb.SearchTV(ctx, title, 1)
		if err != nil {
			return core.IDs{}, err
		}
		if found.TotalResults != 1 || found.TotalPages != 1 || len(found.Results) != 1 || found.Results[0].ID <= 0 {
			return core.IDs{}, nil
		}
		id := found.Results[0].ID
		detail, err := a.tmdb.TVDetails(ctx, id)
		if err != nil {
			return core.IDs{}, err
		}
		if detail.ID != id {
			return core.IDs{}, errors.New("metadata detail identifier mismatch")
		}
		ids := core.IDs{TMDB: id}
		if detail.ExternalIDs != nil {
			ids.IMDB, ids.TVDB = detail.ExternalIDs.IMDBID, detail.ExternalIDs.TVDBID
		}
		return ids, nil
	default:
		return core.IDs{}, nil
	}
}
