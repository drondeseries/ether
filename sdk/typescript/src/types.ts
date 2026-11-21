/**
 * Ether Plugin Protocol v1 (EPP/1) types for @ether/plugin-sdk.
 * Mirrors internal/plugins/contract.go and internal/viewspec.
 */

export type MaybePromise<T> = T | Promise<T>;

export type PluginKind =
  | "source"
  | "integration"
  | "notification_channel"
  | "livetv_source"
  | "extension"
  | "content";

export type Capability =
  | "stream_source"
  | "debrid_provider"
  | "list_source"
  | "livetv_channels"
  | "subtitles"
  | "markers"
  | "notify"
  | "events"
  | "flow_actions"
  | "ranking_step"
  | "rule_condition"
  | "tools"
  | "ui"
  | "catalog_provider"
  | "kind_resolver"
  | "frontend_adapter"
  | "matcher"
  | "poster_badges";

export type LegacyEtherEventName =
  | "playback.started"
  | "playback.paused"
  | "playback.resumed"
  | "playback.stopped"
  | "playback.completed"
  | "playback.error"
  | "playback.failover"
  | "title.available"
  | "viewer.added"
  | "viewer.access_changed"
  | "collection.rebuilt"
  | "server.health_changed";

export type EtherEventName = LegacyEtherEventName | (string & {});

export type EventFieldType =
  | "string"
  | "int"
  | "float"
  | "bool"
  | "duration_ms"
  | "timestamp"
  | "enum"
  | "string_list"
  | "object";

export interface EventEnumOption {
  value: string;
  label?: string;
}

export interface EventFieldMeta {
  name: string;
  type: EventFieldType;
  label: string;
  description?: string;
  unit?: string;
  enum?: EventEnumOption[];
  pii?: boolean;
  secret?: boolean;
  filterable?: boolean;
  example?: unknown;
}

export interface PublishedEventMeta {
  key: string;
  label: string;
  description?: string;
  version?: number;
  fields?: EventFieldMeta[];
}

export interface FlowActionParamMeta {
  key: string;
  type: EventFieldType;
  label: string;
  description?: string;
  required?: boolean;
  secret?: boolean;
  template?: boolean;
  default?: unknown;
  enum?: EventEnumOption[];
  unit?: string;
  picker?: "channel" | "viewer" | "library" | "source" | "event" | "plugin";
}

export interface FlowActionMeta {
  key: string;
  label: string;
  description?: string;
  icon?: string;
  route: string;
  idempotent?: boolean;
  sideEffect?: boolean;
  timeout?: string;
  params?: FlowActionParamMeta[];
  outputs?: EventFieldMeta[];
}

export interface EventCorrelation {
  key?: string;
  sessionId?: string;
  playId?: string;
  titleId?: string;
  viewerId?: number;
  channelId?: string;
  sourceId?: string;
  pluginId?: string;
  deviceId?: string;
  libraryId?: string;
}

export interface FlowActionRequest {
  key: string;
  flowId: string;
  runId: string;
  nodeId: string;
  idempotencyKey: string;
  dryRun?: boolean;
  params: Record<string, unknown>;
}

export interface FlowActionResponse {
  output?: Record<string, unknown>;
  branch?: string;
}

export interface StremioCatalogDecl {
  type: string;
  id: string;
  name?: string;
  extra?: Array<{
    name: string;
    isRequired?: boolean;
    options?: string[];
    optionsLimit?: number;
  }>;
}

export interface StremioResourceDecl {
  name: string;
  types?: string[];
  idPrefixes?: string[];
}

export interface PluginAuthor {
  name?: string;
  url?: string;
}

/** Hosted/data requests are clamped to the owner's global or per-plugin ceilings. */
export interface RuntimeLimits {
  fetchBytes?: number;
  archiveMemberBytes?: number;
  archiveExpandedBytes?: number;
  archiveMembers?: number;
  maxItems?: number;
  memoryBytes?: number;
  readBytes?: number;
  deadline?: string;
  unitBudget?: number;
  poolSize?: number;
}
export interface RuntimeDecl {
  kind: "hosted" | "remote";
  entry?: string;
  module?: string;
  limits?: RuntimeLimits;
}

