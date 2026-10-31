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
	{
		"CONSTRUCTEUR ACTIF (sans ID)",
		[]helpEntry{
			{"crl.host [<url>]", "Définir ou afficher l'URL du constructeur actif"},
			{"crl.host set <id> add|set <url>", "Définir l'URL d'une capture ciblée"},
			{"crl.host set <id> remove", "Retirer l'URL d'une capture ciblée"},
			{"crl.method [<méthode>]", "Définir ou afficher la méthode du constructeur actif"},
			{"crl.method set <id> add|set <méthode>", "Définir la méthode d'une capture ciblée"},
			{"crl.method set <id> remove", "Remettre la méthode d'une capture ciblée à GET"},
			{"crl.head | crl.header [<Nom>: <valeur>]", "Afficher ou ajouter un en-tête du constructeur actif"},
			{"crl.head set <id> add|set <Nom>: <valeur>", "Ajouter ou remplacer un en-tête d'une capture ciblée"},
			{"crl.head set <id> remove <Nom>", "Supprimer un en-tête d'une capture ciblée"},
			{"crl.head.clear | crl.header.clear [<Nom>]", "Supprimer un en-tête nommé, ou tous ceux du constructeur"},
			{"crl.cookie [<nom>=<valeur>|clear]", "Afficher, ajouter ou effacer les cookies du constructeur actif"},
			{"crl.cookie set <id> add|set <nom>=<valeur>", "Ajouter ou remplacer un cookie d'une capture ciblée"},
			{"crl.cookie set <id> remove <nom>", "Supprimer un cookie d'une capture ciblée"},
			{"crl.param [<nom>=<valeur>|clear]", "Afficher, ajouter ou effacer les paramètres d'URL du constructeur actif"},
			{"crl.param set <id> add|set <nom>=<valeur>", "Ajouter ou remplacer un paramètre d'URL d'une capture ciblée"},
			{"crl.param set <id> remove <nom>", "Supprimer un paramètre d'URL d'une capture ciblée"},
			{"crl.json [<JSON>|clear]", "Définir ou effacer le JSON du constructeur actif"},
			{"crl.json set <id> add <JSON>", "Définir le corps JSON d'une capture ciblée"},
			{"crl.json set <id> set <chemin> = <valeur>", "Modifier un champ JSON d'une capture ciblée"},
			{"crl.json set <id> remove <chemin>", "Supprimer un champ JSON d'une capture ciblée"},
			{"crl.json set <id> edit", "Éditer le JSON d'une capture précise (Ctrl+S, Esc)"},
			{"crl.body [<texte>|clear]", "Définir ou effacer le corps brut du constructeur actif"},
			{"crl.body set <id> set <texte>", "Définir le corps brut d'une capture ciblée"},
			{"crl.body set <id> remove", "Supprimer le corps brut d'une capture ciblée"},
			{"crl.body set <id> edit", "Éditer le corps brut d'une capture précise (Ctrl+S, Esc)"},
			{"crl.proxy [<url>|none|auto]", "Afficher ou définir le proxy sortant du constructeur"},
			{"crl.ca [<fichier>|off]", "Afficher ou définir le CA TLS du constructeur"},
		},
	},
	{
		"CAPTURES MINT (avec ID)",
		[]helpEntry{
			{"mint.proxy [status|on|off|config]", "Afficher, démarrer, arrêter ou configurer le proxy de capture"},
			{"mint.requests", "Lister les captures selon le filtre actif"},
			{"mint.history", "Lister toutes les captures"},
			{"mint.request.show | mint.show <id>", "Afficher une capture complète"},
			{"mint.select <ids>", "Remplacer la sélection par ces captures"},
			{"mint.select all", "Sélectionner toutes les captures"},
			{"mint.select add|remove <ids>|all", "Ajouter ou retirer des captures de la sélection"},
			{"mint.select clear", "Vider la sélection"},
			{"mint.selected", "Afficher les requêtes sélectionnées"},
			{"mint.replay [<ids>|selected|all]", "Rejouer la sélection ; all rejoue tout l'historique explicitement"},
			{"mint.replay.body <id> [offset]", "Lire une page de corps de réponse"},
			{"mint.filter [<texte>|clear]", "Filtrer les captures"},
			{"mint.watch [on|off]", "Activer ou suspendre les notifications automatiques"},
			{"mint.events", "Afficher les notifications en attente"},
			{"mint.use [<id>] [--force]", "Importer une capture dans le constructeur sans l'envoyer"},
			{"mint.unselect <id>|all", "Retirer une capture de la sélection"},
			{"mint.clear [--yes|-y]", "Effacer l'historique des captures"},
			{"mint | mint.help", "Afficher l'aide détaillée de Mint"},
		},
	},
	{
		"CIBLES ET VALEURS",
		[]helpEntry{
			{"<id>", "Identifiant numérique d'une capture Mint, affiché par mint.requests (ex. 12)."},
			{"<ids>", "Un ou plusieurs <id>, séparés par des espaces ou des virgules (ex. 1 2 ou 1,2)."},
			{"selected", "La sélection persistante créée par mint.select, utilisée par crl.launch et mint.replay."},
			{"all", "Toutes les captures. Accepté par mint.select, mint.replay all et les commandes ciblées ; refusé par crl.launch all."},
			{"<cible>", "Une capture Mint : <id> (ex. 12), <ids> (12,15 ou 12 15), selected (la sélection) ou all (tout l'historique, sauf crl.launch). La première occurrence de set <cible> désigne la capture visée ; le mot suivant (add, remove ou set) désigne l'opération."},
			{"<méthode>", "Une méthode HTTP : GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS (ou une méthode personnalisée valide)."},
			{"<Nom>", "Le nom d'un champ HTTP valide, par exemple sec-fetch-dest ou content-type. Le premier deux-points sépare le nom de <valeur>."},
			{"<nom>", "Le nom d'un cookie ou d'un paramètre d'URL, sans espace ni deux-points."},
			{"<nom>=<valeur>", "Un cookie ou paramètre d'URL ; la valeur peut contenir des espaces si elle est citée."},
			{"<valeur>", "La valeur associée : valeur d'en-tête, littéral JSON, texte de corps ou argument indiqué par la commande."},
			{"<JSON>", "Un document JSON valide, sur une seule ligne en dehors de crl.json <id> edit."},
			{"<chemin JSON>", "Un chemin de propriétés séparé par des points (ex. user.role ou items.0.name)."},
			{"<url>", "Une URL HTTP ou HTTPS complète."},
			{"<fichier>", "Un chemin vers un fichier existant, par exemple le certificat CA TLS."},
			{"<port>", "Un numéro de port TCP local compris entre 1 et 65535."},
		},
	},
	{
		"INSPECTION ET ENVOI",
		[]helpEntry{
			{"crl.show [<id>|active|selected]", "Afficher le constructeur, une capture ou la sélection"},
			{"crl.show <id> body [offset]", "Afficher une page du corps de requête"},
			{"crl.show active body [offset]", "Afficher une page du corps actif"},
			{"crl.table", "Afficher un résumé du constructeur ou de la sélection"},
			{"crl.output [auto|raw|pretty]", "Choisir le rendu des réponses"},
			{"crl.launch [<id>]", "Rejouer uniquement cette capture, sans changer la sélection"},
			{"crl.launch", "Rejouer la sélection si elle existe, sinon envoyer le constructeur actif"},
			{"crl.launch active", "Envoyer le constructeur actif"},
			{"crl.launch all", "Refusé : utilisez mint.select all, puis crl.launch pour rejouer toute la sélection"},
			{"crl.help", "Afficher l'aide détaillée des commandes CRL"},
		},
	},
	{
		"SESSION",
		[]helpEntry{
			{"reset", "Réinitialiser le constructeur actif"},
			{"clear", "Effacer l'écran interactif"},
			{"help [commande]", "Afficher l'aide générale ou détaillée"},
			{"exit | quit", "Quitter et arrêter proprement Mint"},
		},
	},
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
		for _, e := range group.entries {
			syntax := p.Cyan(e.syntax)
			desc := p.Dim(e.description)
			combined := "  " + e.syntax + "  " + e.description
			if displayWidth(combined) > width-2 {
				fmt.Fprintf(out, "  %s\n", syntax)
				for _, l := range wrapWords(e.description, width-4) {
					fmt.Fprintf(out, "    %s\n", p.Dim(l))
				}
			} else {
				fmt.Fprintf(out, "  %s  %s\n", syntax, desc)
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
