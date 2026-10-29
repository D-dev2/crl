// Package display centralise les règles d'affichage de CRL-CLI :
// masquage des secrets, rendu des URL et des corps HTTP.
//
// Ces fonctions ne modifient jamais les données utilisées pour rejouer une
// requête : elles ne travaillent que sur des copies destinées au terminal.
package display

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var ansiRegexp = regexp.MustCompile(`\033\[[0-9;]*[A-Za-z]`)

// VisibleWidth retourne le nombre de colonnes visibles occupées par s,
// en supprimant les séquences d'échappement ANSI et en comptant les runes.
// Les caractères Unicode larges (double-width) sont comptés comme 1 colonne
// pour simplifier le calcul de largeur.
func VisibleWidth(s string) int {
	s = ansiRegexp.ReplaceAllString(s, "")
	return utf8.RuneCountInString(s)
}

// WrapText découpe s en lignes d'au plus width colonnes visibles. Il préserve
// les sauts de ligne existants et privilégie les coupes aux espaces. Les mots
// trop longs pour tenir sur une ligne sont coupés au milieu, en respectant
// les frontières des runes UTF-8.
func WrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var lines []string
	for _, paragraph := range strings.Split(s, "\n") {
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, wrapParagraph(paragraph, width)...)
	}
	return lines
}

func wrapParagraph(s string, width int) []string {
	if VisibleWidth(s) <= width {
		return []string{s}
	}

	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}

	var lines []string
	var current []string
	currentWidth := 0

	flush := func() {
		if len(current) > 0 {
			lines = append(lines, strings.Join(current, " "))
			current = nil
			currentWidth = 0
		}
	}

	for _, word := range words {
		wordWidth := VisibleWidth(word)

		// Mot trop long : le couper au milieu, rune par rune.
		if wordWidth > width {
			flush()
			runes := []rune(word)
			for len(runes) > 0 {
				take := width
				if take > len(runes) {
					take = len(runes)
				}
				lines = append(lines, string(runes[:take]))
				runes = runes[take:]
			}
			continue
		}

		needed := 0
		if len(current) > 0 {
			needed = 1
		}

		if currentWidth+needed+wordWidth <= width {
			current = append(current, word)
			currentWidth += needed + wordWidth
		} else {
			flush()
			current = []string{word}
			currentWidth = wordWidth
		}
	}

	if len(current) > 0 {
		lines = append(lines, strings.Join(current, " "))
	}

	return lines
}

// PadCellVisible complète text jusqu'à width colonnes visibles, en tenant
// compte des séquences ANSI. Il ne modifie pas le texte si sa largeur visible
// est déjà supérieure ou égale à width.
func PadCellVisible(text string, width int) string {
	w := VisibleWidth(text)
	if w >= width {
		return text
	}
	return text + strings.Repeat(" ", width-w)
}
