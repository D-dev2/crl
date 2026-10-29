// Package termstyle centralise les couleurs et les styles ANSI de CRL-CLI.
//
// Les séquences ne sont produites que lorsque la sortie est un terminal et que
// l'utilisateur n'a pas demandé leur désactivation. Les fonctions de cette
// package ne modifient jamais les données HTTP : elles ne servent qu'au rendu
// terminal.
package termstyle

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// Reset termine une séquence de style ANSI.
const Reset = "\033[0m"

// Palette applique des styles ANSI. Une palette désactivée retourne le texte
// inchangé, ce qui permet de réutiliser le même code pour les fichiers et les
// tests.
type Palette struct {
	Enabled bool
}

// New crée une palette adaptée au writer. Les redirections, les tests, les
// terminaux « dumb » et les variables NO_COLOR/CLICOLOR=0 désactivent les
// couleurs.
func New(w io.Writer) Palette {
	return Palette{Enabled: Enabled(w)}
}

// Enabled indique si les séquences ANSI peuvent être écrites sur w.
func Enabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLICOLOR"))) {
	case "0":
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return IsTerminal(f.Fd())
}

// Paint applique un code SGR à un texte non vide.
func (p Palette) Paint(code, text string) string {
	if !p.Enabled || text == "" {
		return text
	}
	return "\033[" + code + "m" + text + Reset
}

func (p Palette) Bold(text string) string      { return p.Paint("1", text) }
func (p Palette) Dim(text string) string       { return p.Paint("2", text) }
func (p Palette) Red(text string) string       { return p.Paint("31", text) }
func (p Palette) Green(text string) string     { return p.Paint("32", text) }
func (p Palette) Yellow(text string) string    { return p.Paint("33", text) }
func (p Palette) Blue(text string) string      { return p.Paint("34", text) }
func (p Palette) Magenta(text string) string   { return p.Paint("35", text) }
func (p Palette) Cyan(text string) string      { return p.Paint("36", text) }
func (p Palette) White(text string) string     { return p.Paint("37", text) }
func (p Palette) BoldRed(text string) string   { return p.Paint("1;31", text) }
func (p Palette) BoldGreen(text string) string { return p.Paint("1;32", text) }
func (p Palette) BoldYellow(text string) string {
	return p.Paint("1;33", text)
}
func (p Palette) BoldBlue(text string) string    { return p.Paint("1;34", text) }
func (p Palette) BoldMagenta(text string) string { return p.Paint("1;35", text) }
func (p Palette) BoldCyan(text string) string    { return p.Paint("1;36", text) }

// Method colore une méthode HTTP de façon stable.
func (p Palette) Method(method string) string {
	name := strings.ToUpper(strings.TrimSpace(method))
	switch name {
	case "GET":
		return p.BoldGreen(method)
	case "POST":
		return p.BoldBlue(method)
	case "PUT":
		return p.BoldYellow(method)
	case "PATCH":
		return p.BoldMagenta(method)
	case "DELETE":
		return p.BoldRed(method)
	case "HEAD", "OPTIONS", "TRACE", "CONNECT":
		return p.BoldCyan(method)
	default:
		return p.Bold(method)
	}
}

// StatusCode colore un statut HTTP : 2xx vert, 3xx cyan, 4xx jaune, 5xx rouge.
func (p Palette) StatusCode(code int, text string) string {
	switch {
	case code >= 200 && code < 300:
		return p.BoldGreen(text)
	case code >= 300 && code < 400:
		return p.BoldCyan(text)
	case code >= 400 && code < 500:
		return p.BoldYellow(text)
	case code >= 500:
		return p.BoldRed(text)
	default:
		return p.BoldYellow(text)
	}
}

// PadCell complète une cellule jusqu'à une largeur exprimée en caractères
// visibles. Les styles ANSI doivent être appliqués après cet appel.
func PadCell(text string, width int) string {
	n := utf8.RuneCountInString(text)
	if n >= width {
		return text
	}
	return text + strings.Repeat(" ", width-n)
}