export interface HealthDecl {
  interval?: string;
}

export interface ScheduleEntry {
  every: string;
  call: string;
}

export interface ViewerPermissions {
  identity?: "none" | "granted_viewers" | "all";
  watch_history?: "none" | "granted_viewers" | "all";
  write_watch_state?: "none" | "granted_viewers";
}

export interface Permissions {
  network?: string[];
  catalog?: "none" | "read";
  viewers?: ViewerPermissions;
  collections?: "none" | "write";
  folders?: "none" | "read";
  secrets?: string[];
  /** Lets a kind resolver ask Ether to find bytes: search to look, acquire to open. */
  sources?: "none" | "search" | "acquire";
}

export type SettingFieldKind =
  | "group"
  | "text"
  | "secret"
  | "duration"
  | "readonly"
  | "number"
  | "bool"
  | "select"
  | "list"
  | "bytes"
  | "hidden"
  | "multiSelect"
  | "orderedList"
  | "language"
  | "viewer"
  | "collection"
  | "folder"
  | "url"
  | "title";

export interface FieldOption {
  value: string;
  label: string;
  hint?: string;
}

export interface SettingField {
  key: string;
  kind: SettingFieldKind;
  label?: string;
  hint?: string;
  required?: boolean;
  default?: unknown;
  options?: FieldOption[];
  fields?: SettingField[];
  min?: number;
  max?: number;
  placeholder?: string;
  /** Search /ether/options/{key}?q=...; selected value persists verbatim. */
  search?: boolean;
}

export interface SettingsSchema {
  $ref?: string;
  fields?: SettingField[];
  groups?: Array<{
    key?: string;
    label?: string;
    hint?: string;
    advanced?: boolean;
    fields: SettingField[];
  }>;
}

export interface SourceMeta {
  class: "debrid" | "usenet" | "indexer" | "direct";
  abilities?: string[];
  limits?: Record<string, unknown>;
}

export interface ChannelsMeta {
  refresh?: string;
  test: true;
}

export interface SubtitlesMeta {
  languages?: "any" | string[];
  formats?: string[];
}

export type MarkerKind = "intro" | "credits" | "recap" | "preview";

export interface MarkersMeta {
  kinds: MarkerKind[];
}

export interface NotificationMeta {
  fields?: SettingField[] | SettingsSchema;
  sendTest: true;
  formats?: string[];
  maxLength?: number;
}

export interface EventsMeta {
  subscribe?: EtherEventName[];
  publish?: PublishedEventMeta[];
}

export interface IntegrationMeta {
  category: string;
  logo?: string;
  adds: string[];
  connect: "fields" | "device_code" | "none";
  test?: boolean;
}

export interface ListSourceMeta {
  key: string;
  label: string;
  hint?: string;
  catalog: string;
  params?: unknown;
  maxItems?: number;
  refresh?: string;
}

export interface RankingStepMeta {
  key: string;
  label: string;
  hint?: string;
  editor?: unknown;
  budget?: unknown;
}

export interface RuleConditionMeta {
  key: string;
  label: string;
  operators?: string[];
  editor?: unknown;
}

export interface PosterBadgeSetMeta {
  key: string;
  label: string;
  description?: string;
}

export interface BadgeTitleRef {
  type: "movie" | "show";
  tmdb?: number;
  imdb?: string;
}

export interface BadgeEntry {
  title: BadgeTitleRef;
  labels: string[];
}

export interface BadgeUsage {
  titles: number;
  quotaTitles: number;
}

export interface ToolMeta {
  name: string;
  title: string;
  description: string;
  inputSchema: Record<string, unknown>;
  readOnly?: boolean;
  destructive?: boolean;
  openWorld?: boolean;
}

export interface PageMeta {
  key: string;
  label: string;
  route: string;
  group?: string;
  icon?: string;
}

