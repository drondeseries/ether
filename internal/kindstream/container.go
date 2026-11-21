package kindstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/braedonsaunders/ether/internal/archive"
	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
)

const maxArchiveDictBytes = 128 << 20

func wrapArchiveUnstreamable(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, core.ErrArchiveUnstreamable) {
		return err
	}
	if errors.Is(err, archive.ErrEncrypted) || errors.Is(err, archive.ErrUnsupported) {
		return fmt.Errorf("%w: %w", ErrMemberUndecodable, err)
	}
	return err
}

// OpenContainer opens an archive container (RAR4/RAR5/7z/ZIP) over one or more
// VolumeRefs and returns a ByteSource for the requested inner member (spec §5.3).
//
// Store-mode members (including encrypted store-mode RAR/7z when PasswordSecret
// is supplied) are served directly via archive.NewMemberReader without
// extracting the archive. Compressed ZIP, 7z (single-coder LZMA/LZMA2), RAR4,
// and RAR5 members are sequentially decoded into deps.Cache via ExtractMember.
// Encrypted non-stored members and unsupported archive formats/methods fail with
// ErrMemberUndecodable (wrapping core.ErrArchiveUnstreamable).
func OpenContainer(ctx context.Context, c core.ContainerRef, declaredMIME string, maxBytes int64, deps Deps) (ByteSource, error) {
	if len(c.Volumes) == 0 {
		return nil, errors.New("kindstream: container has no volumes")
	}

	var password string
	if strings.TrimSpace(c.PasswordSecret) != "" {
		if deps.Secret == nil {
			return nil, fmt.Errorf("kindstream: container requires password secret %q but no secret provider configured", c.PasswordSecret)
		}
		pw, err := deps.Secret(ctx, strings.TrimSpace(c.PasswordSecret))
		if err != nil {
			return nil, fmt.Errorf("kindstream: resolve container password %q: %w", c.PasswordSecret, err)
		}
		password = pw
	}

	vols := make([]archive.Volume, 0, len(c.Volumes))
	var closers []io.Closer
	closeAll := func() {
		for _, cl := range closers {
			_ = cl.Close()
		}
	}

	for i, vref := range c.Volumes {
		volSrc, err := openVolumeRef(ctx, vref, maxBytes, deps)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("kindstream: open container volume %d: %w", i, err)
		}
		closers = append(closers, volSrc)
		vols = append(vols, archive.Volume{
			R:    newSeqBufferedReaderAt(ctx, volSrc, volSrc.Size()),
			Size: volSrc.Size(),
		})
	}

	format, err := detectContainerFormat(vols[0], c.Format)
	if err != nil {
		closeAll()
		return nil, wrapArchiveUnstreamable(err)
	}

	members, err := listContainerMembers(ctx, format, vols, password)
	if err != nil {
		closeAll()
		return nil, wrapArchiveUnstreamable(err)
	}

	selected, nonDirIdx, err := selectArchiveMember(members, c.Member, c.MemberIndex)
	if err != nil {
		closeAll()
		return nil, err
	}

	streamErr := selected.Streamable()
	if streamErr == nil {
		// Rebind direct extent reads to raw volume readers so random seeks into
		// store-mode members do not share sequential buffer state across callers.
		rawVols := make([]archive.Volume, len(vols))
		for i, cl := range closers {
			if ra, ok := cl.(io.ReaderAt); ok {
				rawVols[i] = archive.Volume{R: ra, Size: vols[i].Size}
			} else {
				rawVols[i] = vols[i]
			}
		}
		mr, err := archive.NewMemberReader(selected, rawVols)
		if err != nil {
			closeAll()
			return nil, wrapArchiveUnstreamable(err)
		}
		headLen := min(selected.Size, int64(probeHeadBytes))
		var head []byte
		if headLen > 0 {
			head = make([]byte, headLen)
			n, rerr := mr.ReadAt(head, 0)
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				closeAll()
				return nil, fmt.Errorf("kindstream: read member head: %w", rerr)
			}
			head = head[:n]
		}
		resolvedMIME, err := verifyAndResolveMIME(head, declaredMIME, selected.Name)
		if err != nil {
			closeAll()
			return nil, err
		}
		return &containerMemberSource{
			r:       mr,
			size:    selected.Size,
			name:    memberDisplayName(selected.Name),
			mime:    resolvedMIME,
			closers: closers,
		}, nil
	}

	if errors.Is(streamErr, archive.ErrCompressed) || errors.Is(streamErr, archive.ErrSolid) {
		defer closeAll()
		return ExtractMember(ctx, c, format, selected, nonDirIdx, declaredMIME, maxBytes, deps)
	}

	closeAll()
	return nil, wrapArchiveUnstreamable(streamErr)
}

