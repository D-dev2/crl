// Package response analyse et met en forme les réponses HTTP reçues par curl.
//
// Le formatage ne concerne que l'affichage : les octets du corps et les
// en-têtes utilisés pour le diagnostic ne sont jamais réécrits sur disque ni
// envoyés à nouveau.
package response

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Mode contrôle le formatage du corps affiché.
type Mode string

const (
	ModeAuto   Mode = "auto"
	ModeRaw    Mode = "raw"
	ModePretty Mode = "pretty"
)

// ParseMode valide un mode de sortie. Les alias français sont acceptés.
func ParseMode(raw string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return ModeAuto, nil
	case "raw", "brut", "brute":
		return ModeRaw, nil
	case "pretty", "joli", "lisible":
		return ModePretty, nil
	default:
		return "", fmt.Errorf("mode de sortie invalide %q : utilisez auto, raw ou pretty", raw)
	}
}

// Label rend le mode avec une courte explication.
func (m Mode) Label() string {
	switch m {
	case ModeRaw:
		return "raw — corps brut, sans indentation"
	case ModePretty:
		return "pretty — JSON indenté et HTML aéré"
	default:
		return "auto — JSON indenté et HTML aéré"
	}
}

// ShortLabel rend le mode seul.
func (m Mode) ShortLabel() string {
	if m == "" {
		return string(ModeAuto)
	}
	return string(m)
}

// Header est un en-tête de réponse ordonné.
type Header struct {
	Name  string
	Value string
}

// Headers conserve l'ordre et les doublons des en-têtes de réponse.
type Headers []Header

// Get retourne la première valeur d'un en-tête, sans tenir compte de la casse.
func (h Headers) Get(name string) string {
	for _, header := range h {
		if strings.EqualFold(header.Name, name) {
			return header.Value
		}
	}
	return ""
}

// ContentType retourne le Content-Type déclaré, sans ses paramètres.
func (h Headers) ContentType() string {
	value := h.Get("Content-Type")
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(value)
}

// Status décrit la ligne de statut HTTP finale.
type Status struct {
	Code   int
	Reason string
	Proto  string
}

// Label rend le statut sans inventer de code.
func (s Status) Label() string {
	if s.Code == 0 {
		return ""
	}
	if s.Reason == "" {
		return strconv.Itoa(s.Code)
	}
	return fmt.Sprintf("%d %s", s.Code, s.Reason)
}

type headerBlock struct {
	status  Status
	headers Headers
	connect bool
}

// ParseHeaderDump analyse la sortie de curl --dump-header. Les blocs
// intermédiaires (100 Continue, CONNECT du proxy, redirections) sont ignorés
// au profit de la dernière réponse HTTP réelle.
func ParseHeaderDump(data []byte) (Status, Headers) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	var blocks []headerBlock
	var current *headerBlock
	flush := func() {
		if current != nil {
			blocks = append(blocks, *current)
			current = nil
		}
	}

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(strings.ToUpper(line), "HTTP/") {
			flush()
			status, ok := parseStatusLine(line)
			if ok {
				current = &headerBlock{
					status:  status,
					connect: strings.Contains(strings.ToLower(status.Reason), "connection established"),
				}
			}
			continue
		}
		if current == nil {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if n := len(current.headers); n > 0 {
				current.headers[n-1].Value += " " + strings.TrimSpace(line)
			}
			continue
		}
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		current.headers = append(current.headers, Header{
			Name:  strings.TrimSpace(line[:i]),
			Value: strings.TrimSpace(line[i+1:]),
		})
	}
	flush()

	if len(blocks) == 0 {
		return Status{}, nil
	}
	// Une réponse finale a normalement un code >= 200. On ignore les blocs
	// CONNECT du proxy : un échec TLS après « 200 Connection established » ne
	// doit pas être présenté comme une réponse HTTP du serveur.
	chosen := headerBlock{}
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].connect {
			continue
		}
		chosen = blocks[i]
		if blocks[i].status.Code >= 200 {
			break
		}
	}
	return chosen.status, chosen.headers
}

func parseStatusLine(line string) (Status, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return Status{}, false
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil || code < 100 || code > 999 {
		return Status{}, false
	}
	return Status{
		Code:   code,
		Reason: strings.Join(fields[2:], " "),
		Proto:  fields[0],
	}, true
}

// CurlMeta contient les métadonnées écrites par curl sur stdout.
type CurlMeta struct {
	HTTPCode     int
	Duration     time.Duration
	Size         int64
	EffectiveURL string
}

const (
	metaStatus = "__CRL_STATUS__"
	metaTime   = "__CRL_DURATION__"
	metaSize   = "__CRL_SIZE__"
	metaURL    = "__CRL_URL__"
)