export type SlotKind =
  | "page"
  | "title.panel"
  | "viewer.tab"
  | "dashboard.panel"
  | "actionMenu.title"
  | "actionMenu.viewer"
  | "actionMenu.collection"
  | "actionMenu.release"
  | "command";

type ActionSlot =
  | "actionMenu.title"
  | "actionMenu.viewer"
  | "actionMenu.collection"
  | "actionMenu.release";
export type SlotMeta = { key: string; label: string } & (
  | { slot: ActionSlot; action: string; route?: never }
  | { slot: Exclude<SlotKind, ActionSlot>; route: string; action?: never }
);

export interface CommandMeta {
  key: string;
  label: string;
  action: string;
}

export interface Contributes {
  source?: SourceMeta;
  channels?: ChannelsMeta;
  subtitles?: SubtitlesMeta;
  markers?: MarkersMeta;
  notification?: NotificationMeta;
  events?: EventsMeta;
  flowActions?: FlowActionMeta[];
  integration?: IntegrationMeta;
  listSources?: ListSourceMeta[];
  rankingSteps?: RankingStepMeta[];
  ruleConditions?: RuleConditionMeta[];
  posterBadges?: PosterBadgeSetMeta[];
  tools?: ToolMeta[];
  pages?: PageMeta[];
  slots?: SlotMeta[];
  commands?: CommandMeta[];
  mediaKinds?: MediaKindMeta[];
  frontendAdapters?: FrontendAdapterMeta[];
  setup?: SetupMeta;
}

/** A Jellyfin/Emby item type a level can present as. */
export type MediaBrowserItemType =
  | "Movie" | "Series" | "Season" | "Episode"
  | "MusicArtist" | "MusicAlbum" | "Audio" | "AudioBook"
  | "Book" | "PhotoAlbum" | "Photo";

/** How a library is published per protocol (Ether v0.3.8 content kinds). */
export interface Presentations {
  adapterOnly?: boolean;
  vela?: { type: "movies" | "shows" };
  plex?: { base: "movie" | "show" | "artist" | "photo" };
  /**
   * Jellyfin/Emby. `itemTypes` maps each level key to its item type; the
   * declared type drives the item's Type, MediaType (Audio/Book/Photo/
   * Video), IsFolder and IncludeItemTypes filtering. Unmapped levels keep
   * the video projection, so declare every level of a music/book kind.
   */
  mediaBrowser?: { collectionType: string; itemTypes?: Record<string, MediaBrowserItemType> };
  stremio?: { type: "movie" | "series" | "channel" | "tv" };
  xtream?: { category: string };
  silo?: { type: string };
}

export interface KindLevel {
  key: string;
  label: string;
  playable?: boolean;
  container?: boolean;
}

export interface KindField {
  key: string;
  type: "string" | "int" | "float" | "bool" | "date" | "duration" | "enum" | "strings" | "people";
  label: string;
  indexed?: boolean;
  enum?: string[];
}

export interface KindArt {
  role: string;
  label: string;
}

export interface KindLibraryTabRule {
  field: string;
  op: string;
  value?: unknown;
}

export interface KindLibraryTabFilter {
  match?: "all" | "any";
  rules?: KindLibraryTabRule[];
  exclude?: KindLibraryTabRule[];
}

export interface KindLibraryTab {
  key: string;
  label: string;
  kind: "recommended" | "library" | "collections" | "people" | "view" | "live";
  /** Declared browse.facets entry (optional on collections; required people-type facet on people). */
  facet?: string;
  /** Bucket/person ordering: "count" (default on collections) or "name" (case-insensitive alphabetical, default on people). */
  order?: "count" | "name";
  /** Card aspect ratio on "live" tabs (default "landscape"). */
  card?: "poster" | "landscape" | "square";
  /** Presentation layout on "view" tabs: "grid" (default), "list", or "timeline". */
  layout?: "grid" | "list" | "timeline";
  /** Enables in-tab search on "collections", "people", "view", and "live" tabs (default true). */
  search?: boolean;
  /** Optional localizable search input placeholder (at most 60 characters; "$t:" supported) when search is enabled. */
  searchPlaceholder?: string;
  sort?: "title" | "added" | "released" | "year" | "rating" | "index" | "lastViewed";
  direction?: "asc" | "desc";
  /** Saved filter rules on "view" tabs (indexed fields and/or viewer-state rules). */
  filters?: KindLibraryTabFilter;
}

