package display

import (
	"strings"
	"testing"
)

func TestVisibleWidth(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"hello", 5},
		{"", 0},
		{"\033[31mred\033[0m", 3},
		{"\033[1;32mbold green\033[0m", 10},
		{"héllo", 5},
		{"日本語", 3},
		{"\033[0m\033[31m", 0},
		{"a\033[31mb\033[0mc", 3},
	}
	for _, c := range cases {
		if got := VisibleWidth(c.input); got != c.want {
			t.Errorf("VisibleWidth(%q) = %d, want %d", c.input, got, c.want)
		}
	}
}

func TestWrapTextBasic(t *testing.T) {
	cases := []struct {
		input string
		width int
		want  []string
	}{
		{"hello world", 100, []string{"hello world"}},
		{"hello world", 5, []string{"hello", "world"}},
		{"hello world", 11, []string{"hello world"}},
		{"hello world", 10, []string{"hello", "world"}},
		{"", 10, []string{""}},
		{"a b c d e", 3, []string{"a b", "c d", "e"}},
		{"a b c", 100, []string{"a b c"}},
		{"unDeuxTroisQuatreCinqSixSeptHuitNeufDix", 10, []string{"unDeuxTroi", "sQuatreCin", "qSixSeptHu", "itNeufDix"}},
		{"ligne1\nligne2", 100, []string{"ligne1", "ligne2"}},
	}
	for _, c := range cases {
		got := WrapText(c.input, c.width)
		if !equalSlice(got, c.want) {
			t.Errorf("WrapText(%q, %d) = %v, want %v", c.input, c.width, got, c.want)
		}
	}
}

func TestWrapTextANSI(t *testing.T) {
	// Les séquences ANSI ne comptent pas dans la largeur visible.
	got := WrapText("\033[31mhello world\033[0m", 5)
	want := []string{"\033[31mhello", "world\033[0m"}
	if !equalSlice(got, want) {
		t.Errorf("WrapText ANSI = %v, want %v", got, want)
	}
}

func TestWrapTextNoSpaces(t *testing.T) {
	got := WrapText("abcdefghij", 3)
	want := []string{"abc", "def", "ghi", "j"}
	if !equalSlice(got, want) {
		t.Errorf("WrapText no spaces = %v, want %v", got, want)
	}
}

func TestWrapTextUnicode(t *testing.T) {
	// Les runes multi-octets comptent pour 1 colonne chacune.
	got := WrapText("日本語テスト", 3)
	want := []string{"日本語", "テスト"}
	if !equalSlice(got, want) {
		t.Errorf("WrapText unicode = %v, want %v", got, want)
	}
}

func TestWrapTextWidthOne(t *testing.T) {
	got := WrapText("hello", 1)
	want := []string{"h", "e", "l", "l", "o"}
	if !equalSlice(got, want) {
		t.Errorf("WrapText width 1 = %v, want %v", got, want)
	}
}

func TestPadCellVisible(t *testing.T) {
	cases := []struct {
		text string
		w    int
		want string
	}{
		{"hi", 5, "hi   "},
		{"hello", 3, "hello"},
		{"\033[31mhi\033[0m", 5, "\033[31mhi\033[0m   "},
		{"", 3, "   "},
	}
	for _, c := range cases {
		got := PadCellVisible(c.text, c.w)
		if got != c.want {
			t.Errorf("PadCellVisible(%q, %d) = %q, want %q", c.text, c.w, got, c.want)
		}
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWrapTextPreservesExistingNewlines(t *testing.T) {
	input := "première ligne\nduxième ligne avec un mot très longquiNeRentrePas"
	got := WrapText(input, 20)
	// La première ligne tient sur 20 colonnes.
	if got[0] != "première ligne" {
		t.Errorf("ligne 1 = %q, want %q", got[0], "première ligne")
	}
	// La deuxième ligne est coupée car elle dépasse 20 colonnes.
	foundLong := false
	for _, line := range got[1:] {
		if strings.Contains(line, "longquiNeRentrePas") {
			foundLong = true
		}
	}
	if !foundLong {
		t.Errorf("le mot long devrait apparaître dans le résultat: %v", got)
	}
}

func TestWrapTextURL(t *testing.T) {
	url := "https://example.com/api/v1/users/12345/profile?format=json&include=details"
	got := WrapText(url, 40)
	// Aucune ligne ne doit dépasser 40 colonnes visibles.
	for _, line := range got {
		if VisibleWidth(line) > 40 {
			t.Errorf("ligne dépasse 40 colonnes: %q (%d)", line, VisibleWidth(line))
		}
	}
}

func TestVisibleWidthANSIComplex(t *testing.T) {
	s := "\033[1;31m\033[4mtexte\033[0m"
	if w := VisibleWidth(s); w != 5 {
		t.Errorf("VisibleWidth = %d, want 5", w)
	}
}