// ExtractMember sequentially decodes the target member from a compressed or
// solid archive into deps.Cache and returns a random-access ByteSource over the
// extracted file.
//
// Compressed ZIP, non-solid 7z, RAR4, and RAR5 volumes are decoded directly
// over ranged volume readers without caching whole volumes first. Solid 7z
// archives over remote URL volumes cache the volume first via ensureVolumeCached,
// honouring ctx and the library cache quota. Encrypted non-stored members and
// unsupported archive formats/methods fail with ErrMemberUndecodable.
func ExtractMember(
	ctx context.Context,
	c core.ContainerRef,
	format archive.Format,
	selected archive.Member,
	nonDirIdx int,
	declaredMIME string,
	maxBytes int64,
	deps Deps,
) (ByteSource, error) {
	if selected.Encrypted {
		return nil, fmt.Errorf("%w: encrypted %s member %q", ErrMemberUndecodable, format, selected.Name)
	}
	switch format {
	case archive.FormatZIP, archive.Format7z, archive.FormatRAR4, archive.FormatRAR5:
	default:
		return nil, fmt.Errorf("%w: %s member %q (method %s)", ErrMemberUndecodable, format, selected.Name, selected.Method)
	}
	if format == archive.FormatZIP && selected.Method != "deflate" && selected.Method != "store" {
		return nil, fmt.Errorf("%w: %s member %q (method %s)", ErrMemberUndecodable, format, selected.Name, selected.Method)
	}
	if deps.Cache == nil {
		return nil, fmt.Errorf("%w: blobcache namespace required to extract compressed member %q", ErrMemberUndecodable, selected.Name)
	}
	limit := effectiveDownloadMaxBytes(maxBytes, deps)
	if limit > 0 && selected.Size > limit {
		return nil, fmt.Errorf("%w: member %q unpacked size %d exceeds limit %d", ErrSizeLimitExceeded, selected.Name, selected.Size, limit)
	}

	matchFn := makeMemberMatchFn(c.Member, selected.Name, nonDirIdx)
	extractKey := containerExtractCacheKey(c, selected.Name, nonDirIdx, deps.ItemIndex)
	ref, err := deps.Cache.SingleFlightPut(ctx, extractKey, func(ctx context.Context) (blobcache.Ref, error) {
		var closers []io.Closer
		closeVols := func() {
			for _, cl := range closers {
				_ = cl.Close()
			}
			closers = nil
		}
		defer closeVols()

		vols := make([]archive.Volume, 0, len(c.Volumes))
		for i, vref := range c.Volumes {
			volSrc, err := openVolumeRef(ctx, vref, maxBytes, deps)
			if err != nil {
				return "", fmt.Errorf("kindstream: open container volume %d: %w", i, err)
			}
			closers = append(closers, volSrc)
			vols = append(vols, archive.Volume{
				R:    newSeqBufferedReaderAt(ctx, volSrc, volSrc.Size()),
				Size: volSrc.Size(),
			})
		}

		if format == archive.Format7z {
			solid, err := archive.Inspect7zMember(ctx, vols, matchFn, maxArchiveDictBytes, limit)
			if err != nil {
				return "", wrapExtractErr(err)
			}
			if solid && hasRemoteVolume(c.Volumes) {
				closeVols()
				vols = vols[:0]
				for i, vref := range c.Volumes {
					localSrc, err := ensureVolumeCached(ctx, vref, maxBytes, deps)
					if err != nil {
						return "", fmt.Errorf("kindstream: cache solid 7z volume %d: %w", i, err)
					}
					closers = append(closers, localSrc)
					vols = append(vols, archive.Volume{
						R:    localSrc,
						Size: localSrc.Size(),
					})
				}
			}
		}

		rc, m, err := openCompressedArchiveMember(ctx, format, vols, matchFn, limit)
		if err != nil {
			return "", wrapExtractErr(err)
		}
		defer rc.Close()

		var reader io.Reader = rc
		if limit > 0 {
			reader = &limitEnforcingReader{r: rc, max: limit}
		}
		ref, err := deps.Cache.PutWithExpectedSHA256(ctx, extractKey, reader, m.Size, "", declaredMIME)
		if err != nil {
			if errors.Is(err, blobcache.ErrTooLarge) {
				return "", fmt.Errorf("%w: %v", ErrSizeLimitExceeded, err)
			}
			return "", err
		}
		return ref, nil
	})
	if err != nil {
		return nil, err
	}

	return finalizeExtractedMember(ref, selected.Name, declaredMIME, deps)
}