export interface KindLibraryShelf {
  /** Unique shelf key in this library, matching ^[a-z][a-z0-9-]{0,39}$. Stable row ID: "plugin:<viewID>:shelf:<key>". */
  key: string;
  /** Localizable shelf title (literal or "$t:<key>", at most 60 characters). */
  label: string;
  /** Optional sort field (defaults to "released"). */
  sort?: "title" | "added" | "released" | "year" | "rating" | "index";
  /** Sort direction (defaults to "desc"). */
  direction?: "asc" | "desc";
  /** Single-facet filter shorthand; must name a declared and indexed field (or year/released/rating). */
  facet?: string;
  /** Single value for `facet`. */
  value?: string;
  /** OR values for `facet`. */
  values?: string[];
  /** Multi-facet filter (AND across facets, OR within each facet); cannot be combined with `facet`. */
  filters?: Record<string, string[]>;
}

export interface KindLibraryDecl {
  name: string;
  icon?: string;
  card?: "poster" | "landscape" | "square";
  /** How item release dates are rendered on cards and detail headers: "year" (default) or "date" (YYYY-MM-DD). */
  dateDisplay?: "year" | "date";
  /** Up to 50 sub-tabs shown inside the library; defaults to Recommended + Library. */
  tabs?: KindLibraryTab[];
  /** Up to 40 default Recommended shelves evaluated in order when present. */
  shelves?: KindLibraryShelf[];
}

/** On-demand background expansion settings for declared browse facets. */
export interface KindBrowseExpand {
  facets: string[];
  /** Freshness TTL in hours before an expanded slice may be refreshed (default 168). */
  ttlHours?: number;
}

/** One declared content kind; each kind becomes its own library. */
export interface MediaKindMeta {
  key: string;
  label: string;
  library: KindLibraryDecl;
  levels: KindLevel[];
  fields?: KindField[];
  art?: KindArt[];
  present: Presentations;
  browse: {
    sorts?: string[];
    facets?: string[];
    search: boolean;
    searchFields?: string[];
    /** Client searches also call /search live; results are upserted and merged. */
    liveSearch?: boolean;
    /** On-demand background slice expansion when a viewer opens a bucket or person. */
    expand?: KindBrowseExpand;
  };
  sync: { mode: "pull" | "push"; refresh?: string };
  cache?: { maxBytes?: number; ttl?: string };
  match?: { thresholdDefault?: number };
}

export interface FrontendAdapterMeta {
  key: string;
  label: string;
  protocolHint: "opds" | "subsonic" | "custom";
  pathPrefix: string;
  defaultPort?: number;
  clients?: string[];
  auth?: "ether_token" | "basic" | "none_lan";
}

export interface SetupStep {
  key: string;
  label: string;
  hint?: string;
  fields: string[];
  test: string;
  required: boolean;
}

export interface SetupMeta {
  steps: SetupStep[];
}

/** The standardised plugin item envelope. */
export interface KindItem {
  unavailable?: { code: string; detail?: string };
  id: string;
  level: string;
  parentId?: string;
  index?: number;
  title: string;
  sortTitle?: string;
  summary?: string;
  year?: number;
  released?: number;
  rating?: number;
  playable?: boolean;
  childCount?: number;
  leafCount?: number;
  art?: Array<{ role: string; url: string; width?: number; height?: number }>;
  fields?: Record<string, unknown>;
  hints?: Record<string, unknown>;
  etag?: string;
}

