/**
 * Host service clients for Ether plugins (SPEC §5.1.5, §5.2, §5.3).
 *
 * In hosted mode (QuickJS on wazero), calls go through the synchronous `ether`
 * and `__ether_host` globals provided by internal/plugins/wasm.
 * In sidecar mode (remote HTTP or tunnel), calls go over HTTP to
 * `/ether/plugin-host/*` authenticated with `X-Ether-Plugin-Credential`.
 */

import type { BadgeEntry, BadgeTitleRef, BadgeUsage, EventCorrelation, MaybePromise, RuntimeLimits } from "./types.ts";
import type { ArchiveMember } from "./kit.ts";

export type { BadgeEntry, BadgeTitleRef, BadgeUsage };

export interface HostFetchRequest {
  /** Bounded by the call deadline. Timeout throws an error with code "timeout". */
  timeoutMs?: number;
  method?: string;
  url: string;
  headers?: Record<string, string>;
  body?: string;
  bodyBase64?: string;
}

export interface HostFetchResponse {
  status: number;
  headers: Record<string, string>;
  body: string;
  bodyBase64?: string;
}

export interface HashRequest { url: string; algo: "sha256" | "sha1" | "md5"; expectSize?: number }
export interface HashResult { pending?: boolean; retryInMs?: number; size?: number; sha256?: string; sha1?: string; md5?: string }

export const MinDataReadBytes = 8 * 1024 * 1024;
export const MaxDataReadBytes = 512 * 1024 * 1024;
export const MaxArchiveReadItems = 4096;

export const MaxPosterBadgeSets = 4;
export const MaxBadgeEntriesPerCall = 500;
export const MaxBadgeLabelsPerTitle = 3;
export const MaxBadgeLabelLen = 16;
export const PosterBadgeQuotaTitles = 200_000;
export const MAX_KIND_VISIBLE_IDS = 500;
export const HEADER_VIEWER_TOKEN = "X-Ether-Viewer-Token";
export const HEADER_IDEMPOTENCY_KEY = "X-Ether-Idempotency-Key";

export interface DataRange { offset: number; length: number }
export interface ArchiveReadItem { member: number; offset: number; length: number }
export interface ArchiveReadResult { index: number; member: number; size: number; body: Uint8Array }
export interface FetchCacheRequest {
  /** Bounded by the call deadline. Timeout throws an error with code "timeout". */
  timeoutMs?: number;
  url: string;
  headers?: Record<string, string>;
  libraryKey: string;
  name: string;
}
export interface FetchCacheResult { blob: string; size: number; crc32: string; md5: string; sha1: string; status: number }
export interface ArchiveExtractOptions { libraryKey?: string; name?: string }
export interface ArchiveExtractResult { ref: string; size: number; sha1: string; md5: string; crc32: string }
/** Output size/format for cache.rasterize; a zero dimension keeps the SVG aspect ratio and each dimension is host-bounded at 2048. */
export interface RasterizeOptions { width?: number; height?: number; format?: "png" | "jpeg" }
export interface RasterizeResult { blob: string; contentType: string; width: number; height: number; size: number }

const B64_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
const B64_TABLE = (() => {
  const t = new Int8Array(128).fill(-1);
  for (let i = 0; i < B64_ALPHABET.length; i++) {
    t[B64_ALPHABET.charCodeAt(i)] = i;
  }
  return t;
})();

function checkDataRange(r: DataRange): void {
  if (
    !Number.isSafeInteger(r.offset) ||
    r.offset < 0 ||
    !Number.isSafeInteger(r.length) ||
    r.length <= 0 ||
    r.length > MaxDataReadBytes
  ) {
    throw new Error(`Data range needs nonnegative offset and length 1..${MaxDataReadBytes}`);
  }
}

function checkArchiveReads(reads: ArchiveReadItem[]): void {
  if (!Array.isArray(reads) || reads.length === 0 || reads.length > MaxArchiveReadItems) {
    throw new Error(`archive.read takes 1..${MaxArchiveReadItems} reads`);
  }
  let total = 0;
  for (let i = 0; i < reads.length; i++) {
    const item = reads[i]!;
    if (!Number.isSafeInteger(item.member) || item.member < 0) {
      throw new Error("archive.read member index must be a nonnegative integer");
    }
    checkDataRange(item);
    total += item.length;
    if (total > MaxDataReadBytes) {
      throw new Error(`archive.read requested bytes exceed maximum readBytes ceiling (${MaxDataReadBytes})`);
    }
  }
}

function checkRasterizeOptions(opts?: RasterizeOptions): void {
  if (!opts) return;
  for (const dim of [opts.width, opts.height]) {
    if (dim !== undefined && (!Number.isSafeInteger(dim) || dim < 0 || dim > 2048)) {
      throw new Error("cache.rasterize width and height must be within 0..2048");
    }
  }
  if (opts.format !== undefined && opts.format !== "png" && opts.format !== "jpeg") {
    throw new Error("cache.rasterize format must be png or jpeg");
  }
}

function checkPartIndex(partIndex: number | undefined): void {
  if (partIndex !== undefined && (!Number.isSafeInteger(partIndex) || partIndex < 0)) {
    throw new Error("stream url: partIndex must be a non-negative integer");
  }
}

function encodeData(data: Uint8Array): string {
  const parts: string[] = [];
  for (let i = 0; i < data.length; i += 3) {
    const n = (data[i]! << 16) | ((data[i + 1] ?? 0) << 8) | (data[i + 2] ?? 0);
    parts.push(
      B64_ALPHABET[(n >>> 18) & 63]! +
        B64_ALPHABET[(n >>> 12) & 63]! +
        (i + 1 < data.length ? B64_ALPHABET[(n >>> 6) & 63] : "=") +
        (i + 2 < data.length ? B64_ALPHABET[n & 63] : "="),
    );
  }
  return parts.join("");
}

let sharedTextEncoder: TextEncoder | undefined;
let sharedTextDecoder: TextDecoder | undefined;

