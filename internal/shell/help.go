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
	{"REQUÊTES — CONSTRUCTEUR ACTIF (sans ID)", []helpEntry{
		{"crl.host [<url>]", "Définir ou afficher l'URL du constructeur actif"},
		{"crl.method [<méthode>]", "Définir ou afficher la méthode du constructeur actif"},
		{"crl.head | crl.header [<Nom>: <valeur>]", "Afficher ou configurer un en-tête du constructeur actif"},
		{"crl.head.clear | crl.header.clear [<Nom>]", "Supprimer un en-tête nommé, ou tous ceux du constructeur"},
		{"crl.cookie [<nom>=<valeur>|clear]", "Afficher, ajouter ou effacer les cookies du constructeur actif"},
		{"crl.param [<nom>=<valeur>|clear]", "Afficher, ajouter ou effacer les paramètres d'URL du constructeur actif"},
		{"crl.json [<JSON>|clear]", "Définir ou effacer le JSON du constructeur actif"},
		{"crl.json set <chemin JSON> = <valeur>", "Modifier un champ JSON du constructeur actif"},
		{"crl.json delete <chemin JSON>", "Supprimer un champ JSON du constructeur actif"},
		{"crl.body [<texte>|clear]", "Définir ou effacer le corps brut du constructeur actif"},
		{"crl.body.clear", "Effacer le corps du constructeur actif"},
		{"crl.proxy [<url>|none|auto]", "Afficher ou définir le proxy sortant du constructeur"},
		{"crl.ca [<fichier>|off]", "Afficher ou définir le CA TLS du constructeur"},
	}},
	{"CAPTURES MINT (avec ID)", []helpEntry{
		{"crl.host add <cible> <url>", "Définir l'URL des captures ciblées"},
		{"crl.host remove <cible>", "Retirer l'URL des captures ciblées"},
		{"crl.method add <cible> <méthode>", "Définir la méthode des captures ciblées"},
		{"crl.method remove <cible>", "Remettre la méthode des captures ciblées à GET"},
		{"crl.head add <cible> <Nom>: <valeur>", "Ajouter ou remplacer un en-tête des captures ciblées"},
		{"crl.head remove <cible> <Nom>", "Supprimer un en-tête des captures ciblées"},
		{"crl.head show <cible>", "Afficher les en-têtes des captures ciblées"},
		{"crl.cookie add <cible> <nom>=<valeur>", "Ajouter ou remplacer un cookie des captures ciblées"},
		{"crl.cookie remove <cible> <nom>", "Supprimer un cookie des captures ciblées"},
		{"crl.cookie clear <cible>", "Supprimer tous les cookies des captures ciblées"},
		{"crl.param add <cible> <nom>=<valeur>", "Ajouter ou remplacer un paramètre d'URL des captures ciblées"},
		{"crl.param remove <cible> <nom>", "Supprimer un paramètre d'URL des captures ciblées"},
		{"crl.param clear <cible>", "Supprimer tous les paramètres d'URL des captures ciblées"},
		{"crl.json add <cible> <JSON>", "Définir le corps JSON des captures ciblées"},
		{"crl.json remove <cible>", "Effacer le corps des captures ciblées"},
		{"crl.json <cible> set <chemin> = <valeur> | delete <chemin> | clear | edit", "Modifier ou éditer les corps JSON ciblés"},
		{"crl.json <id> edit", "Éditer le JSON d'une capture précise (Ctrl+S, Esc)"},
		{"crl.body add <cible> <texte>", "Définir le corps brut des captures ciblées"},
		{"crl.body remove <cible>", "Effacer le corps des captures ciblées"},
		{"crl.body <cible> set <texte> | clear", "Variantes de compatibilité pour les corps ciblés"},
		{"crl.body <id> edit", "Éditer le corps brut d'une capture précise (Ctrl+S, Esc)"},
		{"crl.method <méthode> set <cible>", "Ancienne forme de crl.method add, conservée"},
		{"crl.head set|delete <cible> …", "Anciennes formes de crl.head add|remove, conservées"},
	}},
	{"INSPECTION ET ENVOI", []helpEntry{
		{"crl.show [<id>|active|selected]", "Afficher le constructeur, une capture ou la sélection"},
		{"crl.show active body [offset]", "Afficher une page du corps actif"},
		{"crl.show <id> body [offset]", "Afficher une page du corps de requête"},
		{"crl.show body [offset]", "Afficher une page du corps actif"},
		{"crl.table", "Afficher un résumé du constructeur ou de la sélection"},
		{"crl.output [auto|raw|pretty]", "Choisir le rendu des réponses"},
		{"crl.launch <id>", "Rejouer uniquement cette capture, sans changer la sélection"},
		{"crl.launch <ids>|selected", "Rejouer les captures indiquées ou la sélection persistante"},
		{"crl.launch", "Rejouer la sélection si elle existe, sinon envoyer le constructeur actif"},
		{"crl.launch active", "Envoyer le constructeur actif"},
		{"crl.launch all", "Refusé : utilisez mint.select all puis crl.launch"},
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
		{"mint.select <ids>", "Remplacer la sélection par ces captures (espaces ou virgules)"},
		{"mint.select all", "Sélectionner toutes les captures de l'historique"},
		{"mint.select add|remove <ids>|all", "Ajouter ou retirer des captures de la sélection"},
		{"mint.select clear", "Vider la sélection"},
		{"mint.selected", "Afficher les requêtes sélectionnées"},
		{"mint.replay [<ids>|selected|all]", "Sans cible : rejouer la sélection ; all : tout l'historique explicitement"},
		{"mint.replay.body <id> [offset]", "Lire une page de corps de réponse"},
		{"mint.filter [<texte>|clear]", "Filtrer les captures"},
		{"mint.watch [on|off]", "Activer ou suspendre les notifications"},
		{"mint.events", "Afficher les notifications en attente"},
		{"mint.use [<id>] [--force]", "Importer une capture sans l'envoyer"},
		{"mint.unselect <id>|all", "Retirer une capture de la sélection (alias de mint.select remove/clear)"},
		{"mint.clear [--yes|-y]", "Effacer l'historique"},
		{"mint | mint.help", "Afficher l'aide détaillée de Mint"},
	}},
	{"CIBLES ET VALEURS", []helpEntry{
		{"<id>", "Un identifiant numérique de capture Mint, affiché par mint.requests (ex. 12)."},
		{"<ids>", "Un ou plusieurs <id>, séparés par des espaces ou des virgules (ex. 1 2 ou 1,2)."},
		{"selected", "La sélection persistante créée par mint.select, utilisée par crl.launch et mint.replay."},
		{"all", "Toutes les captures : accepté par mint.select, mint.replay all et les commandes ciblées crl.host, crl.method, crl.head, crl.cookie, crl.param, crl.json et crl.body ; refusé par crl.launch all."},
		{"active", "Le constructeur CRL actif, pour crl.show et crl.launch."},
		{"<cible>", "Une capture Mint : <id> (ex. 12), <ids> (12,15 ou 12 15), selected (la sélection) ou all (tout l'historique, sauf crl.launch)."},
		{"<méthode>", "Une méthode HTTP : GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS (ou une méthode personnalisée valide)."},
		{"<Nom>", "Le nom d'un champ HTTP valide, par exemple sec-fetch-dest ou content-type. Les deux-points séparent le nom de <valeur>."},
		{"<nom>", "Le nom d'un cookie ou d'un paramètre d'URL, sans espace ni deux-points."},
		{"<nom>=<valeur>", "Un cookie ou paramètre d'URL ; la valeur peut contenir des espaces si elle est citée."},
		{"<valeur>", "La valeur associée : valeur d'en-tête, littéral JSON, texte de corps ou argument indiqué par la commande."},
		{"<JSON>", "Un document JSON valide, sur une seule ligne en dehors de crl.json <id> edit."},
		{"<chemin JSON>", "Un chemin de propriétés séparé par des points (ex. user.role ou items.0.name)."},
		{"<fichier>", "Un chemin vers un fichier existant, par exemple le certificat CA TLS."},
		{"<url>", "Une URL HTTP ou HTTPS complète, ou none|auto pour le proxy."},
		{"<port>", "Un numéro de port TCP local compris entre 1 et 65535."},
		{"<texte>", "Une chaîne libre, entre guillemets si elle contient des espaces."},
		{"offset", "Un décalage en octets, entier positif ou nul, pour lire une page de corps."},
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
		// Les deux aides de catégorie doivent aussi expliquer les placeholders
		// qu'elles utilisent, sans obliger l'utilisateur à revenir à help.
		if q == "crl" || q == "mint" {
			for _, group := range commandHelp {
				if group.title == "CIBLES ET VALEURS" {
					matchedGroups = append(matchedGroups, group)
					break
				}
			}
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
	fmt.Fprintf(out, "\n%s\n", p.Dim("Les cibles acceptées dépendent de la commande ; consultez la section CIBLES ET VALEURS avec help (sans filtre)."))
	fmt.Fprintf(out, "%s\n", p.Dim(`Exemples : mint.select 12,15 ; mint.select all ; crl.launch 12 ; crl.launch (sélection) ; crl.launch all (refusé)`))
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