export interface KindItemPart {
  index?: number;
  name?: string;
  mime?: string;
  size?: number;
  durationSec?: number;
}

export interface KindPage {
  items: KindItem[];
  nextCursor?: string;
  total?: number;
}

export interface KindBrowseRequest {
  kind: string;
  parentId?: string;
  level?: string;
  cursor?: string;
  limit?: number;
  sort?: string;
  desc?: boolean;
  /** Any of the listed values per field (OR within a field, AND across fields). */
  facets?: Record<string, string[]>;
  /** Every listed value per field must be present. */
  allFacets?: Record<string, string[]>;
  /** Items having any listed value per field are dropped. */
  excludeFacets?: Record<string, string[]>;
  query?: string;
}

/** Query passed to kinds.browse from a frontend adapter (#615). */
export type KindBrowseQuery = Omit<KindBrowseRequest, "kind">;

export interface KindExpandRequest {
  kind: string;
  facet: string;
  value: string;
  valueId?: number;
  cursor?: string;
}

export interface KindExpandResponse {
  items?: KindItem[];
  cursor?: string;
  nextCursor?: string;
  job?: string;
}

export interface KindChangePage {
  upserts?: KindItem[];
  deletes?: string[];
  nextCursor?: string;
  complete: boolean;
}

export interface SourceQuery {
  kind: "indexer_search" | "debrid_check" | "debrid_acquire" | "release_files";
  query?: string;
  /** Comma-separated Newznab category IDs or standard family names; with empty query, fetches recent releases. */
  category?: string;
  /** Quality profile name to score video release names against on indexer_search; empty uses default. */
  profile?: string;
  infoHashes?: string[];
  infoHash?: string;
  magnet?: string;
  /** A candidate from indexer_search handed back for release_files, so an NZB is fetched through its indexer. */
  candidate?: SourceCandidate;
}

/** One file inside a candidate release, from a release_files query. Index is the order Ether's file selection counts. */
export interface ReleaseFile {
  index: number;
  path: string;
  size: number;
}

export interface SourceCandidate {
  infoHash?: string;
  magnet?: string;
  nzbUrl?: string;
  /** Zero-based index in the provider torrent file list. Omit for normal video selection. */
  fileIndex?: number;
  title?: string;
  size?: number;
  seeders?: number;
  cached?: boolean;
  indexer?: string;
  indexerId?: string;
  /** Quality-profile score and parsed video facts; omitted on non-video candidates. */
  score?: number;
  resolution?: number;
  source?: string;
  codec?: string;
  /** Files inside this release; populated only by a release_files query. */
  files?: ReleaseFile[];
}

export interface KindMatchRequest {
  kind: string;
  item: KindItem;
  candidates: SourceCandidate[];
  settings?: Record<string, unknown>;
}

export interface KindMatchScore {
  candidateRef: string;
  score: number;
  reasons?: string[];
}

export interface PlayableSource {
  path?: string;
  id: string;
  label?: string;
  pattern: "range" | "download" | "direct" | "container" | "release" | "file";
  mime: string;
  size?: number;
  durationSec?: number;
  url?: string;
  headers?: Record<string, string>;
  blobRef?: string;
  sha256?: string;
  /** Hex digests checked against the served bytes (including a release file or container member); size is checked too. */
  sha1?: string;
  crc32?: string;
  maxBytes?: number;
  expiresAt?: number;
  container?: {
    format?: string;
    volumes: Array<{ url?: string; headers?: Record<string, string>; blobRef?: string; size: number }>;
    member?: string;
    memberIndex?: number;
    passwordSecret?: string;
  };
  release?: {
    nzbUrl?: string;
    magnet?: string;
    infoHash?: string;
    candidate?: SourceCandidate;
    /** Zero-based torrent file index; overrides candidate.fileIndex. Invalid selections fail. */
    fileIndex?: number;
    /** Exact torrent path or unique basename, including non-video files. */
    file?: string;
    /** Archive member name when composing a release with container extraction. */
    fileHint?: string;
    passwordSecret?: string;
  };
}

