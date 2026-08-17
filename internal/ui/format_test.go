package ui

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{10240, "10 KB"},
		{1048576, "1.0 MB"},
		{1258291, "1.2 MB"},
		{1073741824, "1.0 GB"},
		{1099511627776, "1.0 TB"},
	}
	for _, tt := range tests {
		if got := FormatBytes(tt.in); got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Every rendering has to fit the column, or the table shears.
func TestFormatBytesFitsTheColumn(t *testing.T) {
	for _, n := range []uint64{0, 999, 1024, 999999, 1 << 30, 1<<64 - 1} {
		if got := FormatBytes(n); len(got) > wBytes {
			t.Errorf("FormatBytes(%d) = %q, %d wide, want at most %d", n, got, len(got), wBytes)
		}
	}
}

func TestFormatAge(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "—"},
		{0, "0s"},
		{45 * time.Second, "45s"},
		{time.Minute, "1m"},
		{90 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h00"},
		{90 * time.Minute, "1h30"},
		{25 * time.Hour, "1d01"},
		{100 * time.Hour, "4d04"},
	}
	for _, tt := range tests {
		if got := FormatAge(tt.in); got != tt.want {
			t.Errorf("FormatAge(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatAgeFitsTheColumn(t *testing.T) {
	for _, d := range []time.Duration{0, time.Second, time.Hour, 400 * 24 * time.Hour} {
		if got := FormatAge(d); ansi.StringWidth(got) > wAge {
			t.Errorf("FormatAge(%v) = %q, wider than the %d-cell column", d, got, wAge)
		}
	}
}

func TestFormatUptime(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "00:00:00"},
		{0, "00:00:00"},
		{61 * time.Second, "00:01:01"},
		{3661 * time.Second, "01:01:01"},
		{100 * time.Hour, "100:00:00"},
	}
	for _, tt := range tests {
		if got := FormatUptime(tt.in); got != tt.want {
			t.Errorf("FormatUptime(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPad(t *testing.T) {
	tests := []struct {
		in    string
		width int
		want  string
	}{
		{"abc", 5, "abc  "},
		{"abc", 3, "abc"},
		{"abcdef", 4, "abc…"},
		{"", 3, "   "},
		{"abc", 0, ""},
		{"abc", -1, ""},
	}
	for _, tt := range tests {
		if got := pad(tt.in, tt.width); got != tt.want {
			t.Errorf("pad(%q, %d) = %q, want %q", tt.in, tt.width, got, tt.want)
		}
	}
}

func TestPadLeft(t *testing.T) {
	tests := []struct {
		in    string
		width int
		want  string
	}{
		{"3000", 6, "  3000"},
		{"3000", 4, "3000"},
		{"300000", 4, "300…"},
		{"", 2, "  "},
		{"x", 0, ""},
	}
	for _, tt := range tests {
		if got := padLeft(tt.in, tt.width); got != tt.want {
			t.Errorf("padLeft(%q, %d) = %q, want %q", tt.in, tt.width, got, tt.want)
		}
	}
}

// Padding must be measured in display cells, not bytes, or wide glyphs shear
// the table.
func TestPadCountsDisplayWidth(t *testing.T) {
	if got := pad("é", 3); ansi.StringWidth(got) != 3 {
		t.Errorf("pad(é, 3) is %d cells wide, want 3", ansi.StringWidth(got))
	}
	if got := pad("日本", 6); ansi.StringWidth(got) != 6 {
		t.Errorf("pad(日本, 6) is %d cells wide, want 6", ansi.StringWidth(got))
	}
}

func TestClampWidth(t *testing.T) {
	if got := clampWidth("hello", 3); got != "hel" {
		t.Errorf("clampWidth = %q, want %q", got, "hel")
	}
	if got := clampWidth("hi", 10); got != "hi" {
		t.Errorf("clampWidth should leave short strings alone, got %q", got)
	}
	if got := clampWidth("hi", 0); got != "hi" {
		t.Errorf("clampWidth with no width should pass through, got %q", got)
	}
}

func TestItoa(t *testing.T) {
	tests := map[int]string{0: "0", 7: "7", 3000: "3000", -42: "-42", 65535: "65535"}
	for in, want := range tests {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPlural(t *testing.T) {
	if plural(1) != "" || plural(0) != "s" || plural(2) != "s" {
		t.Error("plural() is wrong")
	}
}
