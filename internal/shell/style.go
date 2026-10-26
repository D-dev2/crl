package shell

import (
	"io"
	"strings"
	"unicode/utf8"

	"crl-cli/internal/mint"
	"crl-cli/internal/termstyle"
)

// palette est la palette commune de CRL-CLI. Les couleurs sont activées
// uniquement si la sortie est un terminal et que l'utilisateur ne les a pas
// désactivées.
type palette = termstyle.Palette

func colorEnabled(w io.Writer) bool {
	return termstyle.Enabled(w)
}

// captureStatus colore le statut d'une capture Mint : une erreur de transport
// reste rouge, une réponse en attente jaune, et les statuts HTTP suivent la
// palette commune.
func captureStatus(p palette, c mint.Capture, text string) string {
	switch {
	case c.Err != "":
		return p.BoldRed(text)
	case c.Response == nil:
		return p.BoldYellow(text)
	default:
		return p.StatusCode(c.Response.StatusCode, text)
	}
}

// mintState rend l'état du proxy avec une couleur cohérente.
func mintState(p palette, state mint.State) string {
	switch state {
	case mint.StateRunning:
		return p.BoldGreen("actif")
	case mint.StateStarting:
		return p.BoldYellow("en démarrage")
	case mint.StateStopping:
		return p.BoldYellow("en arrêt")
	case mint.StateFailed:
		return p.BoldRed("en erreur")
	case mint.StateStopped:
		return p.Dim("arrêté")
	default:
		if state == "" {
			return ""
		}
		return p.Dim(string(state))
	}
}

// proxyStateBadge affiche l'état demandé par la configuration Mint avec les
// quatre libellés ACTIF, ARRÊTÉ, ERREUR et EN ATTENTE.
func proxyStateBadge(p palette, st mint.Status) string {
	switch st.State {
	case mint.StateRunning:
		return p.BoldGreen("ACTIF")
	case mint.StateFailed:
		return p.BoldRed("ERREUR")
	case mint.StateStarting, mint.StateStopping:
		return p.BoldYellow("EN ATTENTE")
	case mint.StateStopped:
		return p.Dim("ARRÊTÉ")
	default:
		return p.BoldYellow("EN ATTENTE")
	}
}

// padCell complète une cellule jusqu'à une largeur exprimée en caractères
// visibles. Les styles ANSI doivent être appliqués après cet appel.
func padCell(text string, width int) string {
	n := utf8.RuneCountInString(text)
	if n >= width {
		return text
	}
	return text + strings.Repeat(" ", width-n)
}