// WriteOutFormat est le format --write-out utilisé par le shell. Les marqueurs
// sont uniques pour résister aux messages éventuels de curl.
const WriteOutFormat = "\n" + metaStatus + "%{http_code}\n" +
	metaTime + "%{time_total}\n" +
	metaSize + "%{size_download}\n" +
	metaURL + "%{url_effective}\n"

// ParseCurlMeta analyse les métadonnées de WriteOutFormat.
func ParseCurlMeta(out []byte) CurlMeta {
	text := strings.ReplaceAll(string(out), "\r\n", "\n")
	var meta CurlMeta
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, metaStatus):
			meta.HTTPCode, _ = strconv.Atoi(strings.TrimPrefix(line, metaStatus))
		case strings.HasPrefix(line, metaTime):
			seconds, err := strconv.ParseFloat(strings.TrimPrefix(line, metaTime), 64)
			if err == nil && seconds >= 0 {
				meta.Duration = time.Duration(seconds * float64(time.Second))
			}
		case strings.HasPrefix(line, metaSize):
			meta.Size, _ = strconv.ParseInt(strings.TrimPrefix(line, metaSize), 10, 64)
		case strings.HasPrefix(line, metaURL):
			meta.EffectiveURL = strings.TrimPrefix(line, metaURL)
		}
	}
	return meta
}

// BodyKind qualifie le contenu affiché.
type BodyKind string

const (
	BodyEmpty  BodyKind = "empty"
	BodyText   BodyKind = "text"
	BodyJSON   BodyKind = "json"
	BodyHTML   BodyKind = "html"
	BodyBinary BodyKind = "binary"
)

// BodyView est le résultat d'un rendu de corps.
type BodyView struct {
	Text      string
	Kind      BodyKind
	Truncated bool
	Total     int64
}

// RenderBody met en forme un aperçu de corps pour le terminal. total est la
// taille réelle reçue ; body peut être plus court lorsque l'affichage est
// borné. Aucune donnée n'est modifiée sur disque.
func RenderBody(body []byte, total int64, limit int, mode Mode, contentType string) BodyView {
	if total < 0 {
		total = int64(len(body))
	}
	if limit > 0 && len(body) > limit {
		body = body[:limit]
	}
	truncated := total > int64(len(body))
	if truncated {
		body = trimUTF8(body)
	}
	view := BodyView{Truncated: truncated, Total: total}
	if total == 0 && len(body) == 0 {
		view.Kind = BodyEmpty
		view.Text = "(corps vide)"
		return view
	}
	if hasBinaryControl(body) || (len(body) > 0 && !utf8.Valid(body)) {
		view.Kind = BodyBinary
		view.Text = fmt.Sprintf("[corps binaire : %d octets, non affiché]", total)
		return view
	}

	contentType = strings.ToLower(strings.TrimSpace(contentType))
	looksJSON := json.Valid(body)
	looksHTML := strings.Contains(contentType, "html") || looksLikeHTML(body)
	switch {
	case looksJSON:
		view.Kind = BodyJSON
	case looksHTML:
		view.Kind = BodyHTML
	default:
		view.Kind = BodyText
	}

	if mode == ModeRaw {
		view.Text = string(body)
		return view
	}
	// Une réponse tronquée ne peut pas être reformatée de façon fiable : on
	// affiche le préfixe tel quel et l'appelant signale explicitement la coupe.
	if truncated {
		view.Text = string(body)
		return view
	}
	switch {
	case looksJSON:
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err == nil {
			view.Text = pretty.String()
			return view
		}
		view.Text = string(body)
	case looksHTML:
		view.Text = formatHTML(string(body))
	default:
		view.Text = string(body)
	}
	return view
}

