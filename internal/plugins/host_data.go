package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
)

// Data-service limits are host enforced. Per-call read ceilings scale with the
// plugin's memory grant; all operations use the plugin-owned cache namespace.
const (
	MaxDataFetchBytes       = 8 << 30
	MinDataReadBytes        = wasm.MinDataReadBytes
	MaxDataReadBytes        = wasm.MaxDataReadBytes
	MaxArchiveReadItems     = 4096
	maxArchiveMembers       = 65535
	maxArchiveDirectory     = 64 << 20
	maxArchiveMemberBytes   = 8 << 30
	maxArchiveExpandedBytes = 32 << 30
	maxArchiveRatio         = 1000
)

// DataReadBytes returns the per-call byte ceiling for cache.read, archive.open,
// and archive.read derived from the plugin's memory grant:
// clamp(memoryBytes/4, 8 MiB, 512 MiB).
func DataReadBytes(memoryBytes uint64) int64 {
	return wasm.DataReadBytes(memoryBytes)
}

func dataReadLimit(limits RuntimeLimits) int64 {
	if limits.ReadBytes > 0 {
		return min(max(limits.ReadBytes, MinDataReadBytes), MaxDataReadBytes)
	}
	return DataReadBytes(limits.MemoryBytes)
}

type DataRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// ArchiveReadItem selects one byte range inside one archive member for archive.read.
type ArchiveReadItem struct {
	Member int   `json:"member"`
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// ArchiveReadResult is one item returned by archive.read in request order.
type ArchiveReadResult struct {
	Index      int    `json:"index"`
	Member     int    `json:"member"`
	Size       int    `json:"size"`
	BodyBase64 string `json:"bodyBase64"`
}

type FetchCacheRequest struct {
	FetchRequest
	LibraryKey string `json:"libraryKey"`
	Name       string `json:"name"`
}

type FetchCacheResult struct {
	Blob   string `json:"blob"`
	Size   int64  `json:"size"`
	CRC32  string `json:"crc32"`
	MD5    string `json:"md5"`
	SHA1   string `json:"sha1"`
	Status int    `json:"status"`
}

// ArchiveExtractOptions optionally targets one of the plugin's declared media
// kind libraries or overrides the stored entry name when extracting a member.
// When omitted, archive.extract writes into the source archive's own library
// cache namespace (or the plugin's first declared media kind) using the
// member's base name.
type ArchiveExtractOptions struct {
	LibraryKey string `json:"libraryKey,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ArchiveExtractResult is returned by archive.extract after streaming an
// uncompressed archive member into the plugin's own blob cache in one pass.
type ArchiveExtractResult struct {
	Ref   string `json:"ref"`
	Size  int64  `json:"size"`
	SHA1  string `json:"sha1"`
	MD5   string `json:"md5"`
	CRC32 string `json:"crc32"`
}

// RasterizeOptions selects the output size and format for cache.rasterize.
// A zero Width or Height keeps the SVG aspect ratio; both zero renders at
// the rasteriser's default width. Width and Height are each bounded by
// artwork.MaxRasterDimension and Format is "png" (default) or "jpeg".
type RasterizeOptions struct {
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Format string `json:"format,omitempty"`
}

// RasterizeResult is returned by cache.rasterize after rasterising a cached
// SVG blob into a new cached bitmap blob in the plugin's own library cache.
type RasterizeResult struct {
	Blob        string `json:"blob"`
	ContentType string `json:"contentType"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Size        int64  `json:"size"`
}

// boundedDataReader propagates cancellation and rejects overflow instead of
// turning a truncated file into a successful content-addressed cache entry.
type boundedDataReader struct {
	ctx  context.Context
	r    io.Reader
	left int64
	size int64
}

func (r *boundedDataReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.left < 0 {
		return 0, fmt.Errorf("plugins: data exceeds size limit")
	}
	if int64(len(p)) > r.left+1 {
		p = p[:r.left+1]
	}
	n, err := r.r.Read(p)
	r.left -= int64(n)
	r.size += int64(n)
	if r.left < 0 {
		return n, fmt.Errorf("plugins: data exceeds size limit")
	}
	return n, err
}

// archiveExtractReader bounds extracted bytes by both the archive member grant
// and the target library cache quota, charges per-MiB governance units, and
// refuses truncated streams before CachePut commits a partial blob.
type archiveExtractReader struct {
	ctx         context.Context
	r           io.Reader
	memberLimit int64
	cacheLimit  int64
	expected    int64
	size        int64
	chargedMiB  int64
}

func (r *archiveExtractReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	maxAllowed := min(r.memberLimit, r.cacheLimit)
	if r.size > maxAllowed {
		if r.size > r.cacheLimit {
			return 0, fmt.Errorf("%w: archive member exceeds library cache quota", ErrQuotaExceeded)
		}
		return 0, fmt.Errorf("plugins: archive member exceeds size grant")
	}
	if rem := maxAllowed - r.size; rem < int64(len(p))-1 {
		p = p[:rem+1]
	}
	n, err := r.r.Read(p)
	if n > 0 {
		r.size += int64(n)
		if r.size > r.memberLimit {
			return n, fmt.Errorf("plugins: archive member exceeds size grant")
		}
		if r.size > r.cacheLimit {
			return n, fmt.Errorf("%w: archive member exceeds library cache quota", ErrQuotaExceeded)
		}
		if wantMiB := (r.size + (1 << 20) - 1) >> 20; wantMiB > r.chargedMiB {
			if cerr := wasm.ChargeUnits(r.ctx, (wantMiB-r.chargedMiB)*wasm.ArchiveExtractUnitsPerMiB); cerr != nil {
				return n, cerr
			}
			r.chargedMiB = wantMiB
		}
	}
	if err == io.EOF && r.size != r.expected {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (h *host) HTTPFetchToCache(ctx context.Context, req FetchCacheRequest) (FetchCacheResult, error) {
	if req.Method != "" && req.Method != "GET" {
		return FetchCacheResult{}, fmt.Errorf("plugins: fetchToCache supports GET only")
	}
	if err := h.requireWritableToolContext(ctx); err != nil {
		return FetchCacheResult{}, err
	}
	decl, err := h.cacheWriteCheck(ctx, req.LibraryKey, req.Name)
	if err != nil {
		return FetchCacheResult{}, err
	}
	fetchCtx := ctx
	if req.TimeoutMS > 0 {
		var cancel context.CancelFunc
		fetchCtx, cancel = context.WithTimeout(ctx, time.Duration(min(req.TimeoutMS, int64(86400000)))*time.Millisecond)
		defer cancel()
	}
	resp, err := h.openFetch(fetchCtx, req.FetchRequest)
	if err != nil {
		return FetchCacheResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return FetchCacheResult{}, fmt.Errorf("plugins: fetchToCache HTTP %d", resp.StatusCode)
	}
	crc, m, s := crc32.NewIEEE(), md5.New(), sha1.New()
	reader := &boundedDataReader{ctx: fetchCtx, r: resp.Body, left: min(h.DataLimits(ctx).FetchBytes, decl.Cache.EffectiveMaxBytes())}
	ref, err := h.CachePut(fetchCtx, req.LibraryKey, req.Name, io.TeeReader(reader, io.MultiWriter(crc, m, s)))
	h.noteFetchTraffic(ctx, req.FetchRequest, reader.size)
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) ||
			(errors.As(err, &netErr) && netErr.Timeout()) ||
			(errors.Is(fetchCtx.Err(), context.DeadlineExceeded) && !errors.Is(err, ErrPermissionDenied) && !errors.Is(err, ErrQuotaExceeded)) {
			u, _ := url.Parse(req.URL)
			return FetchCacheResult{}, fetchError(u.Hostname(), context.DeadlineExceeded)
		}
		return FetchCacheResult{}, err
	}
	return FetchCacheResult{Blob: ref, Size: reader.size, CRC32: fmt.Sprintf("%08x", crc.Sum32()), MD5: fmt.Sprintf("%x", m.Sum(nil)), SHA1: fmt.Sprintf("%x", s.Sum(nil)), Status: resp.StatusCode}, nil
}

// CacheRasterize rasterises an SVG already in the plugin's own blob cache
// into a new cached bitmap blob, using Ether's bounded SVG rasteriser. The
// source read is capped at the rasteriser's SVG size limit, output sizes
// are bounded by its dimension limit, and artwork.ErrSVG is returned for
// documents the rasteriser refuses. Rendered variants are cached by the
// rasteriser (by source content and size) and the stored blob is
// content-addressed, so repeat calls return the same reference.
func (h *host) CacheRasterize(ctx context.Context, libraryKey, name, sourceBlobRef string, opts RasterizeOptions) (RasterizeResult, error) {
	if err := h.requireWritableToolContext(ctx); err != nil {
		return RasterizeResult{}, err
	}
	if _, err := h.cacheWriteCheck(ctx, libraryKey, name); err != nil {
		return RasterizeResult{}, err
	}
	format := strings.ToLower(strings.TrimSpace(opts.Format))
	if format == "" {
		format = "png"
	}
	if opts.Width < 0 || opts.Height < 0 || opts.Width > artwork.MaxRasterDimension || opts.Height > artwork.MaxRasterDimension || (format != "png" && format != "jpeg") {
		return RasterizeResult{}, artwork.ErrSVG
	}
	rc, err := h.CacheOpen(ctx, sourceBlobRef)
	if err != nil {
		return RasterizeResult{}, err
	}
	source, err := io.ReadAll(io.LimitReader(rc, artwork.MaxSVGBytes+1))
	_ = rc.Close()
	if err != nil {
		return RasterizeResult{}, err
	}
	raster := h.m.iconRaster
	if raster == nil {
		raster = artwork.NewSVGRasterizer()
	}
	data, contentType, err := raster.Rasterize(ctx, "plugin:"+h.plugin+":"+sourceBlobRef, source, artwork.RasterOptions{Width: opts.Width, Height: opts.Height, Format: format})
	if err != nil {
		return RasterizeResult{}, err
	}
	var dims struct{ Width, Height int }
	if format == "png" {
		cfg, derr := png.DecodeConfig(bytes.NewReader(data))
		if derr != nil {
			return RasterizeResult{}, derr
		}
		dims.Width, dims.Height = cfg.Width, cfg.Height
	} else {
		cfg, derr := jpeg.DecodeConfig(bytes.NewReader(data))
		if derr != nil {
			return RasterizeResult{}, derr
		}
		dims.Width, dims.Height = cfg.Width, cfg.Height
	}
	ref, err := h.CachePut(ctx, libraryKey, name, bytes.NewReader(data))
	if err != nil {
		return RasterizeResult{}, err
	}
	return RasterizeResult{Blob: ref, ContentType: contentType, Width: dims.Width, Height: dims.Height, Size: int64(len(data))}, nil
}

func validDataRange(r DataRange, maxBytes int64) error {
	if r.Offset < 0 || r.Length <= 0 || r.Length > maxBytes || r.Offset > 1<<60 {
		return fmt.Errorf("plugins: range needs nonnegative offset and length 1..%d", maxBytes)
	}
	return nil
}

func readAllSized(r io.Reader, sizeHint int64) ([]byte, error) {
	if sizeHint < 0 {
		sizeHint = 0
	}
	capHint := min(sizeHint+512, int64(8<<20))
	if capHint < 512 {
		capHint = 512
	}
	b := make([]byte, 0, capHint)
	for {
		if len(b) == cap(b) {
			next := max(cap(b)*2, 4096)
			if sizeHint > int64(next) {
				next = int(min(sizeHint+512, int64(MaxDataReadBytes)+512))
			}
			grown := make([]byte, len(b), next)
			copy(grown, b)
			b = grown
		}
		n, err := r.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err != nil {
			if err == io.EOF {
				return b, nil
			}
			return b, err
		}
	}
}

func readDataRangeBytes(ctx context.Context, r io.Reader, opt DataRange, maxBytes, sizeHint int64) ([]byte, error) {
	if err := validDataRange(opt, maxBytes); err != nil {
		return nil, err
	}
	cr := &boundedDataReader{ctx: ctx, r: r, left: opt.Offset + opt.Length}
	if opt.Offset > 0 {
		if _, err := io.CopyN(io.Discard, cr, opt.Offset); err != nil {
			return nil, err
		}
	}
	return readAllSized(io.LimitReader(cr, opt.Length), sizeHint)
}

// Always return base64: arbitrary text/control bytes then have a predictable
// JSON envelope scaled with the plugin's per-call readBytes grant.
func readDataRange(ctx context.Context, r io.Reader, opt DataRange, maxBytes, sizeHint int64) (map[string]any, error) {
	data, err := readDataRangeBytes(ctx, r, opt, maxBytes, sizeHint)
	if err != nil {
		return nil, err
	}
	return map[string]any{"bodyBase64": base64.StdEncoding.EncodeToString(data), "size": len(data)}, nil
}

func readArchiveMemberRange(ctx context.Context, archive *dataArchive, member int, opt DataRange, maxBytes int64) ([]byte, error) {
	if member < 0 || member >= len(archive.members) {
		return nil, fmt.Errorf("plugins: archive member index outside range")
	}
	f := archive.members[member]
	if uint64(opt.Offset) > f.Size {
		return nil, fmt.Errorf("plugins: archive member offset outside range")
	}
	stream, err := archive.open(member)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	rem := int64(min(f.Size-uint64(opt.Offset), uint64(maxBytes)))
	sizeHint := min(opt.Length, rem)
	return readDataRangeBytes(ctx, io.LimitReader(stream, int64(f.Size)), opt, maxBytes, sizeHint)
}

// seekReaderAt gives zip.Reader random access without materializing an archive.
// It is used by one synchronous host call, never shared between goroutines.
type seekReaderAt struct {
	ctx context.Context
	r   io.ReadSeeker
}

func (r seekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if ra, ok := r.r.(io.ReaderAt); ok {
		n, err := ra.ReadAt(p, off)
		if err == io.EOF && n == len(p) {
			err = nil
		}
		return n, err
	}
	if _, err := r.r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r.r, p)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return n, err
}