export function encodeText(text: string): Uint8Array {
  if (text.length === 0) return new Uint8Array(0);
  const g = globalThis as unknown as GlobalEtherBindings;
  if (typeof g.__ether_text_encode === "function") {
    return new Uint8Array(g.__ether_text_encode(text));
  }
  if (typeof TextEncoder === "function") {
    sharedTextEncoder ??= new TextEncoder();
    return sharedTextEncoder.encode(text);
  }
  const bytes: number[] = [];
  for (let i = 0; i < text.length; i++) {
    let cp = text.charCodeAt(i);
    if (cp >= 0xd800 && cp <= 0xdbff && i + 1 < text.length) {
      const lo = text.charCodeAt(i + 1);
      if (lo >= 0xdc00 && lo <= 0xdfff) {
        cp = 0x10000 + ((cp - 0xd800) << 10) + (lo - 0xdc00);
        i++;
      } else {
        cp = 0xfffd;
      }
    } else if (cp >= 0xd800 && cp <= 0xdfff) {
      cp = 0xfffd;
    }
    if (cp <= 0x7f) {
      bytes.push(cp);
    } else if (cp <= 0x7ff) {
      bytes.push(0xc0 | (cp >> 6), 0x80 | (cp & 0x3f));
    } else if (cp <= 0xffff) {
      bytes.push(0xe0 | (cp >> 12), 0x80 | ((cp >> 6) & 0x3f), 0x80 | (cp & 0x3f));
    } else {
      bytes.push(
        0xf0 | (cp >> 18),
        0x80 | ((cp >> 12) & 0x3f),
        0x80 | ((cp >> 6) & 0x3f),
        0x80 | (cp & 0x3f),
      );
    }
  }
  return new Uint8Array(bytes);
}

export function decodeText(bytes: ArrayBuffer | ArrayBufferView): string {
  if (bytes.byteLength === 0) return "";
  const g = globalThis as unknown as GlobalEtherBindings;
  if (typeof g.__ether_text_decode === "function") {
    return g.__ether_text_decode(bytes);
  }
  if (typeof TextDecoder === "function") {
    sharedTextDecoder ??= new TextDecoder("utf-8", { ignoreBOM: true });
    return sharedTextDecoder.decode(bytes);
  }
  const u8 = ArrayBuffer.isView(bytes)
    ? new Uint8Array(bytes.buffer, bytes.byteOffset, bytes.byteLength)
    : new Uint8Array(bytes);
  let out = "";
  let i = 0;
  while (i < u8.length) {
    const b0 = u8[i]!;
    if (b0 <= 0x7f) {
      let end = i + 1;
      while (end < u8.length && end - i < 4096 && u8[end]! <= 0x7f) end++;
      out += String.fromCharCode(...u8.subarray(i, end));
      i = end;
    } else if (b0 >= 0xc2 && b0 <= 0xdf) {
      const b1 = u8[i + 1];
      if (b1 !== undefined && b1 >= 0x80 && b1 <= 0xbf) {
        out += String.fromCharCode(((b0 & 0x1f) << 6) | (b1 & 0x3f));
        i += 2;
      } else {
        out += "\ufffd";
        i++;
      }
    } else if (b0 >= 0xe0 && b0 <= 0xef) {
      const b1 = u8[i + 1];
      const lo = b0 === 0xe0 ? 0xa0 : 0x80;
      const hi = b0 === 0xed ? 0x9f : 0xbf;
      if (b1 === undefined || b1 < lo || b1 > hi) {
        out += "\ufffd";
        i++;
      } else {
        const b2 = u8[i + 2];
        if (b2 === undefined || b2 < 0x80 || b2 > 0xbf) {
          out += "\ufffd";
          i += 2;
        } else {
          out += String.fromCharCode(((b0 & 0x0f) << 12) | ((b1 & 0x3f) << 6) | (b2 & 0x3f));
          i += 3;
        }
      }
    } else if (b0 >= 0xf0 && b0 <= 0xf4) {
      const b1 = u8[i + 1];
      const lo = b0 === 0xf0 ? 0x90 : 0x80;
      const hi = b0 === 0xf4 ? 0x8f : 0xbf;
      if (b1 === undefined || b1 < lo || b1 > hi) {
        out += "\ufffd";
        i++;
      } else {
        const b2 = u8[i + 2];
        if (b2 === undefined || b2 < 0x80 || b2 > 0xbf) {
          out += "\ufffd";
          i += 2;
        } else {
          const b3 = u8[i + 3];
          if (b3 === undefined || b3 < 0x80 || b3 > 0xbf) {
            out += "\ufffd";
            i += 3;
          } else {
            const cp = (((b0 & 0x07) << 18) | ((b1 & 0x3f) << 12) | ((b2 & 0x3f) << 6) | (b3 & 0x3f)) - 0x10000;
            out += String.fromCharCode(0xd800 + (cp >> 10), 0xdc00 + (cp & 0x3ff));
            i += 4;
          }
        }
      }
    } else {
      out += "\ufffd";
      i++;
    }
  }
  return out;
}

/** Decode a base64 string (for example a frontend request's bodyBase64) to bytes. */
export function decodeBase64(text: string): Uint8Array {
  return decodeData({ bodyBase64: text });
}