func extractFromOpenVolumes(
	ctx context.Context,
	vols []archive.Volume,
	cacheSeed string,
	format archive.Format,
	wantMember string,
	selected archive.Member,
	nonDirIdx int,
	declaredMIME string,
	maxBytes int64,
	digests expectedDigests,
	deps Deps,
) (ByteSource, error) {
	if selected.Encrypted {
		return nil, fmt.Errorf("%w: encrypted %s member %q", ErrMemberUndecodable, format, selected.Name)
	}
	switch format {
	case archive.FormatZIP, archive.Format7z, archive.FormatRAR4, archive.FormatRAR5:
	default:
		return nil, fmt.Errorf("%w: %s member %q (method %s)", ErrMemberUndecodable, format, selected.Name, selected.Method)
	}
	if deps.Cache == nil {
		return nil, fmt.Errorf("%w: blobcache namespace required to extract compressed member %q", ErrMemberUndecodable, selected.Name)
	}
	limit := effectiveDownloadMaxBytes(maxBytes, deps)
	if limit > 0 && selected.Size > limit {
		return nil, fmt.Errorf("%w: member %q unpacked size %d exceeds limit %d", ErrSizeLimitExceeded, selected.Name, selected.Size, limit)
	}

	matchFn := makeMemberMatchFn(wantMember, selected.Name, nonDirIdx)
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|member=%s|idx=%d|item=%d", cacheSeed, normalizeArchivePath(selected.Name), nonDirIdx, deps.ItemIndex)))
	extractKey := "extract-" + hex.EncodeToString(h[:12]) + "-" + memberDisplayName(selected.Name)

	ref, err := deps.Cache.SingleFlightPut(ctx, extractKey, func(ctx context.Context) (blobcache.Ref, error) {
		bufVols := make([]archive.Volume, len(vols))
		for i, v := range vols {
			bufVols[i] = archive.Volume{
				R:    newSeqBufferedReaderAt(ctx, v.R, v.Size),
				Size: v.Size,
			}
		}
		rc, m, err := openCompressedArchiveMember(ctx, format, bufVols, matchFn, limit)
		if err != nil {
			return "", wrapExtractErr(err)
		}
		defer rc.Close()

		var reader io.Reader = rc
		if limit > 0 {
			reader = &limitEnforcingReader{r: rc, max: limit}
		}
		ref, err := deps.Cache.PutWithExpectedSHA256(ctx, extractKey, newDigestVerifyReader(reader, digests), m.Size, digests.sha256, declaredMIME)
		if err != nil {
			if errors.Is(err, blobcache.ErrTooLarge) {
				return "", fmt.Errorf("%w: %v", ErrSizeLimitExceeded, err)
			}
			return "", err
		}
		return ref, nil
	})
	if err != nil {
		return nil, err
	}
	return finalizeExtractedMember(ref, selected.Name, declaredMIME, deps)
}