// trimUTF8 retire une éventuelle séquence UTF-8 incomplète en fin d'aperçu.
func trimUTF8(body []byte) []byte {
	for len(body) > 0 && !utf8.Valid(body) {
		body = body[:len(body)-1]
	}
	return body
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

func looksLikeHTML(body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"<!doctype html", "<html", "<head", "<body"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func isRawTextTag(name string) bool {
	switch name {
	case "pre", "script", "style", "textarea":
		return true
	default:
		return false
	}
}

func isVoidTag(name string) bool {
	switch name {
	case "area", "base", "br", "col", "embed", "hr", "img", "input",
		"link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

// formatHTML aère le HTML pour le terminal sans prétendre produire du JSON.
// Le contenu est uniquement destiné à l'affichage : les octets d'origine ne
// sont jamais remplacés.
func formatHTML(src string) string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	var lines []string
	indent := 0
	add := func(text string) {
		text = strings.TrimRight(text, " \t")
		if text == "" {
			return
		}
		lines = append(lines, strings.Repeat("  ", indent)+text)
	}
	addRaw := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		lines = append(lines, text)
	}

	for i := 0; i < len(src); {
		if src[i] != '<' {
			j := strings.IndexByte(src[i:], '<')
			if j < 0 {
				j = len(src) - i
			}
			text := strings.Join(strings.Fields(src[i:i+j]), " ")
			if text != "" {
				add(text)
			}
			i += j
			continue
		}
		if strings.HasPrefix(src[i:], "<!--") {
			end := strings.Index(src[i+4:], "-->")
			if end < 0 {
				add(src[i:])
				break
			}
			end = i + 4 + end + 3
			add(src[i:end])
			i = end
			continue
		}
		end := tagEnd(src, i+1)
		if end < 0 {
			add(src[i:])
			break
		}
		tag := src[i : end+1]
		name, closing, selfClosing, ok := tagInfo(tag)
		if !ok {
			add(tag)
			i = end + 1
			continue
		}
		if closing {
			if indent > 0 {
				indent--
			}
			add(tag)
			i = end + 1
			continue
		}
		add(tag)
		if selfClosing || isVoidTag(name) {
			i = end + 1
			continue
		}
		if isRawTextTag(name) {
			needle := "</" + name
			idx := indexFold(src[end+1:], needle)
			if idx < 0 {
				addRaw(strings.TrimSpace(src[end+1:]))
				break
			}
			contentStart := end + 1
			contentEnd := contentStart + idx
			addRaw(src[contentStart:contentEnd])
			closeEnd := tagEnd(src, contentEnd+1)
			if closeEnd < 0 {
				add(src[contentEnd:])
				break
			}
			add(src[contentEnd : closeEnd+1])
			i = closeEnd + 1
			continue
		}
		indent++
		i = end + 1
	}
	return strings.Join(lines, "\n")
}

func tagEnd(src string, start int) int {
	var quote byte
	for i := start; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '>':
			return i
		}
	}
	return -1
}

func tagInfo(tag string) (name string, closing, selfClosing, ok bool) {
	if len(tag) < 2 || tag[0] != '<' {
		return "", false, false, false
	}
	inner := tag[1:]
	if inner == "" || inner[0] == '!' || inner[0] == '?' {
		return "", false, false, false
	}
	if inner[0] == '/' {
		closing = true
		inner = inner[1:]
	}
	j := 0
	for j < len(inner) {
		c := inner[j]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == ':' {
			j++
			continue
		}
		break
	}
	if j == 0 {
		return "", false, false, false
	}
	name = strings.ToLower(inner[:j])
	rest := strings.TrimSpace(inner[j:])
	selfClosing = strings.HasSuffix(rest, "/")
	return name, closing, selfClosing, true
}

func indexFold(haystack, needle string) int {
	return strings.Index(strings.ToLower(haystack), strings.ToLower(needle))
}

// CurlFailure décrit une erreur de transport curl. Un statut HTTP 4xx ou 5xx
// n'est jamais transformé en erreur de transport.
type CurlFailure struct {
	Code   int
	Label  string
	Detail string
}

// ClassifyCurlFailure traduit un code de sortie curl en libellé lisible.
func ClassifyCurlFailure(code int, stderr string) CurlFailure {
	label := "erreur curl"
	switch code {
	case 6:
		label = "nom d'hôte introuvable"
	case 7:
		label = "connexion impossible"
	case 18:
		label = "transfert partiel"
	case 23:
		label = "écriture locale impossible"
	case 28:
		label = "délai dépassé"
	case 35:
		label = "négociation TLS échouée"
	case 47:
		label = "trop de redirections"
	case 52:
		label = "réponse vide du serveur"
	case 55:
		label = "envoi interrompu"
	case 56:
		label = "réception interrompue"
	case 58:
		label = "certificat client local invalide"
	case 59:
		label = "chiffrement TLS indisponible"
	case 60:
		label = "certificat TLS non approuvé"
	case 130:
		label = "interrompu"
	}
	return CurlFailure{
		Code:   code,
		Label:  label,
		Detail: strings.TrimSpace(stderr),
	}
}

// IsTimeout indique si l'échec curl correspond à un dépassement de délai.
func (f CurlFailure) IsTimeout() bool { return f.Code == 28 }

// IsConnection indique si l'échec curl concerne la connexion réseau.
func (f CurlFailure) IsConnection() bool {
	switch f.Code {
	case 6, 7, 35, 52, 55, 56, 58, 59, 60:
		return true
	default:
		return false
	}
}
