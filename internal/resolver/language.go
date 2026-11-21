package resolver

import (
	"fmt"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// catalogLanguage is the server's language: the catalogue metadata language.
func (s *Service) catalogLanguage() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.catalogLang
}

// profileLanguageMode reads a profile's Language (Radarr-style): specific
// is the one language its releases must be in ("" for none), any accepts
// every language, and neither is the original behaviour — each title's
// original language alongside the preferred ones. A language Ether does
// not recognise keeps the original behaviour rather than rejecting
// everything.
func profileLanguageMode(profile core.QualityProfile) (specific string, anyLang bool) {
	switch strings.ToLower(strings.TrimSpace(profile.Language)) {
	case "", core.ProfileLanguageOriginal:
		return "", false
	case core.ProfileLanguageAny:
		return "", true
	}
	if releaseparse.KnownLanguage(profile.Language) {
		return releaseparse.LanguageName(profile.Language), false
	}
	return "", false
}

// profileLanguageRejection is the reason a release fails a profile's
// specific Language, or "" when it passes (or the profile sets none). A
// release declaring no audio is in the title's original language as far as
// anyone knows, so it passes when that is the profile's language or
// unknown; MULTI and DUAL carry more than one track and pass, as in
// audioAcceptable.
func profileLanguageRejection(info core.ReleaseInfo, profile core.QualityProfile, t *core.Title) string {
	lang, _ := profileLanguageMode(profile)
	if lang == "" {
		return ""
	}
	if len(info.Languages) == 0 {
		if orig := originalLanguage(t); orig == "" || releaseparse.LanguageMatch(orig, lang) != releaseparse.LanguageNoMatch {
			return ""
		}
		return fmt.Sprintf("audio is not in the profile's language (%s)", lang)
	}
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti || l == releaseparse.LanguageDual {
			return ""
		}
		if releaseparse.LanguageMatch(l, lang) != releaseparse.LanguageNoMatch {
			return ""
		}
	}
	return fmt.Sprintf("audio is not in the profile's language (%s)", lang)
}

// wantedAudio lists the viewer's own preferred audio languages: the profile's
// language when it names one, else its preferred languages when the owner
// set them, otherwise the viewer's own audio languages, otherwise the
// server's catalogue language.
func wantedAudio(profile core.QualityProfile, user *core.User, serverLang string) []string {
	if lang, _ := profileLanguageMode(profile); lang != "" {
		return []string{lang}
	}
	var out []string
	switch {
	case len(profile.PreferredLanguages) > 0:
		out = append(out, profile.PreferredLanguages...)
	case user != nil && len(user.AudioLanguages) > 0:
		out = append(out, user.AudioLanguages...)
	case strings.TrimSpace(serverLang) != "":
		base, _, _ := strings.Cut(strings.TrimSpace(serverLang), "-")
		out = append(out, base)
	}
	return out
}