export interface KindResolveRequest {
  kind: string;
  libraryKey: string;
  item: KindItem;
  partIndex?: number;
  client?: Record<string, unknown>;
}

export interface PendingReply { pending: true; retryInMs?: number; job?: string; sources?: never; matches?: never }

export interface KindResolveResponse {
  pending?: boolean;
  retryInMs?: number;
  job?: string;
  sources: PlayableSource[];
  ttlSec?: number;
}

export interface KindLiveRequest {
  kind: string;
  tab?: string;
  q?: string;
  filter?: string;
  cursor?: string;
  limit?: number;
}

export interface KindLiveItem {
  id: string;
  title: string;
  subtitle?: string;
  badge?: string;
  viewers?: number;
  tags?: string[];
  thumb?: string;
  live?: boolean;
}

export interface KindLiveFilter {
  key: string;
  label: string;
}

export interface KindLivePage {
  items: KindLiveItem[];
  filters?: KindLiveFilter[];
  nextCursor?: string;
}

export interface KindLiveResolveRequest {
  kind: string;
  id: string;
}

export interface KindLiveResolveResponse {
  url: string;
  headers?: Record<string, string>;
  mime?: string;
  title?: string;
}

export interface SetupTestRequest {
  step: string;
  fields?: Record<string, unknown>;
}

export interface SetupTestResult {
  ok: boolean;
  detail?: string;
  fields?: Record<string, string>;
}

export interface EtherManifestBlock {
  protocol: 1;
  kind: PluginKind;
  capabilities?: Capability[];
  summary: string;
  author?: PluginAuthor;
  license?: string;
  repo?: string;
  icon?: string;
  runtime: RuntimeDecl;
  cluster?: "leader" | "every_node";
  health?: HealthDecl;
  permissions?: Permissions;
  settings?: SettingsSchema | Record<string, unknown>;
  strings?: Record<string, string | Record<string, string>>;
  schedule?: ScheduleEntry[];
  contributes?: Contributes;
}

export interface Manifest {
  id: string;
  version: string;
  name: string;
  description?: string;
  logo?: string;
  background?: string;
  types?: string[];
  catalogs?: StremioCatalogDecl[];
  resources?: Array<string | StremioResourceDecl>;
  idPrefixes?: string[];
  behaviorHints?: {
    adult?: boolean;
    p2p?: boolean;
    configurable?: boolean;
    configurationRequired?: boolean;
  };
  ether?: EtherManifestBlock;
}

// --- Route request and response payloads ---

export interface CatalogItem {
  id: string;
  type: string;
  name: string;
  poster?: string;
  posterShape?: "poster" | "landscape" | "square";
  background?: string;
  logo?: string;
  description?: string;
  releaseInfo?: string;
  imdbRating?: string;
  genres?: string[];
}

export interface CatalogRequest {
  type: string;
  id: string;
  extra: Record<string, string>;
  skip: number;
  search?: string;
  genre?: string;
}

export interface CatalogResponse {
  metas: CatalogItem[];
}

export interface MetaVideo {
  id: string;
  title: string;
  released?: string;
  season?: number;
  episode?: number;
  overview?: string;
  thumbnail?: string;
}

export interface MetaItem extends CatalogItem {
  runtime?: string;
  cast?: string[];
  director?: string[];
  videos?: MetaVideo[];
}

export interface MetaRequest {
  type: string;
  id: string;
}

export interface MetaResponse {
  meta: MetaItem;
}

export interface StreamEtherExt {
  id?: string;
  releaseName?: string;
  size?: number;
  languages?: string[];
  lazy?: boolean;
  file?: string;
}

export interface StreamItem {
  name?: string;
  title?: string;
  description?: string;
  url?: string;
  infoHash?: string;
  fileIdx?: number;
  ytId?: string;
  externalUrl?: string;
  behaviorHints?: {
    notWebReady?: boolean;
    bingeGroup?: string;
    countryWhitelist?: string[];
    filename?: string;
    videoSize?: number;
    videoHash?: string;
    proxyHeaders?: {
      request?: Record<string, string>;
      response?: Record<string, string>;
    };
  };
  ether?: StreamEtherExt;
}

