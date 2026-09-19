package cli

import (
	"strings"
	"testing"
)

func TestWidth_RuneWidth(t *testing.T) {
	cases := []struct {
		r    rune
		want int
	}{
		{'a', 1},
		{'Z', 1},
		{'1', 1},
		{'-', 1},
		{' ', 1},
		{'中', 2},
		{'文', 2},
		{'日', 2},
		{'本', 2},
		{'語', 2},
		{'한', 2},
		{'글', 2},
		{'，', 2},    // Fullwidth comma
		{'！', 2},    // Fullwidth exclamation
		{'【', 2},    // Fullwidth bracket
		{0x200B, 0}, // Zero-width space
		{0x00, 0},   // Null
		{0x1b, 0},   // ESC
	}

	for _, c := range cases {
		got := RuneWidth(c.r)
		if got != c.want {
			t.Errorf("RuneWidth(%c / U+%04X) = %d, want %d", c.r, c.r, got, c.want)
		}
	}
}

func TestWidth_VisibleWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 5},
		{"\033[1mhello\033[0m", 5},
		{"\033[32m\033[1mhello world\033[0m", 11},
		{"中文", 4},
		{"\033[34m中文测试\033[0m", 8},
		{"Main · CLI Base", 15},
		{"\033[1mMain\033[0m · \033[32mCLI Base\033[0m", 15},
		{"Main账号", 8},
	}

	for _, c := range cases {
		got := VisibleWidth(c.in)
		if got != c.want {
			t.Errorf("VisibleWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestWidth_TruncateVisible(t *testing.T) {
	// 1. Plain ASCII truncation
	s1 := "Hello World"
	got1 := TruncateVisible(s1, 8)
	if VisibleWidth(got1) > 8 {
		t.Errorf("TruncateVisible(%q, 8) exceeded width 8: %q (w=%d)", s1, got1, VisibleWidth(got1))
	}
	if !strings.HasSuffix(got1, "…") {
		t.Errorf("expected '…' suffix on truncated string, got %q", got1)
	}

	// 2. CJK truncation (wide characters must not be partially split)
	s2 := "谷歌主账号开发环境"
	got2 := TruncateVisible(s2, 10)
	w2 := VisibleWidth(got2)
	if w2 > 10 {
		t.Errorf("TruncateVisible(%q, 10) exceeded width 10: %q (w=%d)", s2, got2, w2)
	}
	if !strings.HasSuffix(got2, "…") {
		t.Errorf("expected '…' suffix on %q", got2)
	}

	// 3. Colored string truncation: must NOT split escape sequences and MUST append reset code
	s3 := "\033[32mSuperLongColoredAccountName\033[0m"
	got3 := TruncateVisible(s3, 12)
	w3 := VisibleWidth(got3)
	if w3 > 12 {
		t.Errorf("TruncateVisible colored string exceeded width 12: %q (w=%d)", got3, w3)
	}
	if !strings.HasSuffix(got3, clrReset) {
		t.Errorf("expected truncated colored string to end with ANSI reset %q, got %q", clrReset, got3)
	}
	if !strings.Contains(got3, "…") {
		t.Errorf("expected '…' in truncated colored string, got %q", got3)
	}

	// 4. String within budget should not be truncated
	s4 := "Short"
	got4 := TruncateVisible(s4, 10)
	if got4 != s4 {
		t.Errorf("TruncateVisible(%q, 10) = %q, want %q", s4, got4, s4)
	}

	// 5. Zero or negative budget
	if got := TruncateVisible("test", 0); got != "" {
		t.Errorf("TruncateVisible with 0 width want empty string, got %q", got)
	}
}

func TestWidth_StripANSI(t *testing.T) {
	colored := "\033[1m\033[32mHello\033[0m \033[33mWorld\033[0m"
	plain := StripANSI(colored)
	if plain != "Hello World" {
		t.Errorf("StripANSI(%q) = %q, want %q", colored, plain, "Hello World")
	}
}