// movieTargetLanguages returns the ordered audio languages and original
// language for multi-file movie pack file selection. An explicit "Original"
// profile language puts the title's original language ahead of viewer/server
// fallbacks; otherwise wanted audio languages lead and the original language
// trails as a fallback over unrelated dubs.
func movieTargetLanguages(profile core.QualityProfile, wanted []string, t *core.Title) ([]string, string) {
	orig := titleOriginalLanguage(t)
	var out []string
	add := func(v string) {
		if n := releaseparse.LanguageName(v); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if strings.EqualFold(strings.TrimSpace(profile.Language), core.ProfileLanguageOriginal) && orig != "" && len(profile.PreferredLanguages) == 0 {
		add(orig)
	}
	for _, w := range wanted {
		add(w)
	}
	if specific, _ := profileLanguageMode(profile); specific == "" && orig != "" {
		add(orig)
	}
	return out, releaseparse.LanguageName(orig)
}

// acceptedAudio lists the audio languages a release may declare and still
// rank with the rest: the profile's languages when the owner set them,
// otherwise the viewer's own audio languages, otherwise the server's
// language; the title's original language is always accepted. A profile
// language of Any accepts everything; a specific profile language accepts
// only itself (profileLanguageRejection is the hard gate). An empty
// result accepts everything (nothing is known to ask for).
func acceptedAudio(profile core.QualityProfile, user *core.User, serverLang string, t *core.Title) []string {
	if lang, anyLang := profileLanguageMode(profile); anyLang {
		return nil
	} else if lang != "" {
		return []string{lang}
	}
	out := wantedAudio(profile, user, serverLang)
	if len(out) == 0 {
		return nil
	}
	if lang := titleOriginalLanguage(t); lang != "" {
		out = append(out, lang)
	}
	return out
}

// audioAcceptable reports whether a release's declared audio fits the
// accepted languages. A release that declares nothing is in the title's
// original language as far as anyone knows, so it is acceptable when that
// language is; MULTI / DUAL carry more than one track and are acceptable;
// so is a regional variant of an accepted language (Castilian for a Latino
// preference).
func audioAcceptable(info core.ReleaseInfo, accepted []string, t *core.Title) bool {
	if len(accepted) == 0 {
		return true
	}
	if len(info.Languages) == 0 {
		// An untagged release is presumed to carry the title's original
		// language. When the profile accepts only other languages (an
		// English-dub profile on an anime), it is not acceptable, so a
		// declared dub outranks it at any resolution; it still plays as a
		// fallback when no acceptable release exists.
		orig := titleOriginalLanguage(t)
		if orig == "" {
			return true
		}
		for _, want := range accepted {
			if releaseparse.LanguageMatch(orig, want) != releaseparse.LanguageNoMatch {
				return true
			}
		}
		return false
	}
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti || l == releaseparse.LanguageDual {
			return true
		}
		for _, want := range accepted {
			if releaseparse.LanguageMatch(l, want) != releaseparse.LanguageNoMatch {
				return true
			}
		}
	}
	return false
}

// foreignTitleWantedAudio reports whether a release of a non-wanted-language
// title explicitly declares the viewer's wanted audio language (or MULTI).
// When the title's original language is already one of the viewer's wanted
// languages (or unknown), untagged releases are already in the viewer's
// language and this returns false for all candidates so their relative order
// is unchanged. On a foreign-language title (e.g. a Spanish film for an
// English viewer), releases that declare English or MULTI are ranked ahead of
// untagged original-language or bare .DUAL releases within the same resolution
// tier. A bare DUAL/MULTI from an indexer that declares the viewer's wanted
// language (sourceLang) carries that dub, so it counts too.
func foreignTitleWantedAudio(info core.ReleaseInfo, sourceLang string, wanted []string, t *core.Title) bool {
	orig := titleOriginalLanguage(t)
	if orig == "" || len(wanted) == 0 {
		return false
	}
	for _, w := range wanted {
		if releaseparse.LanguageMatch(orig, w) != releaseparse.LanguageNoMatch {
			return false
		}
	}
	// The source language fills a bare DUAL/MULTI's unnamed dub.
	sourceWanted := false
	for _, w := range wanted {
		if releaseparse.LanguageMatch(sourceLang, w) != releaseparse.LanguageNoMatch {
			sourceWanted = true
			break
		}
	}
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti {
			return true
		}
		if l == releaseparse.LanguageDual {
			if sourceWanted {
				return true
			}
			continue
		}
		for _, w := range wanted {
			if releaseparse.LanguageMatch(l, w) != releaseparse.LanguageNoMatch {
				return true
			}
		}
	}
	return false
}

// localizedEpisodeTitle reports a release dubbed into a language that is
// neither the catalogue's nor the title's original: its name carries the
// episode title in that language ("S06E06.La.resa.dei.conti.ITA.ENG"), so
// a different title there is a translation, not another episode.
func localizedEpisodeTitle(info core.ReleaseInfo, t *core.Title, catalogLang string) bool {
	base, _, _ := strings.Cut(strings.TrimSpace(catalogLang), "-")
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti || l == releaseparse.LanguageDual {
			continue
		}
		if base != "" && releaseparse.LanguageMatch(l, base) != releaseparse.LanguageNoMatch {
			continue
		}
		if orig := titleOriginalLanguage(t); orig != "" && releaseparse.LanguageMatch(l, orig) != releaseparse.LanguageNoMatch {
			continue
		}
		return true
	}
	return false
}

// matchesAlternativeTitle reports a release named by another of the title's
// names: a romanized original ("Sousou.no.Frieren"), a regional or working
// title. The year, episode and identifier checks still apply.
func matchesAlternativeTitle(info core.ReleaseInfo, alts []string) bool {
	for _, alt := range alts {
		if releaseparse.TitleMatches(info, alt) {
			return true
		}
	}
	return false
}
