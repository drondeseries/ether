// Package core holds the domain types and the interfaces between Ether's
// components. It has no dependencies on other Ether packages, so every
// component can import it. Changing a type here is a contract change: keep
// edits additive.
package core

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// SetupOwnerID is the reserved source-ownership principal for an admitted LAN
// open-setup request. It is never persisted as a user or accepted as a login.
// Producers must stop using it immediately when any real owner exists.
const SetupOwnerID int64 = 1<<63 - 1

// MediaType matches Plex's metadata "type" attribute.
type MediaType string

const (
	TypeMovie   MediaType = "movie"
	TypeShow    MediaType = "show"
	TypeSeason  MediaType = "season"
	TypeEpisode MediaType = "episode"
)

// IDs are the external identifiers of a title. Episodes carry their show's
// IMDb/TVDB ids in ShowIDs on Title, and their own TMDB episode id here.
type IDs struct {
	TMDB int64  `json:"tmdb,omitempty"`
	IMDB string `json:"imdb,omitempty"`
	TVDB int64  `json:"tvdb,omitempty"`
}

type Person struct {
	TMDB      int64  `json:"tmdb,omitempty"`
	IMDB      string `json:"imdb,omitempty"`
	ID        string `json:"id,omitempty"` // catalogue person id when there is no TMDB id
	Name      string `json:"name"`
	Role      string `json:"role,omitempty"` // character for cast, job for crew
	ThumbPath string `json:"thumbPath,omitempty"`
}

