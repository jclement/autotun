package ui

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// textInput is a single-line editor. The filter box is the only text entry in
// the app, so a purpose-built 60 lines beats pulling in a widget library and
// its version churn.
type textInput struct {
	value  []rune
	cursor int
}

func (t *textInput) Value() string { return string(t.value) }

func (t *textInput) SetValue(s string) {
	t.value = []rune(s)
	t.cursor = len(t.value)
}

func (t *textInput) Reset() {
	t.value = t.value[:0]
	t.cursor = 0
}

// Update applies a key press, reporting whether it was consumed.
func (t *textInput) Update(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyRunes, tea.KeySpace:
		runes := msg.Runes
		if msg.Type == tea.KeySpace {
			runes = []rune{' '}
		}
		for _, r := range runes {
			if unicode.IsControl(r) {
				continue
			}
			t.value = append(t.value, 0)
			copy(t.value[t.cursor+1:], t.value[t.cursor:])
			t.value[t.cursor] = r
			t.cursor++
		}
		return true
	case tea.KeyBackspace:
		if t.cursor > 0 {
			t.value = append(t.value[:t.cursor-1], t.value[t.cursor:]...)
			t.cursor--
		}
		return true
	case tea.KeyDelete:
		if t.cursor < len(t.value) {
			t.value = append(t.value[:t.cursor], t.value[t.cursor+1:]...)
		}
		return true
	case tea.KeyLeft:
		if t.cursor > 0 {
			t.cursor--
		}
		return true
	case tea.KeyRight:
		if t.cursor < len(t.value) {
			t.cursor++
		}
		return true
	case tea.KeyHome, tea.KeyCtrlA:
		t.cursor = 0
		return true
	case tea.KeyEnd, tea.KeyCtrlE:
		t.cursor = len(t.value)
		return true
	case tea.KeyCtrlU:
		t.value = t.value[:0]
		t.cursor = 0
		return true
	case tea.KeyCtrlW:
		t.deleteWord()
		return true
	}
	return false
}

func (t *textInput) deleteWord() {
	i := t.cursor
	for i > 0 && unicode.IsSpace(t.value[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(t.value[i-1]) {
		i--
	}
	t.value = append(t.value[:i], t.value[t.cursor:]...)
	t.cursor = i
}

// Render draws the value with a block cursor.
func (t *textInput) Render(th Theme) string {
	var b strings.Builder
	for i, r := range t.value {
		if i == t.cursor {
			b.WriteString(th.RowSel.Render(string(r)))
		} else {
			b.WriteString(th.Value.Render(string(r)))
		}
	}
	if t.cursor >= len(t.value) {
		b.WriteString(th.RowSel.Render(" "))
	}
	return b.String()
}
