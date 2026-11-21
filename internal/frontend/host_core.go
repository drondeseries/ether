package frontend

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Core host ports and values stay owned by core. Protocol adapters reach
// them through these aliases instead of importing the domain package.

// Host service ports.
type Auth = core.Auth
type Catalog = core.Catalog
type Collections = core.Collections
type DownloadRemuxer = core.DownloadRemuxer
type HomeLayoutSource = core.HomeLayoutSource
type HubItemSource = core.HubItemSource
type ImpressionSink = core.ImpressionSink
type NeutralHomeLayoutSource = core.NeutralHomeLayoutSource
type Playback = core.Playback
type PlaybackPrepareWithin = core.PlaybackPrepareWithin
type Playlists = core.Playlists

type Recommender = core.Recommender
type State = core.State
type Transcoder = core.Transcoder
type TrickplayProvider = core.TrickplayProvider
type VariantResolver = core.VariantResolver

// Viewer access policy ports.
type BitrateAccessPolicy = core.BitrateAccessPolicy
type DownloadAccessPolicy = core.DownloadAccessPolicy

// Playback, session and request values.
type AudioLimit = core.AudioLimit
type ClientCapabilityOverride = core.ClientCapabilityOverride
type DASHSession = core.DASHSession
type EncoderStatus = core.EncoderStatus
type HLSLadderSession = core.HLSLadderSession
type HLSRequest = core.HLSRequest
type HLSSession = core.HLSSession
type HTTPSession = core.HTTPSession
type ImageURLFunc = core.ImageURLFunc
type NeutralHomeRow = core.NeutralHomeRow
type PersonCredit = core.PersonCredit
type SearchCandidates = core.SearchCandidates
type UncachedError = core.UncachedError

// Playback protocols and media kinds.
const (
	ProtocolDASH    = core.ProtocolDASH
	ProtocolHLS     = core.ProtocolHLS
	ProtocolTorrent = core.ProtocolTorrent
)

const (
	MediaAny   = core.MediaAny
	MediaMovie = core.MediaMovie
	MediaTV    = core.MediaTV
)

// Prewarm reasons.
const (
	PrewarmBrowse  = core.PrewarmBrowse
	PrewarmTrailer = core.PrewarmTrailer
)

// Release-source states.
const (
	UsenetFailed = core.UsenetFailed
	UsenetNone   = core.UsenetNone
)

// Domain errors, shared by identity for errors.Is checks.
var (
	ErrArchiveUnstreamable = core.ErrArchiveUnstreamable
	ErrArticleCorrupt      = core.ErrArticleCorrupt
	ErrArticleMissing      = core.ErrArticleMissing
	ErrBudgetExhausted     = core.ErrBudgetExhausted
	ErrDebridAuth          = core.ErrDebridAuth
	ErrDownloadLimit       = core.ErrDownloadLimit
	ErrDownloadPaused      = core.ErrDownloadPaused
	ErrDownloadQueued      = core.ErrDownloadQueued
	ErrDownloadRefused     = core.ErrDownloadRefused
	ErrNoSources           = core.ErrNoSources
	ErrNoTorrentSources    = core.ErrNoTorrentSources
	ErrNotCached           = core.ErrNotCached
	ErrPolicyUnconfirmed   = core.ErrPolicyUnconfirmed
	ErrPrepareSlow         = core.ErrPrepareSlow
	ErrPreparing           = core.ErrPreparing
	ErrVersionUnavailable  = core.ErrVersionUnavailable
	ErrJobsBusy            = core.ErrJobsBusy
	ErrReleaseRejected     = core.ErrReleaseRejected
	ErrSourceUnavailable   = core.ErrSourceUnavailable
	ErrSourceChanged       = core.ErrSourceChanged
	ErrSeekUnavailable     = core.ErrSeekUnavailable
	ErrUnreleased          = core.ErrUnreleased
)

// PreparingError carries a suggested retry delay for ErrPreparing.
type PreparingError = core.PreparingError

// PreparingRetrySeconds returns the HTTP Retry-After value in whole seconds
// for an ErrPreparing error.
func PreparingRetrySeconds(err error, fallback int) int {
	return core.PreparingRetrySeconds(err, fallback)
}

// JobsBusyError carries the refusal message and retry delay for ErrJobsBusy.
type JobsBusyError = core.JobsBusyError

// JobsBusyRetrySeconds returns the HTTP Retry-After value in whole seconds
// for an ErrJobsBusy error.
func JobsBusyRetrySeconds(err error, fallback int) int {
	return core.JobsBusyRetrySeconds(err, fallback)
}

// JobsBusyMessage returns the viewer-facing message for an ErrJobsBusy error.
func JobsBusyMessage(err error) string {
	return core.JobsBusyMessage(err)
}

// JobsBusyErrorBody returns the JSON media error envelope for jobs_busy.
func JobsBusyErrorBody(err error) []byte {
	return core.JobsBusyErrorBody(err)
}

// FilterOutSurroundAudio returns codecs without surround audio formats.
func FilterOutSurroundAudio(codecs []string) []string {
	return core.FilterOutSurroundAudio(codecs)
}

// NewHubContext derives the request context for a hubs call.
func NewHubContext(now time.Time, product, platform, device string) HubContext {
	return core.NewHubContext(now, product, platform, device)
}

// SectionView restricts a Home hub to one library section's media type.
func SectionView(h Hub, t MediaType) (Hub, bool) { return core.SectionView(h, t) }

// WithBrowseScope identifies a tokenless client's browse scope.
func WithBrowseScope(ctx context.Context, scope string) context.Context {
	return core.WithBrowseScope(ctx, scope)
}

// WithPlexSaferRating marks a request for the safer age-rating mapping.
func WithPlexSaferRating(ctx context.Context) context.Context {
	return core.WithPlexSaferRating(ctx)
}

// WithSearchCandidates attaches the request-scoped search candidate set.
func WithSearchCandidates(ctx context.Context, c *SearchCandidates) context.Context {
	return core.WithSearchCandidates(ctx, c)
}

// WithSearchListFacts marks a search rendered as list items.
func WithSearchListFacts(ctx context.Context) context.Context {
	return core.WithSearchListFacts(ctx)
}

// WithSearchClient attaches the client identifier for search-as-you-type single-flight.
func WithSearchClient(ctx context.Context, clientID string) context.Context {
	return core.WithSearchClient(ctx, clientID)
}

// ErrBedtime carries the next playback admission time.
type ErrBedtime = core.ErrBedtime

// WithLocalResponse requests an answer from local snapshots and caches,
// without searches or source opens.
func WithLocalResponse(ctx context.Context) context.Context {
	return core.WithLocalResponse(ctx)
}

// WithColdDetail identifies the single title page allowed a minimum cold fill.
func WithColdDetail(ctx context.Context, key RatingKey) context.Context {
	return core.WithColdDetail(ctx, key)
}

// DetailPending is the exact-page enrichment snapshot.
type DetailPending = core.DetailPending

func WithDetailPending(ctx context.Context, key RatingKey) (context.Context, *DetailPending) {
	return core.WithDetailPending(ctx, key)
}