function decodeData(r: { bodyBase64: string }): Uint8Array {
  const text = r.bodyBase64;
  if (text.length === 0) return new Uint8Array(0);
  const g = globalThis as unknown as GlobalEtherBindings;
  if (typeof g.__ether_b64_decode === "function") {
    return new Uint8Array(g.__ether_b64_decode(text));
  }
  const len = text.length;
  if ((len & 3) !== 0) throw new Error("Invalid base64 host result");
  const pad = text.charCodeAt(len - 1) === 61 ? (text.charCodeAt(len - 2) === 61 ? 2 : 1) : 0;
  const out = new Uint8Array(((len * 3) >> 2) - pad);
  const mainEnd = pad > 0 ? len - 4 : len;
  let pos = 0;
  for (let i = 0; i < mainEnd; i += 4) {
    const c0 = text.charCodeAt(i);
    const c1 = text.charCodeAt(i + 1);
    const c2 = text.charCodeAt(i + 2);
    const c3 = text.charCodeAt(i + 3);
    const b0 = c0 < 128 ? B64_TABLE[c0]! : -1;
    const b1 = c1 < 128 ? B64_TABLE[c1]! : -1;
    const b2 = c2 < 128 ? B64_TABLE[c2]! : -1;
    const b3 = c3 < 128 ? B64_TABLE[c3]! : -1;
    if ((b0 | b1 | b2 | b3) < 0) throw new Error("Invalid base64 host result");
    const n = (b0 << 18) | (b1 << 12) | (b2 << 6) | b3;
    out[pos++] = (n >> 16) & 255;
    out[pos++] = (n >> 8) & 255;
    out[pos++] = n & 255;
  }
  if (pad > 0) {
    const c0 = text.charCodeAt(len - 4);
    const c1 = text.charCodeAt(len - 3);
    const b0 = c0 < 128 ? B64_TABLE[c0]! : -1;
    const b1 = c1 < 128 ? B64_TABLE[c1]! : -1;
    if ((b0 | b1) < 0) throw new Error("Invalid base64 host result");
    if (pad === 2) {
      out[pos++] = ((b0 << 2) | (b1 >> 4)) & 255;
    } else {
      const c2 = text.charCodeAt(len - 2);
      const b2 = c2 < 128 ? B64_TABLE[c2]! : -1;
      if (b2 < 0) throw new Error("Invalid base64 host result");
      const n = (b0 << 12) | (b1 << 6) | b2;
      out[pos++] = (n >> 10) & 255;
      out[pos++] = (n >> 2) & 255;
    }
  }
  return out;
}

export interface FolderHashJob { id: string; state: "running" | "done" | "canceled" | "error"; bytes: number; result?: FolderHash; error?: string }
export interface FolderHash { size: number; modTime: number; crc32: string; md5: string; sha1: string }

export interface FolderEntry {
  name: string;
  path: string;
  dir: boolean;
  size?: number;
  mtime?: number;
}

export interface CatalogTitleSummary {
  id: number | string;
  title: string;
  type: string;
  year?: number;
  imdbId?: string;
  tmdbId?: number;
  genres?: string[];
  posterPath?: string;
  summary?: string;
}

export interface ViewerWatchEntry {
  titleId: string;
  title?: string;
  watchedAt?: string;
  type?: string;
  progress?: number;
  completed?: boolean;
}

export interface KVUsage {
  bytes: number;
  keys: number;
  quotaBytes: number;
  quotaKeys: number;
}

export interface EtherHost {
  runtime?: { limits(): MaybePromise<RuntimeLimits> };
  settings?: { get(): MaybePromise<Record<string, unknown>> };
  kv: {
    get(key: string): MaybePromise<string | null>;
    put(key: string, value: string): MaybePromise<void>;
    delete(key: string): MaybePromise<void>;
    list(prefix?: string): MaybePromise<string[]>;
    usage(): MaybePromise<KVUsage>;
  };
  badges: {
    set(setKey: string, entries: BadgeEntry[]): MaybePromise<void>;
    clear(setKey: string): MaybePromise<void>;
    usage(): MaybePromise<BadgeUsage>;
  };
  secret: {
    get(name: string): MaybePromise<string>;
  };
  http: {
    fetch(req: HostFetchRequest): MaybePromise<HostFetchResponse>;
    hash?(req: HashRequest): MaybePromise<HashResult>;
    fetchToCache?(req: FetchCacheRequest): MaybePromise<FetchCacheResult>;
  };
  catalog: {
    search(
      query: string,
      limit?: number,
      offset?: number,
    ): MaybePromise<{ titles: CatalogTitleSummary[] }>;
    get(id: string): MaybePromise<CatalogTitleSummary | null>;
  };
  viewers: {
    granted(): MaybePromise<Array<{ id: number; name: string }>>;
    history(
      viewerId: number,
      limit?: number,
      offset?: number,
    ): MaybePromise<ViewerWatchEntry[]>;
    setWatchState(
      viewerId: number,
      titleId: string,
      watched: boolean,
    ): MaybePromise<void>;
  };
  collections: {
    upsert(collection: Record<string, unknown>): MaybePromise<void>;
  };
  folders: {
    list(root: string, path?: string): MaybePromise<FolderEntry[]>;
    stat(root: string, path: string): MaybePromise<FolderEntry>;
    hash(root: string, path: string): MaybePromise<FolderHash>;
    hashStart(root: string, path: string): MaybePromise<FolderHashJob>;
    hashPoll(id: string): MaybePromise<FolderHashJob>;
    hashCancel(id: string): MaybePromise<FolderHashJob>;
  };
  events: {
    publish(
      key: string,
      payload?: Record<string, unknown>,
      correlation?: EventCorrelation,
    ): MaybePromise<void>;
  };
  notify: {
    owner(text: string): MaybePromise<void>;
  };
  /** Content-kind host services (hosted, remote, and mock clients provide them). */
  kinds?: {
    upsert(kind: string, items: import("./types.ts").KindItem[]): MaybePromise<number>;
    delete(kind: string, externalIds: string[]): MaybePromise<void>;
    items(kind: string, cursor?: string, limit?: number): MaybePromise<import("./types.ts").KindPage>;
    /** Returns the subset of external IDs (max 500) allowed by the library's Rules for the requesting adapter viewer (#615). */
    visible(kind: string, ids: string[], viewerToken?: string): MaybePromise<string[]>;
    /** Browses or searches a kind library with Rules and viewer policy applied for the requesting adapter viewer (#615). */
    browse(kind: string, query?: import("./types.ts").KindBrowseQuery, viewerToken?: string): MaybePromise<import("./types.ts").KindPage>;
    resolveSources(q: import("./types.ts").SourceQuery): MaybePromise<import("./types.ts").SourceCandidate[]>;
  };
  cache?: {
    put(libraryKey: string, name: string, data: Uint8Array): MaybePromise<string>;
    open(blobRef: string): MaybePromise<Uint8Array>;
    read?(blobRef: string, range: DataRange): MaybePromise<Uint8Array>;
    /** Rasterises a cached SVG blob into a new cached bitmap blob with Ether's bounded SVG rasteriser; unsafe documents are refused. */
    rasterize?(libraryKey: string, name: string, blobRef: string, opts?: RasterizeOptions): MaybePromise<RasterizeResult>;
    delete?(blobRef: string): MaybePromise<void>;
  };
  /** ZIP (Store/Deflate), 7z (Copy/LZMA/LZMA2), and RAR 4/5 data services.
   * Ranges bound returned bytes; solid reads restart at the block beginning.
   * Prefer large sequential chunks. Host byte and memory grants apply. */
  archive?: {
    list(blobRef: string): MaybePromise<ArchiveMember[]>;
    open(blobRef: string, member: number, range: DataRange): MaybePromise<Uint8Array>;
    read(blobRef: string, reads: ArchiveReadItem[]): MaybePromise<ArchiveReadResult[]>;
    extract(blobRef: string, member: number, opts?: ArchiveExtractOptions): MaybePromise<ArchiveExtractResult>;
  };
  /**
   * Frontend-adapter host services (spec §6.5). streamUrl mints a scoped,
   * short-lived Ether stream URL for one of this plugin's own kind items
   * (or one zero-based part of a multi-file item when partIndex is given);
   * bytes are then served by Ether, never by the plugin.
   */
  frontend?: {
    streamUrl(viewerId: number, itemRef: string, partIndex?: number): MaybePromise<string>;
  };
  log(level: string, message: string, attrs?: Record<string, unknown>): MaybePromise<void>;
  abort(message?: string): never;
}