type ArchiveMember struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Size  uint64 `json:"size"`
	// Unsafe names identify members only and must never be used as output paths.
	Unsafe       bool   `json:"unsafe,omitempty"`
	UnsafeReason string `json:"unsafeReason,omitempty"`
}

func openDataZIP(ctx context.Context, rc io.ReadSeeker, grants ...RuntimeLimits) (*zip.Reader, error) {
	limits := defaultDataLimits()
	if len(grants) > 0 {
		limits = grants[0]
	}
	ra := &archiveIOBudget{r: seekReaderAt{ctx, rc}, left: limits.FetchBytes}
	return parseDataZIP(rc, ra, limits)
}

func parseDataZIP(rc io.ReadSeeker, ra *archiveIOBudget, limits RuntimeLimits) (*zip.Reader, error) {
	directoryCap := archiveDirectoryCap(limits)

	size, err := rc.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size < 22 || size > limits.FetchBytes {
		return nil, fmt.Errorf("plugins: ZIP size outside limit")
	}
	tailSize := min(size, int64(65557))
	tail := make([]byte, tailSize)
	if _, err := ra.ReadAt(tail, size-tailSize); err != nil {
		return nil, err
	}
	// Inspect classic/ZIP64 EOCD before zip.NewReader allocates the directory.
	// Split archives are refused; counts and bytes use owner ceilings.
	found := false
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != 0x06054b50 || i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) != len(tail) {
			continue
		}
		e := tail[i:]
		count := uint64(binary.LittleEndian.Uint16(e[10:]))
		directory := uint64(binary.LittleEndian.Uint32(e[12:]))
		offset := uint64(binary.LittleEndian.Uint32(e[16:]))
		directoryEnd := size - tailSize + int64(i)
		if binary.LittleEndian.Uint16(e[4:]) != 0 || binary.LittleEndian.Uint16(e[6:]) != 0 || uint64(binary.LittleEndian.Uint16(e[8:])) != count {
			return nil, fmt.Errorf("plugins: split ZIP archives are unsupported")
		}
		if count == 65535 || directory == 0xffffffff || offset == 0xffffffff {
			locator := make([]byte, 20)
			if directoryEnd < 20 {
				return nil, fmt.Errorf("plugins: invalid ZIP64 locator")
			}
			if _, err := ra.ReadAt(locator, directoryEnd-20); err != nil {
				return nil, err
			}
			if binary.LittleEndian.Uint32(locator) != 0x07064b50 || binary.LittleEndian.Uint32(locator[4:]) != 0 || binary.LittleEndian.Uint32(locator[16:]) != 1 {
				return nil, fmt.Errorf("plugins: invalid or split ZIP64 archive")
			}
			recordAt := binary.LittleEndian.Uint64(locator[8:])
			if recordAt > uint64(directoryEnd-20) || uint64(directoryEnd-20)-recordAt < 56 {
				return nil, fmt.Errorf("plugins: invalid ZIP64 directory offset")
			}
			record := make([]byte, 56)
			if _, err := ra.ReadAt(record, int64(recordAt)); err != nil {
				return nil, err
			}
			recordLength := binary.LittleEndian.Uint64(record[4:])
			if binary.LittleEndian.Uint32(record) != 0x06064b50 || recordLength < 44 || recordLength > 1<<20 || recordAt+12+recordLength != uint64(directoryEnd-20) || binary.LittleEndian.Uint32(record[16:]) != 0 || binary.LittleEndian.Uint32(record[20:]) != 0 || binary.LittleEndian.Uint64(record[24:]) != binary.LittleEndian.Uint64(record[32:]) {
				return nil, fmt.Errorf("plugins: invalid ZIP64 directory")
			}
			count = binary.LittleEndian.Uint64(record[32:])
			directory = binary.LittleEndian.Uint64(record[40:])
			offset = binary.LittleEndian.Uint64(record[48:])
			directoryEnd = int64(recordAt)
		}
		if count > uint64(limits.ArchiveMembers) || count > uint64(archiveMemory(limits)/1024) || directory > uint64(directoryCap) || offset > uint64(directoryEnd) || directory > uint64(directoryEnd)-offset || offset+directory != uint64(directoryEnd) {
			return nil, fmt.Errorf("plugins: ZIP directory exceeds owner limits or has unsupported layout")
		}
		raw := make([]byte, int(directory))
		if _, err := ra.ReadAt(raw, int64(offset)); err != nil {
			return nil, err
		}
		pos := 0
		for n := 0; n < int(count); n++ {
			if pos+46 > len(raw) || binary.LittleEndian.Uint32(raw[pos:]) != 0x02014b50 {
				return nil, fmt.Errorf("plugins: invalid ZIP directory")
			}
			header := raw[pos:]
			pos += 46 + int(binary.LittleEndian.Uint16(header[28:])) + int(binary.LittleEndian.Uint16(header[30:])) + int(binary.LittleEndian.Uint16(header[32:]))
			if pos > len(raw) {
				return nil, fmt.Errorf("plugins: invalid ZIP directory length")
			}
		}
		if pos != len(raw) {
			return nil, fmt.Errorf("plugins: ZIP directory count mismatch")
		}
		found = true
		break
	}
	if !found {
		return nil, fmt.Errorf("plugins: ZIP directory not found")
	}
	z, err := zip.NewReader(ra, size)
	// With zipinsecurepath=0, Go still returns the directory alongside this
	// warning. Names are identifiers here; the per-member guard controls opens.
	if err != nil && err != zip.ErrInsecurePath {
		return nil, err
	}
	if len(z.File) > limits.ArchiveMembers {
		return nil, fmt.Errorf("plugins: ZIP has too many members")
	}
	var total uint64
	names := 0
	for _, f := range z.File {
		names += len(f.Name)
		if int64(names) > directoryCap {
			return nil, fmt.Errorf("plugins: ZIP member names exceed directory grant")
		}
		if !f.FileInfo().IsDir() && !f.Mode().IsRegular() {
			return nil, fmt.Errorf("plugins: unsupported ZIP member type")
		}
		if f.Flags&1 != 0 || (f.Method != zip.Store && f.Method != zip.Deflate) {
			return nil, fmt.Errorf("plugins: encrypted or unsupported ZIP member")
		}
		if f.CompressedSize64 > uint64(size) || f.UncompressedSize64 > uint64(limits.ArchiveMemberBytes) || (f.UncompressedSize64 > 0 && (f.CompressedSize64 == 0 || (f.CompressedSize64 <= ^uint64(0)/maxArchiveRatio && f.UncompressedSize64 > f.CompressedSize64*maxArchiveRatio))) {
			return nil, fmt.Errorf("plugins: ZIP member exceeds size or compression-ratio limit")
		}
		if f.UncompressedSize64 > uint64(limits.ArchiveExpandedBytes)-total {
			return nil, fmt.Errorf("plugins: ZIP expanded size exceeds limit")
		}
		total += f.UncompressedSize64
	}
	return z, nil
}