// Title is a catalogue item: movie, show, season or episode. Image paths are
// TMDB paths ("/abc.jpg"); the front end turns them into URLs.
type Title struct {
	Key  RatingKey `json:"key"`
	Type MediaType `json:"type"`
	IDs  IDs       `json:"ids"`

	// Plugin kind projections (spec §6.3, additive). KindLevel is the
	// declared MediaKindMeta level key a projected plugin item came from
	// (empty for catalogue titles); LibraryID is the owning library, set so
	// protocols can type and section a plugin item without a re-lookup.
	KindLevel     string    `json:"kindLevel,omitempty"`
	LibraryID     LibraryID `json:"-"`
	ItemIndex     int       `json:"-"`
	PluginAddedAt int64     `json:"-"`
	LandscapeCard bool      `json:"-"`
	DateDisplay   string    `json:"-"`
	// MBItemType is the MediaBrowser (Jellyfin/Emby) item type the owning
	// kind's presentation declares for KindLevel (MusicAlbum, Audio, Book,
	// AudioBook, …); empty for catalogue titles and for levels the kind
	// does not map, which keep the video projection. ParentKey/ParentTitle
	// name the item's immediate parent in the declared hierarchy (a
	// track's album, an album's artist) where ShowKey names only the root.
	MBItemType  string    `json:"-"`
	ParentKey   RatingKey `json:"-"`
	ParentTitle string    `json:"-"`
	// SelfChild marks the synthetic playable-leaf incarnation of a
	// single-level playable container item (a flat MusicAlbum kind whose
	// album is one playable item): the title is the item itself, presented
	// once as its own child (an Audio track / Plex track, same Key) so
	// clients that only play leaf items can play it.
	SelfChild bool `json:"-"`

	// Seasons and episodes.
	ShowKey           RatingKey `json:"showKey,omitempty"`
	ShowIDs           IDs       `json:"showIds,omitempty"`
	ShowTitle         string    `json:"showTitle,omitempty"`
	ShowOriginalTitle string    `json:"showOriginalTitle,omitempty"` // canonical original series name, distinct from episode title
	ShowEnglishTitle  string    `json:"-"`                           // canonical en-US series name when catalogue language is non-English; release matching only
	ShowYear          int       `json:"showYear,omitempty"`          // series premiere year, distinct from episode air year
	SeasonTitle       string    `json:"seasonTitle,omitempty"`
	Season            int       `json:"season,omitempty"`
	Episode           int       `json:"episode,omitempty"`
	// AbsoluteEpisode counts an episode from the series start across its
	// regular seasons; 0 when an earlier season's size is unknown (additive).
	AbsoluteEpisode int `json:"absoluteEpisode,omitempty"`

	Title         string `json:"title"`
	OriginalTitle string `json:"originalTitle,omitempty"`
	// EnglishTitle is TMDB's primary en-US title when the catalogue language
	// is non-English; release matching only, never serialized (additive).
	EnglishTitle string `json:"-"`
	SortTitle    string `json:"sortTitle,omitempty"`
	Tagline      string `json:"tagline,omitempty"`
	Summary      string `json:"summary,omitempty"`
	Year         int    `json:"year,omitempty"`
	// PrimaryReleased is TMDB's primary release_date for a movie. The title
	// year comes from it. Released stays the date added (theatrical, when
	// one exists). Empty when the year came from a premiere or the release
	// itself. Not serialized.
	PrimaryReleased time.Time `json:"-"`
	// YearSource records which date produced Year: primary, premiere,
	// released, none, or missing. Empty means a row stored before that
	// date was kept. Not serialized.
	YearSource                  string        `json:"-"`
	Released                    time.Time     `json:"released,omitempty"`   // first air / release date
	AddedAt                     time.Time     `json:"addedAt,omitzero"`     // series/season latest aired episode, or a derived air date; not a catalogue insertion time
	LastAirDate                 time.Time     `json:"lastAirDate,omitzero"` // last aired episode date when known
	HomeReleaseChecked          bool          `json:"homeReleaseChecked,omitempty"`
	HomeReleased                time.Time     `json:"homeReleased,omitempty"`
	HomeReleaseFallbackAdmitted bool          `json:"homeReleaseFallbackAdmitted,omitempty"`
	Runtime                     time.Duration `json:"runtime,omitempty"`
	Status                      string        `json:"status,omitempty"` // TMDB status: Released, In Production, Ended...

	Genres   []string `json:"genres,omitempty"`
	Keywords []string `json:"keywords,omitempty"` // TMDB keywords (lower-case), for content-based recommendations
	// AlternativeTitles are other names the title goes by (TMDB alternative
	// titles: romanized originals, regional and working titles). Release
	// matching only; never serialized (additive). Episodes and seasons carry
	// their series' in ShowAlternativeTitles.
	AlternativeTitles     []string `json:"-"`
	ShowAlternativeTitles []string `json:"-"`
	RemoteTrailerName     string   `json:"remoteTrailerName,omitempty"`
	RemoteTrailerURL      string   `json:"remoteTrailerURL,omitempty"`
	Studios               []string `json:"studios,omitempty"`
	Countries             []string `json:"countries,omitempty"`
	Language              string   `json:"language,omitempty"` // ISO 639-1 original language
	// ContentRating is the certification in contentrating's stored form:
	// bare US labels ("PG", "TV-14"), region-qualified otherwise ("de/6").
	ContentRating string `json:"contentRating,omitempty"`
	// ContentRatingSource records where ContentRating came from: "tmdb:US:6"
	// (TMDB, country, movie release type), "tmdb:CA" (TV), "omdb", "mdblist".
	ContentRatingSource string `json:"contentRatingSource,omitempty"`
	// EnforcementRating maps certifications to the US scale ("G", "PG",
	// "PG-13", "R", "NC-17") or marks uncertified adult/adult-keyword titles
	// ("XXX") for viewer rating enforcement without altering ContentRating.
	EnforcementRating string `json:"-"`
	Adult             bool   `json:"adult,omitempty"`

	Rating     float64 `json:"rating,omitempty"` // TMDB vote average 0-10
	VoteCount  int     `json:"voteCount,omitempty"`
	Popularity float64 `json:"popularity,omitempty"`
	// SearchScore is how well the title answers the search that returned it
	// (1 is the exact title; zero outside search results). Frontends order
	// result groups and report relevance from it. Never persisted.
	SearchScore float64 `json:"-"`

	PosterPath   string `json:"posterPath,omitempty"`
	BackdropPath string `json:"backdropPath,omitempty"`
	StillPath    string `json:"stillPath,omitempty"` // episodes
	LogoPath     string `json:"logoPath,omitempty"`
	ThumbPath    string `json:"-"` // plugin item 16:9 thumb role when present

	Cast      []Person `json:"cast,omitempty"`
	Directors []Person `json:"directors,omitempty"`
	Writers   []Person `json:"writers,omitempty"`
	Crew      []Person `json:"crew,omitempty"` // creators, cinematographers, composers (additive)

	CollectionID   int64  `json:"collectionId,omitempty"` // TMDB franchise
	CollectionName string `json:"collectionName,omitempty"`

	ChildCount int `json:"childCount,omitempty"` // seasons of a show, episodes of a season
	LeafCount  int `json:"leafCount,omitempty"`  // episodes of a show

	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// ShowTrajectory summarizes a series' season/episode rating arc (additive).
type ShowTrajectory struct {
	ShowKey       RatingKey `json:"showKey"`
	EarlyRating   float64   `json:"earlyRating,omitempty"`
	FinaleRating  float64   `json:"finaleRating,omitempty"`
	PeakRating    float64   `json:"peakRating,omitempty"`
	RatedSeasons  int       `json:"ratedSeasons,omitempty"`
	SticksLanding bool      `json:"sticksLanding,omitempty"`
}

// IsReleased reports a passed release/air date or a verified movie home release.
// Home evidence does not change the theatrical date used for date added.
func (t *Title) IsReleased(now time.Time) bool {
	return (!t.Released.IsZero() && !t.Released.After(now)) || (t.Type == TypeMovie && t.HomeReleaseChecked && !t.HomeReleased.IsZero() && !t.HomeReleased.After(now))
}

// IsFutureRelease distinguishes a future cinema date from a movie already
// released digitally. Released remains the date used for date-added display.
func (t *Title) IsFutureRelease(now time.Time) bool {
	return !t.Released.IsZero() && t.Released.After(now) && !t.IsReleased(now)
}

var (
	ErrNotFound            = errors.New("not found")
	ErrNotAvailable        = errors.New("not available yet") // no acceptable release exists
	ErrVersionUnavailable  = errors.New("version unavailable")
	ErrPreparing           = errors.New("item is still being prepared")
	ErrJobsBusy            = errors.New("playback preparation jobs are all busy")
	ErrUnreleased          = errors.New("title has not been released")
	ErrArticleMissing      = errors.New("article missing on all providers")
	ErrArticleCorrupt      = errors.New("corrupt article or media data")
	ErrReleaseRejected     = errors.New("release cannot be streamed")
	ErrArchiveUnstreamable = errors.New("this archive format or compression method cannot be streamed")
	ErrSourceUnavailable   = errors.New("media source temporarily unavailable")
	ErrBudgetExhausted     = errors.New("indexer daily budget exhausted")
	ErrStreamLimit         = errors.New("stream limit reached")
	ErrUnauthorized        = errors.New("unauthorized")
	// ErrPolicyUnconfirmed means the durable policy store could not be read.
	// Callers may keep an unrestricted cached decision. Restricted viewers fail closed.
	ErrPolicyUnconfirmed = errors.New("policy store unavailable")
	// ErrSeekUnavailable means a page's seek title no longer matches its
	// filter; the caller pages by offset instead.
	ErrSeekUnavailable = errors.New("seek title unavailable")
	// ErrVideoTranscodingDisabled means delivery requires re-encoding video
	// while transcode.videoTranscoding is set to "off".
	ErrVideoTranscodingDisabled error = videoTranscodingDisabledError{}
)

type videoTranscodingDisabledError struct{}

func (videoTranscodingDisabledError) Error() string {
	return "needs a video transcode, and transcoding is off"
}

func (videoTranscodingDisabledError) Unwrap() error { return ErrNotAvailable }

// PreparingError wraps ErrPreparing with a suggested retry delay (#580).
type PreparingError struct {
	RetryAfter time.Duration
}

func (e *PreparingError) Error() string        { return ErrPreparing.Error() }
func (e *PreparingError) Is(target error) bool { return target == ErrPreparing }

// PreparingFromPending constructs an ErrPreparing carrying a retry delay from
// a plugin's PendingReply.RetryInMS hint, defaulting to 5s when unspecified.
func PreparingFromPending(retryInMS int64) error {
	d := 5 * time.Second
	if retryInMS > 0 {
		d = time.Duration(retryInMS) * time.Millisecond
		if d < time.Second {
			d = time.Second
		} else if d > 60*time.Second {
			d = 60 * time.Second
		}
	}
	return &PreparingError{RetryAfter: d}
}

// PreparingRetrySeconds returns the HTTP Retry-After value in whole seconds
// for err, rounding sub-second durations up and using fallback (default 5)
// when err carries no explicit RetryAfter.
func PreparingRetrySeconds(err error, fallback int) int {
	if fallback <= 0 {
		fallback = 5
	}
	var pe *PreparingError
	if errors.As(err, &pe) && pe != nil && pe.RetryAfter > 0 {
		sec := int((pe.RetryAfter + time.Second - 1) / time.Second)
		if sec < 1 {
			sec = 1
		}
		if sec > 300 {
			sec = 300
		}
		return sec
	}
	return fallback
}

// JobsBusyError wraps ErrJobsBusy with an honest refusal message and retry delay (#590).
type JobsBusyError struct {
	Message    string
	RetryAfter time.Duration
}

func (e *JobsBusyError) Error() string {
	if e != nil && e.Message != "" {
		return e.Message
	}
	return ErrJobsBusy.Error()
}

func (e *JobsBusyError) Is(target error) bool { return target == ErrJobsBusy }

// JobsBusyFromPending constructs an ErrJobsBusy carrying the refusal message and
// a retry delay from PendingReply.RetryInMS, defaulting to 5s when unspecified.
func JobsBusyFromPending(message string, retryInMS int64) error {
	d := 5 * time.Second
	if retryInMS > 0 {
		d = time.Duration(retryInMS) * time.Millisecond
		if d < time.Second {
			d = time.Second
		} else if d > 60*time.Second {
			d = 60 * time.Second
		}
	}
	return &JobsBusyError{Message: message, RetryAfter: d}
}

// JobsBusyRetrySeconds returns the HTTP Retry-After value in whole seconds for
// an ErrJobsBusy error, rounding sub-second durations up and using fallback
// (default 5) when err carries no explicit RetryAfter.
func JobsBusyRetrySeconds(err error, fallback int) int {
	if fallback <= 0 {
		fallback = 5
	}
	var be *JobsBusyError
	if errors.As(err, &be) && be != nil && be.RetryAfter > 0 {
		sec := int((be.RetryAfter + time.Second - 1) / time.Second)
		if sec < 1 {
			sec = 1
		}
		if sec > 300 {
			sec = 300
		}
		return sec
	}
	return fallback
}

// JobsBusyMessage returns the viewer-facing message for an ErrJobsBusy refusal.
func JobsBusyMessage(err error) string {
	var be *JobsBusyError
	if errors.As(err, &be) && be != nil && be.Message != "" {
		return be.Message
	}
	return "Four playback preparation jobs are already running; wait for one to finish."
}

// JobsBusyErrorBody encodes the standard JSON media error envelope for jobs_busy.
func JobsBusyErrorBody(err error) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"code":    "jobs_busy",
			"message": JobsBusyMessage(err),
		},
	})
	return b
}