interface GlobalEtherBindings {
  __ether_host?: (name: string, argsJson: string) => string;
  __ether_host_binary?: (name: string, argsJson: string) => unknown;
  __ether_b64_decode?: (text: string) => ArrayBuffer;
  __ether_text_decode?: (bytes: ArrayBuffer | ArrayBufferView) => string;
  __ether_text_encode?: (text: string) => ArrayBuffer;
  ether?: {
    log: (...args: unknown[]) => void;
    abort: (msg?: string) => never;
    kv: {
      get: (k: string) => string | null;
      put: (k: string, v: string) => void;
      delete: (k: string) => void;
      list: (prefix?: string) => string[];
      usage?: () => KVUsage;
    };
    badges?: {
      set: (setKey: string, entries: BadgeEntry[]) => void;
      clear: (setKey: string) => void;
      usage: () => BadgeUsage;
    };
    secret: {
      get: (name: string) => string;
    };
    http: {
      fetch: (req: HostFetchRequest) => HostFetchResponse;
    };
    catalog: {
      search: (q: string, limit?: number, offset?: number) => { titles: CatalogTitleSummary[] };
      get: (id: string) => CatalogTitleSummary | null;
    };
    viewers: {
      granted: () => Array<{ id: number; name: string }>;
      history: (viewerId: number, limit?: number, offset?: number) => ViewerWatchEntry[];
      setWatchState: (
        viewerIdOrObj: number | { viewerId: number; titleId: string; watched: boolean },
        titleId?: string,
        watched?: boolean,
      ) => void;
    };
    collections: {
      upsert: (c: Record<string, unknown>) => void;
    };
    folders: {
      list: (root: string, path?: string) => FolderEntry[];
      stat: (root: string, path: string) => FolderEntry;
      hash: (root: string, path: string) => FolderHash;
      hashStart: (root: string, path: string) => FolderHashJob;
      hashPoll: (id: string) => FolderHashJob;
      hashCancel: (id: string) => FolderHashJob;
    };
    events?: {
      publish: (
        key: string,
        payload?: Record<string, unknown>,
        correlation?: EventCorrelation,
      ) => void;
    };
    notify: {
      owner: (text: string) => void;
    };
    kinds?: {
      upsert: (kind: string, items: import("./types.ts").KindItem[]) => { accepted: number } | number;
      delete: (kind: string, externalIds: string[]) => void;
      items: (kind: string, cursor?: string, limit?: number) => import("./types.ts").KindPage;
      visible: (kind: string, ids: string[]) => string[];
      browse: (kind: string, query?: import("./types.ts").KindBrowseQuery) => import("./types.ts").KindPage;
    };
    frontend: {
      streamURL: (itemRef: string, partIndex?: number) => { url: string };
    };
  };
}

/**
 * Returns true when running inside the Ether QuickJS hosted runtime.
 */
export function isHostedRuntime(): boolean {
  const g = globalThis as unknown as GlobalEtherBindings;
  return typeof g.__ether_host === "function" || typeof g.ether === "object";
}

/**
 * Creates a host client bound to the QuickJS hosted runtime globals (`globalThis.ether` / `__ether_host`).
 */
