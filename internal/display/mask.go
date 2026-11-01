// Package display centralise les règles d'affichage de CRL-CLI :
// rendu des URL et des corps HTTP, et vues récapitulatives.
//
// Les vues détaillées de CRL-CLI affichent les données telles qu'elles ont été
// préparées, capturées ou reçues. Aucune valeur d'en-tête, de cookie, d'URL ou
// de corps JSON n'y est masquée. Les fonctions *Summary* ne servent qu'aux
// tableaux compacts, jamais aux vues détaillées.
//
// Ces fonctions ne modifient jamais les données utilisées pour rejouer une
// requête : elles ne travaillent que sur des copies destinées au terminal.
package display

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// sensitiveHeaderFragments contient les fragments de noms d'en-tête qui
// déclenchent un résumé masqué. La comparaison est insensible à la casse.
var sensitiveHeaderFragments = []string{
	"authorization",
	"cookie",
	"token",
	"secret",
	"password",
	"passwd",
	"api-key",
	"apikey",
	"session",
	"private-key",
	"client-key",
	"x-amz-security",
	"x-goog-api-key",
}

// sensitiveQueryKeys contient les noms de paramètres dont la valeur est
// résumée dans les tableaux compacts. Les clés sont comparées en minuscules.
var sensitiveQueryKeys = map[string]struct{}{
	"access_token":         {},
	"access-token":         {},
	"api_key":              {},
	"api-key":              {},
	"apikey":               {},
	"auth":                 {},
	"authorization":        {},
	"client_secret":        {},
	"client-secret":        {},
	"code":                 {},
	"id_token":             {},
	"id-token":             {},
	"key":                  {},
	"passwd":               {},
	"password":             {},
	"pwd":                  {},
	"refresh_token":        {},
	"refresh-token":        {},
	"secret":               {},
	"session":              {},
	"session_id":           {},
	"sessionid":            {},
	"sig":                  {},
	"signature":            {},
	"token":                {},
	"x-amz-signature":      {},
	"x-amz-credential":     {},
	"x-amz-security-token": {},
}

// HeaderSensitive indique si un nom d'en-tête est habituellement porteur d'un
// secret. Cette classification ne sert qu'aux résumés, jamais à masquer une vue
// détaillée.
func HeaderSensitive(name string) bool {
	lname := strings.ToLower(strings.TrimSpace(name))
	if lname == "" {
		return false
	}
	for _, fragment := range sensitiveHeaderFragments {
		if strings.Contains(lname, fragment) {
			return true
		}
	}
	return false
}

// MaskHeaderValue retourne la valeur intacte. Le nom est conservé dans la
// signature pour les appelants historiques ; les vues détaillées ne masquent
// plus aucune valeur.
func MaskHeaderValue(name, value string) string {
	return value
}

// HeaderLine rend une ligne d'en-tête complète, sans masquage.
func HeaderLine(name, value string) string {
	return name + ": " + MaskHeaderValue(name, value)
}

// QuerySensitive indique si un nom de paramètre de requête est habituellement
// porteur d'un secret. Cette classification ne sert qu'aux tableaux compacts.
func QuerySensitive(name string) bool {
	lname := strings.ToLower(strings.TrimSpace(name))
	if lname == "" {
		return false
	}
	if _, ok := sensitiveQueryKeys[lname]; ok {
		return true
	}
	for _, fragment := range []string{"token", "secret", "password", "passwd", "session", "apikey", "api_key", "api-key"} {
		if strings.Contains(lname, fragment) {
			return true
		}
	}
	return false
}

// MaskURL retourne l'URL intacte. Les vues détaillées ne masquent plus les
// paramètres sensibles ; SummaryURL reste disponible pour les tableaux.
func MaskURL(raw string) string {
	return raw
}

// SummaryURL masque uniquement les valeurs des paramètres sensibles d'une URL
// pour un tableau récapitulatif. La structure brute de l'URL (encodage,
// paramètres répétés, valeurs vides, ordre) est préservée.
func SummaryURL(raw string) string {
	q := strings.IndexByte(raw, '?')
	if q < 0 {
		return raw
	}
	query := raw[q+1:]
	fragment := ""
	if h := strings.IndexByte(query, '#'); h >= 0 {
		fragment = query[h:]
		query = query[:h]
	}
	if query == "" {
		return raw
	}
	segments := strings.Split(query, "&")
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		name := segment
		if eq := strings.IndexByte(segment, '='); eq >= 0 {
			name = segment[:eq]
		}
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			decoded = name
		}
		if !QuerySensitive(decoded) {
			continue
		}
		segments[i] = name + "=***"
	}
	return raw[:q+1] + strings.Join(segments, "&") + fragment
}

func RenderBody(body []byte, limit int) (string, bool) {
	if len(body) == 0 {
		return "(corps vide)", false
	}
	if !utf8.Valid(body) || hasBinaryControl(body) {
		return fmt.Sprintf("[corps binaire : %d octets, non affiché]", len(body)), false
	}
	if limit > 0 && len(body) > limit {
		return string(body[:limit]) + fmt.Sprintf("\n[... tronqué pour l'affichage : %d octets au total]", len(body)), true
	}
	if json.Valid(body) {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err == nil {
			return pretty.String(), false
		}
	}
	return string(body), false
}

// MaskBody retourne une copie intacte du corps. Le nom est conservé pour les
// appelants historiques : l'affichage local doit montrer les données réelles.
func MaskBody(body []byte) []byte {
	return append([]byte(nil), body...)
}

func hasBinaryControl(body []byte) bool {
	for _, b := range body {
		if b == 0 {
			return true
		}
		if b < 0x20 {
			switch b {
			case '\n', '\r', '\t':
				continue
			}
			return true
		}
	}
	return false
}

// FormatBytes rend une taille lisible par un humain.
func FormatBytes(n int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f Gio", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.1f Mio", float64(n)/mib)
	case n >= kib:
		return fmt.Sprintf("%.1f Kio", float64(n)/kib)
	default:
		return fmt.Sprintf("%d octets", n)
	}
}
