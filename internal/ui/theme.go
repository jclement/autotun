// Package ui renders autotun's terminal interface.
package ui

import "github.com/charmbracelet/lipgloss"

// Theme holds every style the UI draws with. Colors are adaptive so the app
// looks deliberate on light terminals too, not just the dark ones it was
// designed on.
type Theme struct {
	Accent    lipgloss.AdaptiveColor
	Accent2   lipgloss.AdaptiveColor
	Muted     lipgloss.AdaptiveColor
	Faint     lipgloss.AdaptiveColor
	Warn      lipgloss.AdaptiveColor
	Err       lipgloss.AdaptiveColor
	Text      lipgloss.AdaptiveColor
	SelBG     lipgloss.AdaptiveColor
	MatrixLit lipgloss.AdaptiveColor
	MatrixDim lipgloss.AdaptiveColor

	Title       lipgloss.Style
	Host        lipgloss.Style
	Meta        lipgloss.Style
	Header      lipgloss.Style
	Row         lipgloss.Style
	RowSel      lipgloss.Style
	Live        lipgloss.Style
	Fresh       lipgloss.Style
	Frame       lipgloss.Style
	Accent2Text lipgloss.Style
	Dim         lipgloss.Style
	Faintest    lipgloss.Style
	Good        lipgloss.Style
	Warning     lipgloss.Style
	Bad         lipgloss.Style
	Key         lipgloss.Style
	KeyDesc     lipgloss.Style
	Box         lipgloss.Style
	BoxTitle    lipgloss.Style
	Label       lipgloss.Style
	Value       lipgloss.Style
	Separator   lipgloss.Style
}

// DefaultTheme is the phosphor-green look: green for live traffic, cyan for
// identity, everything else deliberately quiet.
func DefaultTheme() Theme {
	t := Theme{
		Accent:    lipgloss.AdaptiveColor{Light: "#00875f", Dark: "#00d787"},
		Accent2:   lipgloss.AdaptiveColor{Light: "#0087af", Dark: "#5fd7ff"},
		Muted:     lipgloss.AdaptiveColor{Light: "#6c6f85", Dark: "#8a8f98"},
		Faint:     lipgloss.AdaptiveColor{Light: "#9ca0b0", Dark: "#5c6169"},
		Warn:      lipgloss.AdaptiveColor{Light: "#c07000", Dark: "#ffaf5f"},
		Err:       lipgloss.AdaptiveColor{Light: "#c62828", Dark: "#ff5f5f"},
		Text:      lipgloss.AdaptiveColor{Light: "#1c1c1c", Dark: "#e6e6e6"},
		SelBG:     lipgloss.AdaptiveColor{Light: "#d7f5e6", Dark: "#1f3a30"},
		MatrixLit: lipgloss.AdaptiveColor{Light: "#00c070", Dark: "#b9ffcf"},
		MatrixDim: lipgloss.AdaptiveColor{Light: "#8fd8b4", Dark: "#00a95c"},
	}

	t.Title = lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	t.Host = lipgloss.NewStyle().Foreground(t.Accent2).Bold(true)
	t.Meta = lipgloss.NewStyle().Foreground(t.Muted)
	t.Header = lipgloss.NewStyle().Foreground(t.Muted).Bold(true)
	t.Row = lipgloss.NewStyle().Foreground(t.Text)
	t.RowSel = lipgloss.NewStyle().Foreground(t.Text).Background(t.SelBG).Bold(true)
	// A tunnel in use is the one you are looking for; a brand new one is the
	// one you just started. Both earn a brighter treatment than the rest.
	t.Live = lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	t.Fresh = lipgloss.NewStyle().Foreground(t.Accent2)
	// The frame is structure, not content: it should read as a quiet outline
	// rather than competing with the data inside it.
	t.Frame = lipgloss.NewStyle().Foreground(t.Faint)
	t.Accent2Text = lipgloss.NewStyle().Foreground(t.Accent2)
	t.Dim = lipgloss.NewStyle().Foreground(t.Muted)
	t.Faintest = lipgloss.NewStyle().Foreground(t.Faint)
	t.Good = lipgloss.NewStyle().Foreground(t.Accent)
	t.Warning = lipgloss.NewStyle().Foreground(t.Warn)
	t.Bad = lipgloss.NewStyle().Foreground(t.Err)
	t.Key = lipgloss.NewStyle().Foreground(t.Accent2).Bold(true)
	t.KeyDesc = lipgloss.NewStyle().Foreground(t.Muted)
	t.Box = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Accent).
		Padding(0, 2)
	t.BoxTitle = lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	t.Label = lipgloss.NewStyle().Foreground(t.Muted)
	t.Value = lipgloss.NewStyle().Foreground(t.Text)
	t.Separator = lipgloss.NewStyle().Foreground(t.Faint)
	return t
}