export function createHostedClient(): EtherHost {
  const g = globalThis as unknown as GlobalEtherBindings;

  const callHost = <T>(name: string, args: unknown[]): T => {
    if (typeof g.__ether_host === "function") {
      const raw = g.__ether_host(name, JSON.stringify(args));
      return (raw ? JSON.parse(raw) : null) as T;
    }
    throw new Error(`Host service ${name} is not available outside the Ether runtime`);
  };

  return {
    runtime: {
      limits: () => callHost<RuntimeLimits>("runtime.limits", []),
    },
    archive: {
      list: (blob) => callHost<ArchiveMember[]>("archive.list", [blob]),
      open: (blob, member, range) => {
        checkDataRange(range);
        if (typeof g.__ether_host_binary === "function") {
          return new Uint8Array(g.__ether_host_binary("archive.open", JSON.stringify([blob, member, range])) as ArrayBuffer);
        }
        return decodeData(callHost("archive.open", [blob, member, range]));
      },
      read: (blob, reads) => {
        checkArchiveReads(reads);
        if (typeof g.__ether_host_binary === "function") {
          const packed = g.__ether_host_binary("archive.read", JSON.stringify([blob, reads])) as { metaJson: string; buffer: ArrayBuffer };
          const meta = JSON.parse(packed.metaJson) as Array<{ index: number; member: number; size: number; offset: number }>;
          return meta.map((m) => ({ index: m.index, member: m.member, size: m.size, body: new Uint8Array(packed.buffer, m.offset, m.size) }));
        }
        const rows = callHost<Array<{ index: number; member: number; size: number; bodyBase64: string }>>("archive.read", [blob, reads]);
        return rows.map((row) => ({ index: row.index, member: row.member, size: row.size, body: decodeData(row) }));
      },
      extract: (blob, member, opts) => callHost<ArchiveExtractResult>("archive.extract", opts == null ? [blob, member] : [blob, member, opts]),
    },
    cache: {
      put: (lib, name, data) => callHost<{blob: string}>("cache.put", [lib, name, {bodyBase64: encodeData(data)}]).blob,
      open: (blob) => { const r = callHost<{body?: string; bodyBase64?: string}>("cache.open", [blob]); return r.bodyBase64 ? decodeData({bodyBase64: r.bodyBase64}) : encodeText(r.body ?? ""); },
      read: (blob, range) => {
        checkDataRange(range);
        if (typeof g.__ether_host_binary === "function") {
          return new Uint8Array(g.__ether_host_binary("cache.read", JSON.stringify([blob, range])) as ArrayBuffer);
        }
        return decodeData(callHost("cache.read", [blob, range]));
      },
      rasterize: (lib, name, blob, opts) => {
        checkRasterizeOptions(opts);
        return callHost<RasterizeResult>("cache.rasterize", opts == null ? [lib, name, blob] : [lib, name, blob, opts]);
      },
      delete: (blob) => {
        callHost("cache.delete", [blob]);
      },
    },
    settings: { get: () => callHost<Record<string, unknown>>("settings.get", []) },
    kv: {
      get(key: string): string | null {
        if (g.ether?.kv) return g.ether.kv.get(key) ?? null;
        return callHost<string | null>("kv.get", [key]);
      },
      put(key: string, value: string): void {
        if (g.ether?.kv) {
          g.ether.kv.put(key, String(value));
          return;
        }
        callHost("kv.put", [key, String(value)]);
      },
      delete(key: string): void {
        if (g.ether?.kv) {
          g.ether.kv.delete(key);
          return;
        }
        callHost("kv.delete", [key]);
      },
      list(prefix = ""): string[] {
        if (g.ether?.kv) return g.ether.kv.list(prefix) ?? [];
        return callHost<string[]>("kv.list", [prefix]) ?? [];
      },
      usage(): KVUsage {
        if (g.ether?.kv?.usage) return g.ether.kv.usage();
        return callHost<KVUsage>("kv.usage", []);
      },
    },
    badges: {
      set(setKey: string, entries: BadgeEntry[]): void {
        if (g.ether?.badges) {
          g.ether.badges.set(setKey, entries);
          return;
        }
        callHost("badges.set", [setKey, entries]);
      },
      clear(setKey: string): void {
        if (g.ether?.badges) {
          g.ether.badges.clear(setKey);
          return;
        }
        callHost("badges.clear", [setKey]);
      },
      usage(): BadgeUsage {
        if (g.ether?.badges) return g.ether.badges.usage();
        return callHost<BadgeUsage>("badges.usage", []);
      },
    },
    secret: {
      get(name: string): string {
        try {
          if (g.ether?.secret) return g.ether.secret.get(name) ?? "";
          return callHost<string>("secret.get", [name]) ?? "";
        } catch (err) {
          if (err instanceof Error && /is not saved/i.test(err.message)) return "";
          throw err;
        }
      },
    },
    http: {
      hash: (req) => callHost<HashResult>("http.hash", [req]),
      fetchToCache: (req) => callHost<FetchCacheResult>("http.fetchToCache", [req]),
      fetch(req: HostFetchRequest): HostFetchResponse {
        const normalized = {
          timeoutMs: req.timeoutMs,
          method: req.method ?? "GET",
          url: req.url,
          headers: req.headers ?? {},
          ...(req.bodyBase64 !== undefined ? { bodyBase64: req.bodyBase64 } : { body: req.body ?? "" }),
        };
        if (g.ether?.http) return g.ether.http.fetch(normalized);
        return callHost<HostFetchResponse>("http.fetch", [normalized]);
      },
    },
    catalog: {
      search(query: string, limit = 50, offset = 0): { titles: CatalogTitleSummary[] } {
        if (typeof g.__ether_host === "function") {
          return callHost<{ titles: CatalogTitleSummary[] }>("catalog.search", [
            query,
            limit,
            offset,
          ]);
        }
        if (g.ether?.catalog) return g.ether.catalog.search(query, limit, offset);
        return { titles: [] };
      },
      get(id: string): CatalogTitleSummary | null {
        if (g.ether?.catalog) return g.ether.catalog.get(id);
        return callHost<CatalogTitleSummary | null>("catalog.get", [id]);
      },
    },
    viewers: {
      granted() { return callHost<Array<{ id: number; name: string }>>("viewers.granted", []) ?? []; },
      history(viewerId: number, limit = 50, offset = 0): ViewerWatchEntry[] {
        if (typeof g.__ether_host === "function") {
          return callHost<ViewerWatchEntry[]>("viewers.history", [viewerId, limit, offset]) ?? [];
        }
        if (g.ether?.viewers) return g.ether.viewers.history(viewerId, limit, offset) ?? [];
        return [];
      },
      setWatchState(viewerId: number, titleId: string, watched: boolean): void {
        if (typeof g.__ether_host === "function") {
          callHost("viewers.setWatchState", [viewerId, titleId, watched]);
          return;
        }
        if (g.ether?.viewers) {
          g.ether.viewers.setWatchState(viewerId, titleId, watched);
          return;
        }
        callHost("viewers.setWatchState", [viewerId, titleId, watched]);
      },
    },
    collections: {
      upsert(collection: Record<string, unknown>): void {
        if (g.ether?.collections) {
          g.ether.collections.upsert(collection);
          return;
        }
        callHost("collections.upsert", [collection]);
      },
    },
    folders: {
      list(root: string, path = ""): FolderEntry[] {
        if (g.ether?.folders) return g.ether.folders.list(root, path) ?? [];
        return callHost<FolderEntry[]>("folders.list", [root, path]) ?? [];
      },
      hashStart(root: string, path: string): FolderHashJob { if (g.ether?.folders) return g.ether.folders.hashStart(root, path); return callHost<FolderHashJob>("folders.hashStart", [root, path]); },
      hashPoll(id: string): FolderHashJob { if (g.ether?.folders) return g.ether.folders.hashPoll(id); return callHost<FolderHashJob>("folders.hashPoll", [id]); },
      hashCancel(id: string): FolderHashJob { if (g.ether?.folders) return g.ether.folders.hashCancel(id); return callHost<FolderHashJob>("folders.hashCancel", [id]); },
      hash(root: string, path: string): FolderHash {
        if (g.ether?.folders) return g.ether.folders.hash(root, path);
        return callHost<FolderHash>("folders.hash", [root, path]);
      },
      stat(root: string, path: string): FolderEntry {
        if (g.ether?.folders) return g.ether.folders.stat(root, path);
        return callHost<FolderEntry>("folders.stat", [root, path]);
      },
    },
    events: {
      publish(
        key: string,
        payload?: Record<string, unknown>,
        correlation?: EventCorrelation,
      ): void {
        const body = payload ?? {};
        if (g.ether?.events?.publish) {
          if (correlation !== undefined) {
            g.ether.events.publish(key, body, correlation);
          } else {
            g.ether.events.publish(key, body);
          }
          return;
        }
        callHost(
          "events.publish",
          correlation !== undefined ? [key, body, correlation] : [key, body],
        );
      },
    },
    notify: {
      owner(text: string): void {
        if (g.ether?.notify) {
          g.ether.notify.owner(text);
          return;
        }
        callHost("notify.owner", [text]);
      },
    },
    kinds: {
      upsert(kind, items) {
        if (g.ether?.kinds?.upsert) {
          const r = g.ether.kinds.upsert(kind, items);
          return typeof r === "number" ? r : r?.accepted ?? 0;
        }
        const r = callHost<{ accepted: number }>("kinds.upsert", [kind, items]);
        return r?.accepted ?? 0;
      },
      delete(kind, externalIds) {
        if (g.ether?.kinds?.delete) {
          g.ether.kinds.delete(kind, externalIds);
          return;
        }
        callHost("kinds.delete", [kind, externalIds]);
      },
      items(kind, cursor = "", limit = 200) {
        if (g.ether?.kinds?.items) {
          return g.ether.kinds.items(kind, cursor, limit) ?? { items: [] };
        }
        return callHost<import("./types.ts").KindPage>("kinds.items", [kind, cursor, limit]) ?? { items: [] };
      },
      visible(kind, ids, _viewerToken) {
        if (ids.length > MAX_KIND_VISIBLE_IDS) {
          throw new Error(`kinds.visible: batch larger than ${MAX_KIND_VISIBLE_IDS} ids`);
        }
        if (g.ether?.kinds?.visible) {
          return g.ether.kinds.visible(kind, ids) ?? [];
        }
        return callHost<string[]>("kinds.visible", [kind, ids]) ?? [];
      },
      browse(kind, query = {}, _viewerToken) {
        if (g.ether?.kinds?.browse) {
          return g.ether.kinds.browse(kind, query) ?? { items: [] };
        }
        return callHost<import("./types.ts").KindPage>("kinds.browse", [kind, query]) ?? { items: [] };
      },
      resolveSources(q) {
        const r = callHost<{ candidates: import("./types.ts").SourceCandidate[] }>("sources.resolve", [q]);
        return r?.candidates ?? [];
      },
    },
    frontend: {
      streamUrl(viewerId: number, itemRef: string, partIndex?: number): string {
        // The hosted binding is viewer-scoped by Ether already; the viewerId
        // argument is accepted for parity with the remote client.
        void viewerId;
        checkPartIndex(partIndex);
        const r = callHost<{ url: string }>(
          "frontend.streamURL",
          partIndex === undefined ? [itemRef] : [itemRef, partIndex],
        );
        return r?.url ?? "";
      },
    },
    log(level: string, message: string, attrs?: Record<string, unknown>): void {
      if (typeof g.__ether_host === "function") {
        try {
          g.__ether_host(
            "log",
            JSON.stringify(attrs ? [level, message, attrs] : [level, message]),
          );
        } catch {
          // Logging never fails the caller.
        }
        return;
      }
      if (g.ether?.log) {
        g.ether.log(level, message, attrs ? JSON.stringify(attrs) : "");
      }
    },
    abort(message = "aborted"): never {
      if (g.ether?.abort) {
        return g.ether.abort(message);
      }
      throw new Error(
        "__ETHER_FAULT__" + JSON.stringify({ code: "aborted", message: String(message) }),
      );
    },
  };
}