func finalizeExtractedMember(ref blobcache.Ref, memberName, declaredMIME string, deps Deps) (ByteSource, error) {
	cachedSrc, err := deps.Cache.Open(ref)
	if err != nil {
		return nil, err
	}
	headLen := min(cachedSrc.Size(), int64(probeHeadBytes))
	var head []byte
	if headLen > 0 {
		head = make([]byte, headLen)
		n, rerr := cachedSrc.ReadAt(head, 0)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			_ = cachedSrc.Close()
			return nil, rerr
		}
		head = head[:n]
	}
	resolvedMIME, err := verifyAndResolveMIME(head, declaredMIME, memberName)
	if err != nil {
		_ = cachedSrc.Close()
		return nil, err
	}
	return &renamedByteSource{
		ByteSource: cachedSrc,
		name:       memberDisplayName(memberName),
		mime:       resolvedMIME,
	}, nil
}

func makeMemberMatchFn(wantMember, selectedName string, nonDirIdx int) func(int, archive.Member) bool {
	normSelected := normalizeArchivePath(selectedName)
	hasExplicit := strings.TrimSpace(wantMember) != ""
	return func(idx int, cand archive.Member) bool {
		if hasExplicit {
			return normalizeArchivePath(cand.Name) == normSelected
		}
		return idx == nonDirIdx
	}
}

func hasRemoteVolume(vols []core.VolumeRef) bool {
	for _, v := range vols {
		if strings.TrimSpace(v.BlobRef) == "" {
			return true
		}
	}
	return false
}

func openCompressedArchiveMember(
	ctx context.Context,
	format archive.Format,
	vols []archive.Volume,
	matchFn func(int, archive.Member) bool,
	maxExpandedBytes int64,
) (io.ReadCloser, archive.Member, error) {
	switch format {
	case archive.FormatZIP:
		return archive.OpenZIPMember(vols, matchFn)
	case archive.Format7z:
		return archive.Open7zMember(ctx, vols, matchFn, maxArchiveDictBytes, maxExpandedBytes)
	case archive.FormatRAR4, archive.FormatRAR5:
		return archive.OpenRARMember(ctx, vols, matchFn, maxArchiveDictBytes, maxExpandedBytes)
	default:
		return nil, archive.Member{}, fmt.Errorf("%w: unsupported archive format %s", ErrMemberUndecodable, format)
	}
}

func wrapExtractErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, core.ErrArchiveUnstreamable) {
		return err
	}
	if errors.Is(err, archive.ErrEncrypted) ||
		errors.Is(err, archive.ErrUnsupported) ||
		errors.Is(err, archive.ErrCompressed) ||
		errors.Is(err, archive.ErrSolid) {
		return fmt.Errorf("%w: %w", ErrMemberUndecodable, err)
	}
	return err
}

func openVolumeRef(ctx context.Context, v core.VolumeRef, maxBytes int64, deps Deps) (ByteSource, error) {
	if strings.TrimSpace(v.BlobRef) != "" {
		if deps.Cache == nil {
			return nil, errors.New("kindstream: volume references blobRef but Cache is nil")
		}
		src, err := deps.Cache.Open(blobcache.Ref(strings.TrimSpace(v.BlobRef)))
		if err != nil {
			return nil, err
		}
		if v.Size > 0 && src.Size() != v.Size {
			_ = src.Close()
			return nil, fmt.Errorf("kindstream: cached volume size %d != declared %d", src.Size(), v.Size)
		}
		return src, nil
	}
	if strings.TrimSpace(v.URL) == "" {
		return nil, errors.New("kindstream: volume has neither url nor blobRef")
	}

	tmpl, err := ParseURLTemplate(v.URL, deps.AllowHost)
	if err != nil {
		return nil, err
	}
	hdrs, err := ExpandHeaders(ctx, v.Headers, deps.Secret)
	if err != nil {
		return nil, err
	}
	hr, err := newHTTPReaderAt(ctx, httpReaderConfig{
		Template:  tmpl,
		Headers:   hdrs,
		ItemIndex: deps.ItemIndex,
		KnownSize: v.Size,
		MIME:      "application/octet-stream",
		Client:    deps.HTTP,
		AllowHost: deps.AllowHost,
		Backoff:   deps.Backoff,
	})
	if err == nil {
		return hr, nil
	}
	var noRange *noRangeSupportError
	if errors.As(err, &noRange) {
		defer noRange.closeBody()
		if deps.Cache == nil {
			return nil, err
		}
		return downloadFromNoRangeResponse(ctx, v.URL, noRange, expectedDigests{}, "application/octet-stream", maxBytes, deps)
	}
	return nil, err
}

