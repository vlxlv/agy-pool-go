package cli

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var ansiSeqRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// StripANSI removes all ANSI escape sequences from s.
func StripANSI(s string) string {
	return ansiSeqRegex.ReplaceAllString(s, "")
}

// RuneWidth returns the visual column width of a single rune:
// 0 for control chars, zero-width spaces/combining marks.
// 2 for CJK and East Asian Wide / Fullwidth characters.
// 1 for normal single-column characters.
func RuneWidth(r rune) int {
	if r < 32 || (r >= 0x7f && r < 0xa0) {
		return 0
	}
	// Zero-width characters (zero-width spaces, joiners, BOM, soft hyphen)
	if r == 0x200B || r == 0x200C || r == 0x200D || r == 0x2060 || r == 0xFEFF || r == 0x00AD {
		return 0
	}
	// Combining diacritical marks
	if (r >= 0x0300 && r <= 0x036F) || (r >= 0x1DC0 && r <= 0x1DFF) || (r >= 0x20D0 && r <= 0x20FF) || (r >= 0xFE20 && r <= 0xFE2F) {
		return 0
	}
	// East Asian Wide / Fullwidth characters
	if (r >= 0x1100 && r <= 0x115F) || // Hangul Jamo
		(r >= 0x2E80 && r <= 0x9FFF) || // CJK Radicals, Kangxi, Ideographs
		(r >= 0xAC00 && r <= 0xD7A3) || // Hangul Syllables
		(r >= 0xF900 && r <= 0xFAFF) || // CJK Compatibility Ideographs
		(r >= 0xFE10 && r <= 0xFE19) || // Vertical forms
		(r >= 0xFE30 && r <= 0xFE6F) || // CJK Compatibility Forms
		(r >= 0xFF01 && r <= 0xFF60) || // Fullwidth ASCII & forms
		(r >= 0xFFE0 && r <= 0xFFE6) || // Fullwidth symbols
		(r >= 0x20000 && r <= 0x2FA1F) || // SIP
		(r >= 0x30000 && r <= 0x3134F) { // TIP
		return 2
	}
	return 1
}

// VisibleWidth returns the visual column width of s, ignoring ANSI escape sequences and accounting for CJK characters.
func VisibleWidth(s string) int {
	if s == "" {
		return 0
	}
	width := 0
	inEsc := false
	for i := 0; i < len(s); {
		if !inEsc && s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			i += 2
			continue
		}
		if inEsc {
			if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
				inEsc = false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		width += RuneWidth(r)
		i += size
	}
	return width
}

// TruncateVisible truncates s so its visible width is at most maxWidth.
// If s is truncated, it appends "…" and resets ANSI color state if ANSI was present.
// It never splits escape sequences.
func TruncateVisible(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	totalVisible := VisibleWidth(s)
	if totalVisible <= maxWidth {
		return s
	}

	// Budget for truncation: maxWidth - 1 for '…' (width 1)
	budget := maxWidth - 1
	if budget < 0 {
		budget = 0
	}

	var sb strings.Builder
	curWidth := 0
	inEsc := false
	hadANSI := false

	for i := 0; i < len(s); {
		if !inEsc && s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			hadANSI = true
			sb.WriteByte(s[i])
			sb.WriteByte(s[i+1])
			i += 2
			continue
		}
		if inEsc {
			sb.WriteByte(s[i])
			if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
				inEsc = false
			}
			i++
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		rw := RuneWidth(r)
		if curWidth+rw > budget {
			break
		}
		sb.WriteString(s[i : i+size])
		curWidth += rw
		i += size
	}

	sb.WriteString("…")
	if hadANSI {
		sb.WriteString(clrReset)
	}
	return sb.String()
}