export const createHostedHostClient = createHostedClient;
// Choose the host transport for the runtime that loaded this plugin.
export const ether: EtherHost = isHostedRuntime()
  ? createHostedClient()
  : createRemoteHostClient();

export interface RemoteHostOptions {
  /** Base URL of the Ether server (for example "http://127.0.0.1:32450"). */
  baseUrl?: string;
  /** Plugin credential issued during pairing (`X-Ether-Plugin-Credential`). */
  credential?: string;
  /** Optional host-minted frontend adapter viewer token (`X-Ether-Viewer-Token`, #615). */
  viewerToken?: string;
  /** Optional custom fetch implementation for testing. */
  fetchImpl?: typeof fetch;
}

function readEnv(name: string): string | undefined {
  const g = globalThis as { process?: { env?: Record<string, string | undefined> } };
  return g.process?.env?.[name];
}

/**
 * Creates a host client for remote/sidecar plugins that calls `/ether/plugin-host/*`
 * on Ether with `X-Ether-Plugin-Credential` (SPEC §5.1.5, WS8b).
 */
export function createRemoteHostClient(opts: RemoteHostOptions = {}): EtherHost {
  const rawBase = opts.baseUrl ?? readEnv("ETHER_HOST_URL") ?? "http://127.0.0.1:32450";
  const base = rawBase.replace(/\/+$/, "");
  const credential = opts.credential ?? readEnv("ETHER_PLUGIN_CREDENTIAL") ?? "";
  const doFetch = opts.fetchImpl ?? fetch;

  const callJson = async <T>(
    method: string,
    path: string,
    body?: unknown,
    allow404 = false,
    extraHeaders?: Record<string, string>,
  ): Promise<T | null> => {
    const headers: Record<string, string> = {
      "X-Ether-Plugin-Credential": credential,
      Accept: "application/json",
      ...(extraHeaders ?? {}),
    };
    let payload: string | undefined;
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      payload = JSON.stringify(body);
    }
    const resp = await doFetch(`${base}${path}`, {
      method,
      headers,
      body: payload,
    });
    if (allow404 && resp.status === 404) {
      return null;
    }
    const text = await resp.text();
    const parsed = text ? (JSON.parse(text) as Record<string, unknown>) : null;
    if (!resp.ok) {
      const errObj =
        parsed && typeof parsed.error === "object" ? (parsed.error as Record<string, string>) : null;
      throw new Error(
        errObj?.message ?? `Host call ${method} ${path} failed with HTTP ${resp.status}`,
      );
    }
    return parsed as T;
  };

  return {
    runtime: {
      limits: async () => (await callJson<RuntimeLimits>("GET", "/ether/plugin-host/limits")) ?? {},
    },
    archive: {
      list: async (blob) => (await callJson<ArchiveMember[]>("POST", "/ether/plugin-host/data/archive.list", [blob])) ?? [],
      open: async (blob, member, range) => { checkDataRange(range); return decodeData((await callJson<{bodyBase64: string}>("POST", "/ether/plugin-host/data/archive.open", [blob, member, range]))!); },
      read: async (blob, reads) => {
        checkArchiveReads(reads);
        const rows = (await callJson<Array<{ index: number; member: number; size: number; bodyBase64: string }>>("POST", "/ether/plugin-host/data/archive.read", [blob, reads])) ?? [];
        return rows.map((row) => ({ index: row.index, member: row.member, size: row.size, body: decodeData(row) }));
      },
      extract: async (blob, member, opts) => (await callJson<ArchiveExtractResult>("POST", "/ether/plugin-host/data/archive.extract", opts == null ? [blob, member] : [blob, member, opts]))!,
    },
    ...(credential ? { settings: { get: async () => (await callJson<{ settings: Record<string, unknown> }>("GET", "/ether/plugin-host/settings"))?.settings ?? {} } } : {}),
    kv: {
      async get(key: string): Promise<string | null> {
        const res = await callJson<{ key: string; value: string }>(
          "GET",
          `/ether/plugin-host/kv/${encodeURIComponent(key)}`,
          undefined,
          true,
        );
        return res ? res.value : null;
      },
      async put(key: string, value: string): Promise<void> {
        await callJson("PUT", `/ether/plugin-host/kv/${encodeURIComponent(key)}`, {
          value: String(value),
        });
      },
      async delete(key: string): Promise<void> {
        await callJson("DELETE", `/ether/plugin-host/kv/${encodeURIComponent(key)}`);
      },
      async list(prefix = ""): Promise<string[]> {
        const q = prefix ? `?prefix=${encodeURIComponent(prefix)}` : "";
        const res = await callJson<{ keys: string[] }>("GET", `/ether/plugin-host/kv${q}`);
        return res?.keys ?? [];
      },
      async usage(): Promise<KVUsage> {
        return (await callJson<KVUsage>("GET", "/ether/plugin-host/kv-usage"))!;
      },
    },
    badges: {
      async set(setKey: string, entries: BadgeEntry[]): Promise<void> {
        await callJson("PUT", `/ether/plugin-host/badges/${encodeURIComponent(setKey)}`, {
          entries,
        });
      },
      async clear(setKey: string): Promise<void> {
        await callJson("DELETE", `/ether/plugin-host/badges/${encodeURIComponent(setKey)}`);
      },
      async usage(): Promise<BadgeUsage> {
        return (await callJson<BadgeUsage>("GET", "/ether/plugin-host/badges-usage"))!;
      },
    },
    secret: {
      get(_name: string): Promise<string> {
        return Promise.reject(
          new Error(
            "secret.get is only available in hosted plugins; remote plugins receive secrets via /ether/configure",
          ),
        );
      },
    },
    http: {
      hash: async (req) => (await callJson<HashResult>("POST", "/ether/plugin-host/data/http.hash", [req]))!,
      fetchToCache: async (req) => (await callJson<FetchCacheResult>("POST", "/ether/plugin-host/data/http.fetchToCache", [req]))!,
      async fetch(req: HostFetchRequest): Promise<HostFetchResponse> {
        const resp = await doFetch(req.url, {
          method: req.method ?? "GET",
          headers: req.headers,
          body: req.body,
          signal: req.timeoutMs && req.timeoutMs > 0 ? AbortSignal.timeout(req.timeoutMs) : undefined,
        });
        const outHeaders: Record<string, string> = {};
        resp.headers.forEach((v, k) => {
          outHeaders[k] = v;
        });
        return {
          status: resp.status,
          headers: outHeaders,
          body: await resp.text(),
        };
      },
    },
    catalog: {
      async search(
        query: string,
        limit = 50,
        offset = 0,
      ): Promise<{ titles: CatalogTitleSummary[] }> {
        const params = new URLSearchParams({
          q: query,
          limit: String(limit),
          offset: String(offset),
        });
        const res = await callJson<{ titles: CatalogTitleSummary[] }>(
          "GET",
          `/ether/plugin-host/catalog/search?${params.toString()}`,
        );
        return res ?? { titles: [] };
      },
      async get(id: string): Promise<CatalogTitleSummary | null> {
        return callJson<CatalogTitleSummary>(
          "GET",
          `/ether/plugin-host/catalog/${encodeURIComponent(id)}`,
          undefined,
          true,
        );
      },
    },
    viewers: {
      async granted() { return (await callJson<Array<{ id: number; name: string }>>("GET", "/ether/plugin-host/viewers/granted")) ?? []; },
      async history(viewerId: number, limit = 50, offset = 0): Promise<ViewerWatchEntry[]> {
        const params = new URLSearchParams({
          limit: String(limit),
          offset: String(offset),
        });
        const res = await callJson<ViewerWatchEntry[]>(
          "GET",
          `/ether/plugin-host/viewers/${viewerId}/history?${params.toString()}`,
        );
        return res ?? [];
      },
      async setWatchState(viewerId: number, titleId: string, watched: boolean): Promise<void> {
        await callJson("POST", "/ether/plugin-host/viewers/watch-state", {
          viewerId,
          titleId,
          watched,
        });
      },
    },
    collections: {
      async upsert(collection: Record<string, unknown>): Promise<void> {
        await callJson("POST", "/ether/plugin-host/collections/upsert", collection);
      },
    },
    folders: {
      list(): Promise<FolderEntry[]> {
        return Promise.reject(
          new Error(
            "folders.list is for hosted plugins; sidecar plugins read their local fileRoot directly",
          ),
        );
      },
      hashStart(): Promise<FolderHashJob> { return Promise.reject(new Error("folders.hash requires the hosted runtime")); },
      hashPoll(): Promise<FolderHashJob> { return Promise.reject(new Error("folders.hash requires the hosted runtime")); },
      hashCancel(): Promise<FolderHashJob> { return Promise.reject(new Error("folders.hash requires the hosted runtime")); },
      hash(): Promise<FolderHash> { return Promise.reject(new Error("folders.hash requires the hosted runtime")); },
      stat(): Promise<FolderEntry> {
        return Promise.reject(
          new Error(
            "folders.stat is for hosted plugins; sidecar plugins read their local fileRoot directly",
          ),
        );
      },
    },
    events: {
      async publish(
        key: string,
        payload?: Record<string, unknown>,
        correlation?: EventCorrelation,
      ): Promise<void> {
        const body: Record<string, unknown> = {
          key,
          payload: payload ?? {},
        };
        if (correlation !== undefined) {
          body.correlation = correlation;
        }
        await callJson("POST", "/ether/plugin-host/events/publish", body);
      },
    },
    notify: {
      async owner(text: string): Promise<void> {
        await callJson("POST", "/ether/plugin-host/notify", { text });
      },
    },
    kinds: {
      async upsert(kind, items) {
        const r = await callJson<{ accepted: number }>("POST", `/ether/plugin-host/kinds/${encodeURIComponent(kind)}/upsert`, { items });
        return r?.accepted ?? 0;
      },
      async delete(kind, externalIds) {
        await callJson("POST", `/ether/plugin-host/kinds/${encodeURIComponent(kind)}/delete`, { externalIds });
      },
      async items(kind, cursor = "", limit = 200) {
        return (await callJson("GET", `/ether/plugin-host/kinds/${encodeURIComponent(kind)}/items?cursor=${encodeURIComponent(cursor)}&limit=${limit}`)) ?? { items: [] };
      },
      async visible(kind, ids, viewerToken) {
        const tok = (viewerToken ?? opts.viewerToken ?? "").trim();
        if (!tok) {
          throw new Error("kinds.visible: no authenticated frontend adapter viewer token on this request");
        }
        if (ids.length > MAX_KIND_VISIBLE_IDS) {
          throw new Error(`kinds.visible: batch larger than ${MAX_KIND_VISIBLE_IDS} ids`);
        }
        const r = await callJson<{ ids: string[] }>(
          "POST",
          `/ether/plugin-host/kinds/${encodeURIComponent(kind)}/visible`,
          { ids },
          false,
          { [HEADER_VIEWER_TOKEN]: tok },
        );
        return r?.ids ?? [];
      },
      async browse(kind, query = {}, viewerToken) {
        const tok = (viewerToken ?? opts.viewerToken ?? "").trim();
        if (!tok) {
          throw new Error("kinds.browse: no authenticated frontend adapter viewer token on this request");
        }
        const r = await callJson<import("./types.ts").KindPage>(
          "POST",
          `/ether/plugin-host/kinds/${encodeURIComponent(kind)}/browse`,
          query,
          false,
          { [HEADER_VIEWER_TOKEN]: tok },
        );
        return r ?? { items: [] };
      },
      async resolveSources(q) {
        const r = await callJson<{ candidates: unknown[] }>("POST", "/ether/plugin-host/sources/resolve", q);
        return (r?.candidates ?? []) as never[];
      },
    },
    cache: {
      read: async (blob, range) => { checkDataRange(range); return decodeData((await callJson<{bodyBase64: string}>("POST", "/ether/plugin-host/data/cache.read", [blob, range]))!); },
      rasterize: async (lib, name, blob, opts) => { checkRasterizeOptions(opts); return (await callJson<RasterizeResult>("POST", "/ether/plugin-host/data/cache.rasterize", opts == null ? [lib, name, blob] : [lib, name, blob, opts]))!; },
      async put(libraryKey, name, data) {
        const resp = await doFetch(`${base}/ether/plugin-host/cache/${encodeURIComponent(libraryKey)}?name=${encodeURIComponent(name)}`, {
          method: "POST",
          headers: credential ? { "X-Ether-Plugin-Credential": credential } : {},
          body: data as BodyInit,
        });
        if (!resp.ok) throw new Error(`plugin-host cache put: status ${resp.status}`);
        return ((await resp.json()) as { blob?: string }).blob ?? "";
      },
      async open(blobRef) {
        const resp = await doFetch(`${base}/ether/plugin-host/cache/${blobRef.replace(/^\//, "")}`, {
          headers: credential ? { "X-Ether-Plugin-Credential": credential } : {},
        });
        if (!resp.ok) throw new Error(`plugin-host cache open: status ${resp.status}`);
        return new Uint8Array(await resp.arrayBuffer());
      },
      async delete(blobRef) {
        await callJson("DELETE", `/ether/plugin-host/cache/${encodeURIComponent(blobRef)}`);
      },
    },
    frontend: {
      async streamUrl(viewerId: number, itemRef: string, partIndex?: number): Promise<string> {
        checkPartIndex(partIndex);
        const r = await callJson<{ url: string }>("POST", "/ether/plugin-host/frontend/stream-url", {
          viewerId,
          itemRef,
          ...(partIndex === undefined ? {} : { partIndex }),
        });
        return r?.url ?? "";
      },
    },
    async log(level: string, message: string, attrs?: Record<string, unknown>): Promise<void> {
      await callJson("POST", "/ether/plugin-host/log", { level, message, attrs });
    },
    abort(message = "aborted"): never {
      throw new Error(message);
    },
  };
}
