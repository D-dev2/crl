package shell

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

type helpEntry struct{ syntax, description string }
type helpGroup struct {
	title   string
	entries []helpEntry
}

var commandHelp = []helpGroup{
	{"REQUÊTES", []helpEntry{
		{"crl.host [url]", "Définir ou afficher l'URL cible"},
		{"crl.method [méthode]", "Définir ou afficher la méthode active"},
		{"crl.method <méthode> set <cible>", "Changer la méthode des captures ciblées"},
		{"crl.head | crl.header [Nom: valeur]", "Afficher ou configurer les en-têtes actifs"},
		{"crl.head set <cible> <Nom> <valeur>", "Ajouter ou remplacer un en-tête ciblé"},
		{"crl.head delete <cible> <Nom>", "Supprimer un en-tête ciblé"},
		{"crl.head show <cible>", "Afficher les en-têtes d'une capture"},
		{"crl.head.clear | crl.header.clear [Nom]", "Supprimer un en-tête ou tous"},
		{"crl.json [JSON|clear]", "Éditer le JSON actif, le définir ou l'effacer"},
		{"crl.json set <chemin> = <valeur>", "Modifier un champ JSON actif"},
		{"crl.json delete <chemin>", "Supprimer un champ JSON actif"},
		{"crl.json <cible> set|delete|clear|edit …", "Modifier ou éditer les corps JSON ciblés"},
		{"crl.json <id> edit", "Éditer le JSON d'une capture précise"},
		{"crl.body [texte|clear]", "Éditer le corps brut, le définir ou l'effacer"},
		{"crl.body <cible> set|clear <texte>", "Définir ou effacer les corps ciblés"},
		{"crl.body <id> edit", "Éditer le corps brut d'une capture précise"},
		{"crl.body.clear", "Effacer le corps actif"},
		{"crl.proxy [url|none|auto]", "Afficher ou définir le proxy"},
		{"crl.ca [fichier|off]", "Afficher ou définir le CA TLS"},
	}},
	{"INSPECTION ET ENVOI", []helpEntry{
		{"crl.show [id|active]", "Afficher les requêtes détaillées"},
		{"crl.show active body [offset]", "Afficher une page du corps actif"},
		{"crl.show <id> body [offset]", "Afficher une page du corps de requête"},
		{"crl.show body [offset]", "Afficher une page du corps actif"},
		{"crl.table", "Afficher un résumé des requêtes"},
		{"crl.output [auto|raw|pretty]", "Choisir le rendu des réponses"},
		{"crl.launch [cibles|active]", "Envoyer les cibles ou le constructeur actif"},
		{"crl.help", "Afficher l'aide détaillée des commandes CRL"},
	}},
	{"MINT", []helpEntry{
		{"mint.proxy [status|on|off|config]", "Afficher, démarrer, arrêter ou configurer le proxy"},
		{"mint.proxy on|start, off|stop", "Démarrer ou arrêter le proxy"},
		{"mint.proxy.on|off|status|config", "Alias des sous-commandes du proxy"},
		{"mint.proxy.port <port>", "Changer le port quand le proxy est arrêté"},
		{"mint.requests", "Lister les captures selon le filtre"},
		{"mint.history", "Lister toutes les captures"},
		{"mint.request.show | mint.show <id>", "Afficher une capture complète"},
		{"mint.select <id...|all>", "Remplacer la sélection de captures"},
		{"mint.select add|remove <cible>", "Modifier la sélection courante"},
		{"mint.select clear", "Vider la sélection"},
		{"mint.selected", "Afficher les requêtes sélectionnées"},
		{"mint.replay [cibles]", "Rejouer les captures en parallèle"},
		{"mint.replay.body <id> [offset]", "Lire une page de corps de réponse"},
		{"mint.filter [texte|clear]", "Filtrer les captures"},
		{"mint.watch [on|off]", "Activer ou suspendre les notifications"},
		{"mint.events", "Afficher les notifications en attente"},
		{"mint.use [id] [--force]", "Importer une capture sans l'envoyer"},
		{"mint.unselect <id>|all", "Retirer une capture de la sélection (alias de mint.select remove/clear)"},
		{"mint.clear [--yes|-y]", "Effacer l'historique"},
		{"mint | mint.help", "Afficher l'aide détaillée de Mint"},
	}},
	{"SESSION", []helpEntry{
		{"reset", "Réinitialiser le constructeur actif"},
		{"clear", "Effacer l'écran interactif"},
		{"help [commande]", "Afficher l'aide générale ou détaillée"},
		{"exit | quit", "Quitter et arrêter proprement Mint"},
	}},
}