func ensureVolumeCached(ctx context.Context, v core.VolumeRef, maxBytes int64, deps Deps) (ByteSource, error) {
	if strings.TrimSpace(v.BlobRef) != "" {
		return deps.Cache.Open(blobcache.Ref(strings.TrimSpace(v.BlobRef)))
	}
	ps := core.PlayableSource{
		Pattern:  PatternDownload,
		URL:      v.URL,
		Headers:  v.Headers,
		Size:     v.Size,
		MaxBytes: maxBytes,
		MIME:     "application/octet-stream",
	}
	return openDownloadSource(ctx, ps, deps)
}

func detectContainerFormat(vol0 archive.Volume, hint string) (archive.Format, error) {
	if vol0.Size < archive.SignatureLen {
		return archive.FormatUnknown, fmt.Errorf("kindstream: volume 0 too short (%d bytes): %w", vol0.Size, archive.ErrNotArchive)
	}
	var head [archive.SignatureLen]byte
	n, err := vol0.R.ReadAt(head[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return archive.FormatUnknown, fmt.Errorf("kindstream: read volume 0 signature: %w", err)
	}
	detected := archive.Detect(head[:n])
	hint = strings.ToLower(strings.TrimSpace(hint))
	switch hint {
	case "":
		if detected == archive.FormatUnknown {
			return archive.FormatUnknown, archive.ErrNotArchive
		}
		return detected, nil
	case "rar", "cbr":
		if detected == archive.FormatRAR4 || detected == archive.FormatRAR5 {
			return detected, nil
		}
		return archive.FormatUnknown, fmt.Errorf("kindstream: expected rar container, detected %s: %w", detected, archive.ErrNotArchive)
	case "rar4":
		if detected != archive.FormatRAR4 {
			return archive.FormatUnknown, fmt.Errorf("kindstream: expected rar4 container, detected %s: %w", detected, archive.ErrNotArchive)
		}
		return detected, nil
	case "rar5":
		if detected != archive.FormatRAR5 {
			return archive.FormatUnknown, fmt.Errorf("kindstream: expected rar5 container, detected %s: %w", detected, archive.ErrNotArchive)
		}
		return detected, nil
	case "7z", "cb7":
		if detected != archive.Format7z {
			return archive.FormatUnknown, fmt.Errorf("kindstream: expected 7z container, detected %s: %w", detected, archive.ErrNotArchive)
		}
		return detected, nil
	case "zip", "cbz", "epub":
		if detected != archive.FormatZIP {
			return archive.FormatUnknown, fmt.Errorf("kindstream: expected zip container, detected %s: %w", detected, archive.ErrNotArchive)
		}
		return detected, nil
	default:
		return archive.FormatUnknown, fmt.Errorf("%w: unsupported container format %q: %w", ErrMemberUndecodable, hint, archive.ErrUnsupported)
	}
}

func listContainerMembers(ctx context.Context, format archive.Format, vols []archive.Volume, password string) ([]archive.Member, error) {
	switch format {
	case archive.FormatRAR4, archive.FormatRAR5:
		parsed := make([]*archive.RARVolume, len(vols))
		opts := &archive.RAROptions{
			Context:  ctx,
			Password: password,
		}
		for i, v := range vols {
			rv, err := archive.ReadRARVolume(v.R, v.Size, opts)
			if err != nil {
				return nil, fmt.Errorf("kindstream: read rar volume %d: %w", i, wrapArchiveUnstreamable(err))
			}
			parsed[i] = rv
		}
		members, err := archive.ComposeRAR(parsed)
		if err != nil {
			return nil, wrapArchiveUnstreamable(err)
		}
		return members, nil

	case archive.Format7z:
		mr := archive.NewMultiReader(vols)
		members, err := archive.List7zWithPassword(ctx, mr, mr.Size(), password)
		if err != nil {
			return nil, wrapArchiveUnstreamable(err)
		}
		for i := range members {
			if len(members[i].Extents) > 0 {
				exts, err := archive.SplitExtents(vols, members[i].Extents)
				if err != nil {
					return nil, err
				}
				members[i].Extents = exts
			}
		}
		return members, nil

	case archive.FormatZIP:
		members, err := archive.ListZIP(vols)
		if err != nil {
			return nil, wrapArchiveUnstreamable(err)
		}
		for i := range members {
			if len(members[i].Extents) > 0 {
				exts, err := archive.SplitExtents(vols, members[i].Extents)
				if err != nil {
					return nil, err
				}
				members[i].Extents = exts
			}
		}
		return members, nil

	default:
		return nil, archive.ErrNotArchive
	}
}

func selectArchiveMember(members []archive.Member, wantName string, wantIndex *int) (archive.Member, int, error) {
	var nonDir []archive.Member
	for _, m := range members {
		if !m.Dir {
			nonDir = append(nonDir, m)
		}
	}
	if len(nonDir) == 0 {
		return archive.Member{}, -1, fmt.Errorf("kindstream: archive contains no non-directory members: %w", archive.ErrCorrupt)
	}

	wantName = normalizeArchivePath(wantName)
	if wantName != "" {
		for idx, m := range nonDir {
			if normalizeArchivePath(m.Name) == wantName {
				return m, idx, nil
			}
		}
		// Fallback to unique basename match when wantName has no directory separator.
		if !strings.Contains(wantName, "/") {
			matchIdx := -1
			for idx, m := range nonDir {
				if path.Base(normalizeArchivePath(m.Name)) == wantName {
					if matchIdx >= 0 {
						matchIdx = -2
						break
					}
					matchIdx = idx
				}
			}
			if matchIdx >= 0 {
				return nonDir[matchIdx], matchIdx, nil
			}
		}
		return archive.Member{}, -1, fmt.Errorf("kindstream: archive member %q not found: %w", wantName, ErrPluginAnswer)
	}

	if wantIndex != nil {
		idx := *wantIndex
		if idx < 0 || idx >= len(nonDir) {
			return archive.Member{}, -1, fmt.Errorf("kindstream: archive memberIndex %d out of range [0,%d): %w", idx, len(nonDir), ErrPluginAnswer)
		}
		return nonDir[idx], idx, nil
	}

	return nonDir[0], 0, nil
}

func normalizeArchivePath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return p
}

