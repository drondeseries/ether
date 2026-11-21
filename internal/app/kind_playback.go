package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// kindAccept lists the retrieval patterns the transcode bridge can turn into
// bytes. "direct" is excluded: a redirect cannot be probed or encoded, so a
// kind source must be a real stream.
var kindAccept = []string{"range", "download", "container", "release"}

// kindSourceFunc is the broker's KindSource dep: it projects a plugin kind
// item and hands back an opener for its bytes (spec §15.1). The opener
// re-authorizes the viewer through KindResolver.OpenKindExcluding, so
// authorization is enforced at open time, not on the projection alone.
func (a *App) kindSourceFunc() func(context.Context, *core.Viewer, core.RatingKey) (*core.KindInput, error) {
	if a.pluginsRT == nil || a.pluginsRT.kindCatalog == nil || a.pluginsRT.kindResolver == nil {
		return nil
	}
	catalog := a.pluginsRT.kindCatalog
	resolver := a.pluginsRT.kindResolver
	return func(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (*core.KindInput, error) {
		if viewer == nil || !key.IsPluginKey() {
			return nil, core.ErrNotFound
		}
		piv, err := catalog.Item(ctx, key)
		if err != nil {
			return nil, core.ErrNotFound
		}
		title := piv.Title
		openCandidate := func(octx context.Context, exclude []string) (core.MediaSource, core.Release, error) {
			media, err := resolver.OpenKindExcluding(octx, viewer, key, kindAccept, exclude)
			if err != nil {
				return nil, core.Release{}, err
			}
			rel := media.Release
			if rel.ID == "" {
				rel.ID = "kind:" + key.String()
			}
			if rel.Size <= 0 {
				rel.Size = media.Size
			}
			if ms := media.MediaSource; ms != nil && ms.Size() == media.Size && (ms.ReleaseID() == "" || ms.ReleaseID() == rel.ID) {
				if ms.ReleaseID() == rel.ID {
					return ms, rel, nil
				}
			}
			return kindMediaSource{media: media, releaseID: rel.ID}, rel, nil
		}
		return &core.KindInput{
			Title:         &title,
			SourceID:      key.String(),
			OpenCandidate: openCandidate,
			Open: func(octx context.Context) (core.MediaSource, error) {
				ms, _, err := openCandidate(octx, nil)
				return ms, err
			},
		}, nil
	}
}

// kindMediaSource adapts a frontend.KindMedia (a byte-addressable plugin
// stream) into a core.MediaSource so the playback session pipeline can probe
// and encode it like any other source.
type kindMediaSource struct {
	media     *frontend.KindMedia
	releaseID string
}

func (k kindMediaSource) ReadAt(p []byte, off int64) (int, error) { return k.media.ReadAt(p, off) }
func (k kindMediaSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if cr, ok := k.media.ReaderAt.(core.ContextReaderAt); ok {
		return cr.ReadAtContext(ctx, p, off)
	}
	return k.media.ReadAt(p, off)
}
func (k kindMediaSource) Size() int64 { return k.media.Size }
func (k kindMediaSource) Name() string {
	if k.media.Name != "" {
		return k.media.Name
	}
	return k.releaseID
}
func (k kindMediaSource) ReleaseID() string { return k.releaseID }
func (k kindMediaSource) Close() error      { return k.media.Close() }