func renderHelp(out io.Writer, p palette, query string, unicode bool, width int) bool {
	groups := commandHelp
	if query != "" {
		q := strings.ToLower(strings.TrimSpace(query))
		var matchedGroups []helpGroup
		found := false
		for _, group := range commandHelp {
			var matched []helpEntry
			for _, entry := range group.entries {
				first := strings.Fields(entry.syntax)
				if strings.HasPrefix(strings.ToLower(entry.syntax), q) || (len(first) > 0 && strings.EqualFold(first[0], q)) {
					matched = append(matched, entry)
				}
			}
			if len(matched) > 0 {
				matchedGroups = append(matchedGroups, helpGroup{title: group.title, entries: matched})
				found = true
			}
		}
		if !found {
			return false
		}
		groups = matchedGroups
	}
	if width < 40 {
		width = 40
	}
	if width > 110 {
		width = 110
	}
	inside := width - 2
	if unicode {
		prefix := "╭─ CRL-CLI "
		fmt.Fprintf(out, "%s\n", p.BoldCyan(prefix+strings.Repeat("─", maxInt(1, width-utf8.RuneCountInString(prefix)-1))+"╮"))
		title := "│ Commandes CRL-CLI"
		fmt.Fprintf(out, "%s%s│\n", p.Bold(title), strings.Repeat(" ", maxInt(0, inside-utf8.RuneCountInString(title))))
		fmt.Fprintf(out, "%s\n", p.Cyan("╰"+strings.Repeat("─", inside)+"╯"))
	} else {
		prefix := "+-- CRL-CLI "
		fmt.Fprintf(out, "%s\n", p.BoldCyan(prefix+strings.Repeat("-", maxInt(1, width-len(prefix)-1))+"+"))
		title := "| Commandes CRL-CLI"
		fmt.Fprintf(out, "%s%s|\n", p.Bold(title), strings.Repeat(" ", maxInt(0, inside-len(title))))
		fmt.Fprintf(out, "%s\n", p.Cyan("+"+strings.Repeat("-", inside)+"+"))
	}
	for _, group := range groups {
		fmt.Fprintf(out, "\n%s\n", p.BoldBlue(group.title))
		maxSyntax := 0
		for _, e := range group.entries {
			if n := utf8.RuneCountInString(e.syntax); n > maxSyntax {
				maxSyntax = n
			}
		}
		if maxSyntax > width-26 {
			maxSyntax = width - 26
		}
		for _, e := range group.entries {
			syntax := e.syntax
			if width < 70 || utf8.RuneCountInString(syntax) > maxSyntax {
				for _, line := range wrapWords(syntax, width-4) {
					fmt.Fprintf(out, "  %s\n", p.Cyan(line))
				}
				for _, line := range wrapWords(e.description, width-6) {
					fmt.Fprintf(out, "    %s\n", p.Dim(line))
				}
				continue
			}
			padding := maxSyntax - utf8.RuneCountInString(syntax)
			descWidth := width - maxSyntax - 7
			wrapped := wrapWords(e.description, descWidth)
			fmt.Fprintf(out, "  %s%s  %s\n", p.Cyan(syntax), strings.Repeat(" ", padding), wrapped[0])
			for _, line := range wrapped[1:] {
				fmt.Fprintf(out, "  %s%s  %s\n", strings.Repeat(" ", maxSyntax), "", p.Dim(line))
			}
		}
	}
	fmt.Fprintf(out, "\n%s\n", p.Dim("Cibles : ID, liste séparée par des virgules, selected ou all. Utilisez help <commande> pour filtrer l'aide."))
	fmt.Fprintf(out, "%s\n", p.Dim(`Exemple : mint.select 12,15 ; mint.replay ; crl.json set user.role = "admin"`))
	return true
}

func wrapWords(s string, width int) []string {
	if width < 20 {
		width = 20
	}
	words := strings.Fields(s)
	var lines []string
	line := ""
	for _, word := range words {
		if line != "" && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > width {
			lines = append(lines, line)
			line = word
		} else if line == "" {
			line = word
		} else {
			line += " " + word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}