// CallDataHost is the shared hosted/remote data-service funnel. Only named
// services are accepted; cache access rechecks plugin ownership each call.
func CallDataHost(ctx context.Context, h Host, name string, raw json.RawMessage) (json.RawMessage, error) {
	ctx = withCallRecord(ctx, h)
	// Remote callers receive the same maximum I/O deadline as hosted calls.
	limits := defaultDataLimits()
	if provider, ok := h.(interface {
		DataLimits(context.Context) RuntimeLimits
	}); ok {
		limits = provider.DataLimits(ctx)
	}
	if active, ok := wasm.CallLimits(ctx); ok {
		limits.MemoryBytes = active.MemoryBytes
		limits.Deadline = active.Deadline.String()
		limits.UnitBudget = active.UnitBudget
		limits.PoolSize = active.PoolSize
	}
	limits.ReadBytes = dataReadLimit(limits)
	readCap := limits.ReadBytes

	deadline, err := time.ParseDuration(limits.Deadline)
	if err != nil || deadline <= 0 {
		deadline = 12 * time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var args []json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	var value any
	if name == "runtime.limits" {
		if len(args) != 0 {
			return nil, fmt.Errorf("plugins: runtime.limits takes no arguments")
		}
		if ph, ok := h.(*host); ok {
			r, rerr := ph.callRecord(ctx)
			if rerr != nil {
				return nil, rerr
			}
			if !r.Enabled {
				return nil, ErrDisabled
			}
		}
		value = limits
	} else if name == "http.hash" {
		var req HashRequest
		if len(args) != 1 {
			return nil, fmt.Errorf("plugins: hash takes one request")
		}
		if err = json.Unmarshal(args[0], &req); err != nil {
			return nil, err
		}
		service, ok := h.(interface {
			HTTPHash(context.Context, HashRequest) (HashResult, error)
		})
		if !ok {
			return nil, ErrUnavailable
		}
		value, err = service.HTTPHash(ctx, req)
	} else if name == "http.fetchToCache" {
		var req struct {
			TimeoutMS  int64             `json:"timeoutMs"`
			Method     string            `json:"method"`
			URL        string            `json:"url"`
			Headers    map[string]string `json:"headers"`
			LibraryKey string            `json:"libraryKey"`
			Name       string            `json:"name"`
		}
		if len(args) != 1 {
			return nil, fmt.Errorf("plugins: fetchToCache takes one request")
		}
		if err = json.Unmarshal(args[0], &req); err != nil {
			return nil, err
		}
		if req.Method != "" && req.Method != "GET" {
			return nil, fmt.Errorf("plugins: fetchToCache supports GET only")
		}
		service, ok := h.(interface {
			HTTPFetchToCache(context.Context, FetchCacheRequest) (FetchCacheResult, error)
		})
		if !ok {
			return nil, ErrUnavailable
		}
		value, err = service.HTTPFetchToCache(ctx, FetchCacheRequest{
			FetchRequest: FetchRequest{TimeoutMS: req.TimeoutMS, Method: req.Method, URL: req.URL, Headers: req.Headers},
			LibraryKey:   req.LibraryKey,
			Name:         req.Name,
		})
	} else if name == "cache.rasterize" {
		if len(args) < 3 || len(args) > 4 {
			return nil, fmt.Errorf("plugins: cache.rasterize takes (libraryKey, name, sourceBlob[, options])")
		}
		var libraryKey, entryName, sourceBlobRef string
		var opts RasterizeOptions
		if err = json.Unmarshal(args[0], &libraryKey); err == nil {
			err = json.Unmarshal(args[1], &entryName)
		}
		if err == nil {
			err = json.Unmarshal(args[2], &sourceBlobRef)
		}
		if err == nil && len(args) == 4 && string(args[3]) != "null" {
			err = json.Unmarshal(args[3], &opts)
		}
		if err != nil {
			return nil, err
		}
		service, ok := h.(interface {
			CacheRasterize(context.Context, string, string, string, RasterizeOptions) (RasterizeResult, error)
		})
		if !ok {
			return nil, ErrUnavailable
		}
		value, err = service.CacheRasterize(ctx, libraryKey, entryName, sourceBlobRef, opts)
	} else {
		if name != "cache.read" && name != "archive.list" && name != "archive.open" && name != "archive.read" && name != "archive.extract" {
			return nil, ErrPermissionDenied
		}
		if len(args) == 0 {
			return nil, fmt.Errorf("plugins: data service needs a blob reference")
		}
		var ref string
		if err = json.Unmarshal(args[0], &ref); err != nil {
			return nil, err
		}
		var opt DataRange
		var reads []ArchiveReadItem
		var extractOpt ArchiveExtractOptions
		var member int
		switch name {
		case "archive.list":
			if len(args) != 1 {
				return nil, fmt.Errorf("plugins: archive.list takes one blob reference")
			}
		case "cache.read":
			if len(args) != 2 {
				return nil, fmt.Errorf("plugins: cache.read takes (blob, range)")
			}
			err = json.Unmarshal(args[1], &opt)
		case "archive.open":
			if len(args) != 3 {
				return nil, fmt.Errorf("plugins: archive.open takes (blob, memberIndex, range)")
			}
			if err = json.Unmarshal(args[1], &member); err == nil {
				err = json.Unmarshal(args[2], &opt)
			}
		case "archive.read":
			if len(args) != 2 {
				return nil, fmt.Errorf("plugins: archive.read takes (blob, reads)")
			}
			err = json.Unmarshal(args[1], &reads)
		case "archive.extract":
			if len(args) < 2 || len(args) > 3 {
				return nil, fmt.Errorf("plugins: archive.extract takes (blob, memberIndex[, options])")
			}
			err = json.Unmarshal(args[1], &member)
			if err == nil && len(args) == 3 && string(args[2]) != "null" {
				err = json.Unmarshal(args[2], &extractOpt)
			}
		}
		if err != nil {
			return nil, err
		}
		if name == "cache.read" || name == "archive.open" {
			if err = validDataRange(opt, readCap); err != nil {
				return nil, err
			}
		}
		if name == "archive.read" {
			if len(reads) == 0 || len(reads) > MaxArchiveReadItems {
				return nil, fmt.Errorf("plugins: archive.read takes 1..%d reads", MaxArchiveReadItems)
			}
			var totalRequested int64
			for _, item := range reads {
				if err = validDataRange(DataRange{Offset: item.Offset, Length: item.Length}, readCap); err != nil {
					return nil, err
				}
				if item.Length > readCap-totalRequested {
					return nil, fmt.Errorf("plugins: archive.read requested bytes exceed per-call readBytes limit (%d)", readCap)
				}
				totalRequested += item.Length
			}
			if err = wasm.ChargeUnits(ctx, int64(len(reads))*wasm.ArchiveReadUnitsPerMember); err != nil {
				return nil, err
			}
		}
		var defaultLibKey string
		if name == "archive.extract" {
			if checker, ok := h.(interface {
				archiveExtractCheck(context.Context, string) (string, error)
			}); ok {
				defaultLibKey, err = checker.archiveExtractCheck(ctx, extractOpt.LibraryKey)
				if err != nil {
					return nil, err
				}
			}
		}
		rc, e := h.CacheOpen(ctx, ref)
		if e != nil {
			return nil, e
		}
		defer rc.Close()
		if name == "cache.read" {
			if _, err = rc.Seek(opt.Offset, io.SeekStart); err != nil {
				return nil, err
			}
			sizeHint := min(opt.Length, int64(64<<10))
			if sizer, ok := rc.(interface{ Size() int64 }); ok {
				if rem := sizer.Size() - opt.Offset; rem >= 0 {
					sizeHint = min(opt.Length, rem)
				}
			}
			opt.Offset = 0
			value, err = readDataRange(ctx, rc, opt, readCap, sizeHint)
		} else {
			var archive *dataArchive
			var e error
			if owner, ok := h.(interface{ archiveDirectoryCache() *archiveDirectoryCache }); ok {
				archive, e = owner.archiveDirectoryCache().open(ctx, ref, rc, limits)
			} else {
				archive, e = openDataArchive(ctx, rc, limits)
			}
			if e != nil {
				return nil, e
			}
			switch name {
			case "archive.list":
				value = archive.members
			case "archive.open":
				var data []byte
				data, err = readArchiveMemberRange(ctx, archive, member, opt, readCap)
				if err != nil {
					return nil, err
				}
				value = map[string]any{"bodyBase64": base64.StdEncoding.EncodeToString(data), "size": len(data)}
			case "archive.read":
				out := make([]ArchiveReadResult, len(reads))
				for i, item := range reads {
					var data []byte
					data, err = readArchiveMemberRange(ctx, archive, item.Member, DataRange{Offset: item.Offset, Length: item.Length}, readCap)
					if err != nil {
						return nil, err
					}
					out[i] = ArchiveReadResult{
						Index:      i,
						Member:     item.Member,
						Size:       len(data),
						BodyBase64: base64.StdEncoding.EncodeToString(data),
					}
				}
				value = out
			case "archive.extract":
				if member < 0 || member >= len(archive.members) {
					return nil, fmt.Errorf("plugins: archive member index outside range")
				}
				f := archive.members[member]
				entryName := strings.TrimSpace(extractOpt.Name)
				if entryName == "" && !f.Unsafe {
					entryName = path.Base(strings.TrimSuffix(f.Name, "/"))
				}
				if entryName == "" || entryName == "." || entryName == "/" {
					entryName = fmt.Sprintf("member-%d", member)
				}
				sourceLibKey := ""
				if src, ok := rc.(interface{ LibraryKey() string }); ok {
					sourceLibKey = src.LibraryKey()
				}
				libraryKey := extractOpt.LibraryKey
				cacheLimit := int64(MaxDataFetchBytes)
				if resolver, ok := h.(interface {
					resolveExtractTarget(context.Context, string, string, string) (string, int64, error)
				}); ok {
					libraryKey, cacheLimit, err = resolver.resolveExtractTarget(ctx, extractOpt.LibraryKey, sourceLibKey, entryName)
					if err != nil {
						return nil, err
					}
				} else if libraryKey == "" {
					if sourceLibKey != "" {
						libraryKey = sourceLibKey
					} else {
						libraryKey = defaultLibKey
					}
				}
				if f.Size > uint64(limits.ArchiveMemberBytes) {
					return nil, fmt.Errorf("plugins: archive member exceeds size grant")
				}
				if f.Size > uint64(cacheLimit) {
					return nil, fmt.Errorf("%w: archive member (%d bytes) exceeds library cache quota (%d bytes)", ErrQuotaExceeded, f.Size, cacheLimit)
				}
				stream, e := archive.open(member)
				if e != nil {
					return nil, e
				}
				defer stream.Close()
				crc, md5Hash, sha1Hash := crc32.NewIEEE(), md5.New(), sha1.New()
				reader := &archiveExtractReader{
					ctx:         ctx,
					r:           stream,
					memberLimit: limits.ArchiveMemberBytes,
					cacheLimit:  cacheLimit,
					expected:    int64(f.Size),
				}
				cachedRef, e := h.CachePut(ctx, libraryKey, entryName, io.TeeReader(reader, io.MultiWriter(crc, md5Hash, sha1Hash)))
				if e != nil {
					return nil, e
				}
				value = ArchiveExtractResult{
					Ref:   cachedRef,
					Size:  reader.size,
					SHA1:  fmt.Sprintf("%x", sha1Hash.Sum(nil)),
					MD5:   fmt.Sprintf("%x", md5Hash.Sum(nil)),
					CRC32: fmt.Sprintf("%08x", crc.Sum32()),
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func defaultDataLimits() RuntimeLimits {
	return RuntimeLimits{
		MemoryBytes:          32 << 20,
		ReadBytes:            MinDataReadBytes,
		FetchBytes:           MaxDataFetchBytes,
		ArchiveMemberBytes:   maxArchiveMemberBytes,
		ArchiveExpandedBytes: maxArchiveExpandedBytes,
		ArchiveMembers:       maxArchiveMembers,
		MaxItems:             DefaultMaxPluginItems,
		Deadline:             "12h",
		PoolSize:             wasm.DefaultPoolSize,
	}
}
