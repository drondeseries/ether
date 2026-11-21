package pluginitems

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// SortTitle derives the normalized lowercase sort key for a display title,
// matching catalog.SortTitle rules (spec §3.2): strips invisible format
// characters, removes a leading English article ("the ", "a ", "an "), and
// folds Latin accents/diacritics and ligatures to base ASCII letters.
func SortTitle(s string) string {
	l := strings.ToLower(strings.TrimSpace(stripFormatRunes(s)))
	for _, p := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(l, p) {
			if rest := strings.TrimSpace(l[len(p):]); rest != "" {
				l = rest
			}
			break
		}
	}
	return FoldSortTitle(l)
}

// FoldSortTitle normalizes an explicit sort title to lowercase with Latin
// diacritics and ligatures folded to their base ASCII letters.
func FoldSortTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(stripFormatRunes(s)))
	if s == "" || isASCII(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	lastLatin := false
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		if lastLatin && (unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Me, r)) {
			continue
		}
		switch r {
		case 'æ':
			b.WriteString("ae")
			lastLatin = true
			continue
		case 'œ':
			b.WriteString("oe")
			lastLatin = true
			continue
		case 'ß':
			b.WriteString("ss")
			lastLatin = true
			continue
		case 'ø':
			b.WriteByte('o')
			lastLatin = true
			continue
		case 'ł':
			b.WriteByte('l')
			lastLatin = true
			continue
		case 'đ', 'ð':
			b.WriteByte('d')
			lastLatin = true
			continue
		case 'þ':
			b.WriteString("th")
			lastLatin = true
			continue
		case 'ı':
			b.WriteByte('i')
			lastLatin = true
			continue
		}
		if unicode.Is(unicode.Latin, r) {
			lastLatin = true
			if r < 0x80 {
				b.WriteByte(byte(r))
				continue
			}
			for _, dr := range norm.NFD.String(string(r)) {
				if unicode.Is(unicode.Mn, dr) || unicode.Is(unicode.Mc, dr) || unicode.Is(unicode.Me, dr) {
					continue
				}
				b.WriteRune(dr)
			}
			continue
		}
		lastLatin = false
		b.WriteRune(r)
	}
	return b.String()
}

// foldFieldWords applies the same case, diacritic, ligature, and word-boundary
// folding used by plugin people search (experience.peopleFoldedWords).
func foldFieldWords(value string) []string {
	if value == "" {
		return nil
	}
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range norm.NFD.String(strings.ToLower(value)) {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		switch r {
		case 'ł':
			b.WriteByte('l')
		case 'ø':
			b.WriteByte('o')
		case 'đ', 'ð':
			b.WriteByte('d')
		case 'ß':
			b.WriteString("ss")
		case 'æ':
			b.WriteString("ae")
		case 'œ':
			b.WriteString("oe")
		case 'þ':
			b.WriteString("th")
		case 'ı':
			b.WriteByte('i')
		default:
			b.WriteRune(r)
		}
	}
	return strings.FieldsFunc(b.String(), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// FoldFieldText returns space-separated folded words for v_fold indexing on
// plugin_item_fields.
func FoldFieldText(value string) string {
	return strings.Join(foldFieldWords(value), " ")
}

func foldQueryTerms(value string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, word := range foldFieldWords(value) {
		if !seen[word] {
			seen[word] = true
			terms = append(terms, word)
			if len(terms) == 8 {
				break
			}
		}
	}
	return terms
}

func foldedWordsMatch(label string, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	words := foldFieldWords(label)
	if len(words) == 0 {
		return false
	}
	for _, term := range terms {
		found := false
		for _, w := range words {
			if strings.HasPrefix(w, term) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func appendFoldedWordPrefix(where *strings.Builder, args *[]any, terms []string) {
	if len(terms) == 0 {
		return
	}
	where.WriteString(` AND f.v_fold > ''`)
	for _, term := range terms {
		where.WriteString(` AND (f.v_fold LIKE ? OR f.v_fold LIKE ?)`)
		*args = append(*args, term+"%", "% "+term+"%")
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func stripFormatRunes(s string) string {
	if isASCII(s) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