export interface StreamRequest {
  type: string;
  id: string;
  extra: Record<string, string>;
}

export interface StreamResponse {
  streams: StreamItem[];
}

export interface OpenStreamRequest {
  streamId: string;
}

export interface OpenStreamResponse {
  url?: string;
  headers?: Record<string, string>;
  expiresAt?: string;
  file?: string;
}

export interface SubtitleItem {
  id: string;
  url: string;
  lang: string;
  label?: string;
  format?: string;
  hearingImpaired?: boolean;
}

export interface SubtitlesRequest {
  type: string;
  id: string;
  extra: Record<string, string>;
  pickToken?: string;
  videoHash?: string;
  videoSize?: number;
  filename?: string;
}

export interface SubtitlesResponse {
  subtitles: SubtitleItem[];
}

export interface MarkerItem {
  kind: MarkerKind;
  start: number;
  end: number;
  label?: string;
}

export interface MarkersRequest {
  type: string;
  id: string;
}

export interface MarkersResponse {
  markers: MarkerItem[];
}

export interface ChannelInput {
  url: string;
  name?: string;
  headers?: Record<string, string>;
  priority?: number;
}

export interface ChannelItem {
  id: string;
  name: string;
  number?: number;
  group?: string;
  logo?: string;
  guideId?: string;
  inputs: ChannelInput[];
}

export interface ChannelsResponse {
  channels: ChannelItem[];
}

export interface GuideRequest {
  channelId: string;
  from?: string;
  to?: string;
}

export interface ProgrammeItem {
  id?: string;
  channelId?: string;
  title: string;
  subtitle?: string;
  description?: string;
  start: string;
  stop: string;
  icon?: string;
  categories?: string[];
}

export interface GuideResponse {
  programmes: ProgrammeItem[];
}

export interface ConfigureResponse {
  errors?: Record<string, string>;
}

export interface TestResponse {
  ok: boolean;
  message?: string;
  details?: string[];
}

export type HealthStatus = "ok" | "attention" | "error";

export interface HealthResponse {
  status: HealthStatus;
  reason?: string;
}

export interface ViewSpecFieldRef {
  $: string;
}

export type ViewSpecValue = string | ViewSpecFieldRef;

export type ViewSpecBlock =
  | {
      kind: "page-header";
      title: ViewSpecValue;
      description?: ViewSpecValue;
      back?: { href: ViewSpecValue; label: ViewSpecValue };
      actions?: Array<{ widget: string; props?: Record<string, unknown> }>;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "stat-tile";
      iconKey: ViewSpecValue;
      accent: ViewSpecValue;
      label: ViewSpecValue;
      value: ViewSpecValue;
      sub?: ViewSpecValue;
      tone?: "default" | "positive" | "warning" | "negative" | ViewSpecFieldRef;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "panel";
      title: ViewSpecValue;
      iconKey?: ViewSpecValue;
      hint?: ViewSpecValue;
      blocks: ViewSpecBlock[];
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "text";
      content: ViewSpecValue;
      tone?:
        | "default"
        | "negative"
        | "positive"
        | "warning"
        | "muted"
        | "strong"
        | ViewSpecFieldRef;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "heading";
      level: 2 | 3;
      content: ViewSpecValue;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "grid";
      as?: "div" | "section";
      blocks: ViewSpecBlock[];
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "widget";
      widget:
        | "ether.posterGrid"
        | "ether.posterShelf"
        | "ether.titleCard"
        | "ether.liveChart"
        | "ether.settingsForm"
        | "ether.actionButton"
        | "ether.titleMatch"
        | "ether.healthStrip"
        | "ether.channelRow"
        | "ether.badge";
      props?: Record<string, unknown>;
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "frame";
      frame: "ether.section" | "ether.tabs" | "ether.drawer";
      props?: Record<string, unknown>;
      blocks: ViewSpecBlock[];
      when?: ViewSpecFieldRef;
    }
  | {
      kind: "repeat";
      items: ViewSpecFieldRef;
      itemKey: ViewSpecFieldRef;
      unwrapped?: boolean;
      empty?: { text: ViewSpecValue };
      blocks: ViewSpecBlock[];
      when?: ViewSpecFieldRef;
    }
  | Record<string, unknown>;