// User is a Plex account with access to this server: the owner or a friend
// the owner shared with on plex.tv.
type User struct {
	// Timestamps are internal identity metadata and do not change existing wire JSON.
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
	// Delegated marks short-lived server credentials; these never grant owner access.
	Delegated bool   `json:"-"`
	ID        int64  `json:"id"` // local id
	PlexID    int64  `json:"plexId"`
	UUID      string `json:"uuid,omitempty"`
	Username  string `json:"username"`
	Title     string `json:"title"` // display name
	Email     string `json:"email,omitempty"`
	Thumb     string `json:"thumb,omitempty"`
	IsOwner   bool   `json:"isOwner"`

	StreamLimit       int         `json:"streamLimit"`       // 0 = server default
	MaxContentRating  string      `json:"maxContentRating"`  // "" = unrestricted, e.g. "PG-13", "TV-14"
	SubtitleLanguages []string    `json:"subtitleLanguages"` // ISO 639-1, preference order
	AudioLanguages    []string    `json:"audioLanguages"`
	AllowedLibraries  []LibraryID `json:"allowedLibraries"` // empty = all libraries (§1.4)
}

// ClientCaps is what a Plex client can play, derived from X-Plex-* headers,
// the client profile and its declared limits.
type ClientCaps struct {
	DVProfilesExplicit bool `json:"dvProfilesExplicit,omitempty"` // preserves an owner-forced empty profile set
	// DVProfilesForced records an owner-forced non-empty Dolby Vision profile list.
	DVProfilesForced bool `json:"-"`
	// ForcedOverrideApplied records a runtime declaration replaced by an owner edit.
	ForcedOverrideApplied bool `json:"-"`
	// Broadcast inputs are selected for the channel rendition, independently
	// of viewer quality preferences. Exclusions last only for this airing.
	BroadcastBitrateKbps  int      `json:"-"`
	BroadcastExclude      []string `json:"-"`
	BroadcastReadyRelease string   `json:"-"`

	// Playback telemetry is supplied by the adapter.
	InferBuffering      bool `json:"inferBuffering,omitempty"`
	NoPlaybackTelemetry bool `json:"noPlaybackTelemetry,omitempty"`

	// BestEffortLiveStart bounds startup waits; playlist requests finish admission.
	BestEffortLiveStart bool `json:"bestEffortLiveStart,omitempty"`
	// ProbeLiveWithoutCodecs requires codec analysis even without declared codecs.
	ProbeLiveWithoutCodecs bool `json:"probeLiveWithoutCodecs,omitempty"`
	// ProbeLiveInput requests source facts before live delivery selection.
	ProbeLiveInput bool `json:"probeLiveInput,omitempty"`

	// PreserveEmptyVideoCodecs prevents registry inference when the adapter owns codec defaults.
	PreserveEmptyVideoCodecs bool `json:"preserveEmptyVideoCodecs,omitempty"`
	// DeferSourceCodecValidation lets delivery selection handle a source codec mismatch.
	DeferSourceCodecValidation bool `json:"deferSourceCodecValidation,omitempty"`
	// AwaitAdmissionIndex briefly joins the generation-owned index before delivery selection.
	AwaitAdmissionIndex bool `json:"awaitAdmissionIndex,omitempty"`

	ClientIP string `json:"clientIp,omitempty"` // trusted-proxy-resolved address, when known

	Profiles        *ClientCapabilityProfiles `json:"profiles,omitempty"`
	Protocol        Protocol                  `json:"protocol,omitempty"`
	Product         string                    `json:"product"`  // X-Plex-Product: "Plex Web", "Plex for iOS", "Plex for Apple TV"
	Platform        string                    `json:"platform"` // X-Plex-Platform
	PlatformVersion string                    `json:"platformVersion"`
	Device          string                    `json:"device"`
	DeviceName      string                    `json:"deviceName"`
	ClientID        string                    `json:"clientId"` // X-Plex-Client-Identifier
	Version         string                    `json:"version"`

	// PreferredSourceHeight softly favors a known HD source for a new quality
	// request. It never constrains eligibility or replaces an active source.
	PreferredSourceHeight int    `json:"preferredSourceHeight,omitempty"`
	MaxHeight             int    `json:"maxHeight"`                   // 2160, 1080, 720...
	ExplicitMaxHeight     bool   `json:"explicitMaxHeight,omitempty"` // true when MaxHeight came from a request header/query limitation rather than a product default
	MaxHeightSource       string `json:"-"`                           // declaration, preset or override that established MaxHeight
	// HDRFormats preserves explicit display range declarations for Vela.
	HDRFormats           []string            `json:"hdrFormats,omitempty"`
	HDR10                bool                `json:"hdr10"`
	ExplicitNoHDR        bool                `json:"explicitNoHdr,omitempty"` // true when HDR was explicitly denied by profile-extra limitations
	DolbyVision          bool                `json:"dolbyVision"`
	VideoCodecs          []string            `json:"videoCodecs"`                    // h264, hevc, av1, vp9
	AudioCodecs          []string            `json:"audioCodecs"`                    // aac, ac3, eac3, truehd, dts, opus, flac
	Containers           []string            `json:"containers"`                     // mp4, mkv, mpegts
	HLSContainers        []string            `json:"hlsContainers,omitempty"`        // explicit segmented output targets; distinct from direct-play containers
	MaxBitrateKbps       int                 `json:"maxBitrateKbps"`                 // 0 = unlimited
	DefaultRemoteBitrate bool                `json:"defaultRemoteBitrate,omitempty"` // true when MaxBitrateKbps came from the server's default remote cap rather than a client limitation
	SubtitleCodecs       []string            `json:"subtitleCodecs"`                 // native image/text subtitle rendering
	DVProfiles           []int               `json:"dvProfiles,omitempty"`           // explicit Dolby Vision profiles; empty uses defaults unless DVProfilesExplicit
	VideoProfiles        map[string][]string `json:"videoProfiles,omitempty"`
	VideoLevels          map[string]int      `json:"videoLevels,omitempty"` // codec level in ffprobe units (H.264 51 = 5.1)
	Remote               bool                `json:"remote"`                // not on the server's LAN

	// AudioLimits are request-declared limits on source audio and encoded output.
	AudioLimits []AudioLimit `json:"audioLimits,omitempty"`

	// TranscodeVideoCodecs and TranscodeAudioCodecs describe ordered output
	// preferences for the current streaming protocol. Source selection and
	// direct play continue to use VideoCodecs and AudioCodecs.
	TranscodeVideoCodecs []string `json:"transcodeVideoCodecs,omitempty"`
	TranscodeAudioCodecs []string `json:"transcodeAudioCodecs,omitempty"`

	// Download marks a request for an offline download (Plex Downloads /
	// Sync) rather than live playback: the resolver prefers compact,
	// mobile-native releases and the whole file is remuxed (additive).
	Download bool `json:"download,omitempty"`

	// DownloadSourceMaxHeight permits a higher source on a download retry;
	// MaxHeight remains the output constraint at the download remuxer.
	DownloadSourceMaxHeight int `json:"downloadSourceMaxHeight,omitempty"`

	// DolbyVisionBaseOnly delivers a Dolby Vision 7.6/8.1 source as its
	// compatible HDR10 base layer: an owner override turned Dolby Vision off
	// for the client, or a Dolby Vision start stalled on this client
	// (additive).
	DolbyVisionBaseOnly bool `json:"dolbyVisionBaseOnly,omitempty"`

	// ConvertSurroundAudio is set by an owner device rule that converts
	// Dolby/DTS/TrueHD audio to AAC. SupportsAudio refuses those codecs even
	// when a later layer (a client direct-play profile) lists them again
	// (additive).
	ConvertSurroundAudio bool `json:"convertSurroundAudio,omitempty"`

	// VideoMaxHeights bounds one decoder, such as a legacy Plex
	// videoDecoders=h264{resolution:1080} declaration. MaxHeight stays the
	// ceiling of the tallest codec the client decodes (additive).
	VideoMaxHeights map[string]int `json:"videoMaxHeights,omitempty"`

	// SegmentedSurroundOff keeps Dolby/DTS/TrueHD audio out of copied
	// segmented output because a start stalled on it for this client.
	// Direct play of the original file is unaffected (additive).
	SegmentedSurroundOff bool `json:"segmentedSurroundOff,omitempty"`
}