func memberDisplayName(p string) string {
	norm := normalizeArchivePath(p)
	base := path.Base(norm)
	if base == "" || base == "." || base == "/" {
		return "member"
	}
	return base
}

func containerExtractCacheKey(c core.ContainerRef, memberName string, nonDirIdx, itemIdx int) string {
	h := sha256.New()
	for _, v := range c.Volumes {
		_, _ = io.WriteString(h, v.BlobRef)
		_, _ = io.WriteString(h, "|")
		_, _ = io.WriteString(h, v.URL)
		_, _ = io.WriteString(h, "\n")
	}
	_, _ = fmt.Fprintf(h, "member=%s|idx=%d|item=%d", normalizeArchivePath(memberName), nonDirIdx, itemIdx)
	return "extract-" + hex.EncodeToString(h.Sum(nil))[:24] + "-" + memberDisplayName(memberName)
}

type containerMemberSource struct {
	r       *archive.MemberReader
	size    int64
	name    string
	mime    string
	closers []io.Closer
}

func (s *containerMemberSource) ReadAt(p []byte, off int64) (int, error) {
	return s.r.ReadAt(p, off)
}

func (s *containerMemberSource) Size() int64  { return s.size }
func (s *containerMemberSource) Name() string { return s.name }
func (s *containerMemberSource) MIME() string { return s.mime }

func (s *containerMemberSource) Close() error {
	var firstErr error
	for _, c := range s.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.closers = nil
	return firstErr
}