export interface ViewSpecDocument {
  specVersion: 1;
  layout: "list" | "detail" | "bare";
  route?: string;
  header: ViewSpecBlock[];
  body: ViewSpecBlock[];
}

export interface ViewRequest {
  page: string;
  query: Record<string, string>;
}

export interface ViewResponse {
  spec: ViewSpecDocument;
  data: Record<string, unknown>;
}

export interface ActionProposal {
  category: string;
  description: string;
  input?: Record<string, unknown>;
}

export interface ActionRequest<P = Record<string, unknown>> {
  action: string;
  input: P;
}

export interface ActionResponse {
  toast?: string;
  refresh?: boolean;
  open?: string;
  proposal?: ActionProposal;
}

export interface EtherEvent<P = Record<string, unknown>> {
  id: string;
  name: EtherEventName | string;
  at: string;
  viewerId?: number;
  payload?: P;
}

export interface EventResponse {
  ack: string[];
}

export interface ToolRequest<I = Record<string, unknown>> {
  name: string;
  input: I;
}

export type ToolResponse = Record<string, unknown>;

export interface JobRequest {
  name: string;
  continuation?: unknown;
}

export interface JobResponse {
  /** Presence asks Ether to call again after 0..300000 ms (minimum pause 100ms). */
  continueInMs?: number;
  /** Opaque JSON checkpoint forwarded as the next request's continuation. */
  continuation?: unknown;
  progress?: string;
  done?: boolean;
  failed?: string;
  ok?: boolean;
  [key: string]: unknown;
}

export interface NotifyRequest {
  title?: string;
  message: string;
  event?: string;
  settings?: Record<string, unknown>;
}

export interface NotifyResponse {
  ok: boolean;
  message?: string;
}

// --- Hot-path types (SPEC §5.2 & §5.8) ---

export interface ClientCaps {
  maxHeight?: number;
  hdr?: string[];
  codecs?: string[];
}

export interface CandidateFacts {
  releaseId: string;
  name: string;
  size?: number;
  protocol?: "usenet" | "torrent" | "direct" | string;
  sourceClass?: string;
  indexer?: string;
  resolution?: string | number;
  hdr?: string[];
  videoCodec?: string;
  audioCodecs?: string[];
  audioChannels?: string;
  languages?: string[];
  subtitles?: string[];
  releaseType?: string;
  group?: string;
  cached?: boolean;
  verified?: boolean;
  clientCaps?: ClientCaps;
}

export type RankKeyFn<C = Record<string, unknown>> = (
  candidate: CandidateFacts,
  config: C,
) => number;

export type CondMatchFn<V = unknown> = (
  candidate: CandidateFacts,
  operator: string,
  value: V,
) => boolean;

// --- Low-level ABI request/response ---

export interface RawRequest {
  pluginId?: string;
  method?: string;
  path: string;
  query?: Record<string, string | string[]>;
  headers?: Record<string, string | string[]>;
  body?: unknown;
  /** Exact request bytes, base64-encoded (frontend adapter routes). A non-JSON body arrives only here. */
  bodyBase64?: string;
  contentType?: string;
  locale?: string;
}

/** Host-served cached bytes for an authenticated frontend adapter response. */
export interface FrontendBlob {
  libraryKey: string;
  ref: string;
  memberIndex?: number;
}

export interface RawResponse {
  blob?: FrontendBlob;
  status: number;
  headers?: Record<string, string>;
  body?: unknown;
}

export class PluginHttpError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "PluginHttpError";
    this.status = status;
    this.code = code;
  }
}