// AudioLimit applies to one audio codec, or all codecs when Codec is "*".
type AudioLimit struct {
	Codec, Name, Type string
	Value             int
}

// IsSurroundAudioCodec reports the Dolby and DTS formats (AC-3, E-AC-3,
// TrueHD, DTS) that the surround conversion rule replaces with AAC.
func IsSurroundAudioCodec(c string) bool {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "eac3", "ec3", "e-ac3", "ac3", "dts", "dtshd", "dts-hd", "dtshd_ma", "dca", "dca-ma", "truehd", "mlp":
		return true
	}
	return false
}

// FilterOutSurroundAudio returns codecs without surround audio formats.
func FilterOutSurroundAudio(codecs []string) []string {
	var kept []string
	for _, c := range codecs {
		if !IsSurroundAudioCodec(c) {
			kept = append(kept, c)
		}
	}
	return kept
}

func (c ClientCaps) SupportsVideo(codec string) bool { return contains(c.VideoCodecs, codec) }
func (c ClientCaps) SupportsAudio(codec string) bool {
	if c.ConvertSurroundAudio && IsSurroundAudioCodec(codec) {
		return false
	}
	return contains(c.AudioCodecs, codec)
}
func (c ClientCaps) SupportsContainer(ct string) bool { return contains(c.Containers, ct) }

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
