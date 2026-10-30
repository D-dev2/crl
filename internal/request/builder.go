// Package request construit et prépare les requêtes HTTP exécutées par curl.
package request

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Header est un en-tête HTTP ordonné. Les doublons sont conservés.
type Header struct {
	Name  string
	Value string
}

// CapturedRequest est la projection d'une capture Mint utilisée pour
// l'importation. Ce type évite au paquet request de dépendre de Mint.
type CapturedRequest struct {
	URL                   string
	Method                string
	Headers               []Header
	Body                  []byte
	BodyPresent           bool
	BodyTruncated         bool
	PendingReply          bool
	ResponseBodyTruncated bool
}

// ProxyResolution décrit le proxy réellement utilisé par curl.
type ProxyResolution struct {
	URL    string
	Source string
	// None signifie que curl doit ignorer explicitement tout proxy.
	None bool
}

// Builder contient l'état du constructeur de requêtes CRL-CLI.
type Builder struct {
	method  string
	rawURL  string
	headers []Header
	body    []byte
	bodySet bool
	proxy   string // "", "none", ou URL explicite
	caFile  string
}

// New crée un constructeur vide (GET, sans URL).
func New() *Builder {
	return &Builder{method: "GET"}
}

// Method retourne la méthode HTTP.
func (b *Builder) Method() string { return b.method }

// SetMethod définit la méthode HTTP.
func (b *Builder) SetMethod(method string) error {
	m, err := normalizeMethod(method)
	if err != nil {
		return err
	}
	b.method = m
	return nil
}

// URL retourne l'URL brute, avec son encodage et sa chaîne de requête exacts.
func (b *Builder) URL() string { return b.rawURL }

// ClearURL retire l'URL cible sans toucher aux en-têtes, au corps ni au proxy.
func (b *Builder) ClearURL() { b.rawURL = "" }

// SetURL définit l'URL cible. Si le schéma est absent, http:// est ajouté.
// L'URL n'est jamais reconstruite à partir de composants : les caractères
// encodés, les paramètres répétés et les valeurs vides sont conservés.
func (b *Builder) SetURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("URL vide")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return fmt.Errorf("URL contenant un espace ou un caractère de contrôle")
		}
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL invalide : %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("schéma %q non pris en charge (attendu http ou https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL sans hôte")
	}
	b.rawURL = raw
	return nil
}

// Headers retourne une copie des en-têtes dans l'ordre.
func (b *Builder) Headers() []Header {
	out := make([]Header, len(b.headers))
	copy(out, b.headers)
	return out
}

// AddHeader ajoute un en-tête. Un en-tête existant du même nom (insensible à
// la casse) est remplacé afin d'éviter les doublons accidentels.
func (b *Builder) AddHeader(name, value string) error {
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if err := ValidateHeader(name, value); err != nil {
		return err
	}
	b.RemoveHeader(name)
	b.headers = append(b.headers, Header{Name: name, Value: value})
	return nil
}

// SetHeader remplace tous les en-têtes du nom donné par une seule valeur.
func (b *Builder) SetHeader(name, value string) error {
	return b.AddHeader(name, value)
}

// RemoveHeader supprime tous les en-têtes portant ce nom.
func (b *Builder) RemoveHeader(name string) int {
	removed := 0
	out := b.headers[:0]
	for _, h := range b.headers {
		if strings.EqualFold(h.Name, name) {
			removed++
			continue
		}
		out = append(out, h)
	}
	b.headers = out
	return removed
}

// ClearHeaders supprime tous les en-têtes.
func (b *Builder) ClearHeaders() { b.headers = nil }

// SetJSON définit un corps JSON et le Content-Type correspondant.
func (b *Builder) SetJSON(data string) error {
	if strings.TrimSpace(data) == "" {
		return fmt.Errorf("corps JSON vide")
	}
	if !json.Valid([]byte(data)) {
		return fmt.Errorf("JSON invalide")
	}
	b.body = []byte(data)
	b.bodySet = true
	return b.SetHeader("Content-Type", "application/json")
}

// SetBody définit un corps brut, sans modifier le Content-Type.
func (b *Builder) SetBody(body []byte) {
	b.body = append([]byte(nil), body...)
	b.bodySet = true
}

// SetBodyText est la variante texte de SetBody.
func (b *Builder) SetBodyText(body string) {
	b.SetBody([]byte(body))
}

// ClearBody supprime le corps.
func (b *Builder) ClearBody() {
	b.body = nil
	b.bodySet = false
}

// Body retourne une copie du corps.
func (b *Builder) Body() []byte {
	out := make([]byte, len(b.body))
	copy(out, b.body)
	return out
}

// BodySet indique si un corps a été explicitement défini.
func (b *Builder) BodySet() bool { return b.bodySet }

// BodySize retourne la taille du corps.
func (b *Builder) BodySize() int { return len(b.body) }

// ContentType retourne le Content-Type courant, s'il existe.
func (b *Builder) ContentType() string {
	for _, h := range b.headers {
		if strings.EqualFold(h.Name, "Content-Type") {
			return h.Value
		}
	}
	return ""
}

// Proxy retourne le proxy explicite : "", "none" ou une URL.
func (b *Builder) Proxy() string { return b.proxy }

// SetProxy configure un proxy explicite distinct du proxy Mint automatique.
// Les valeurs "none" et "off" désactivent explicitement tout proxy.
func (b *Builder) SetProxy(value string) error {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "":
		b.proxy = ""
		return nil
	case "none", "off":
		b.proxy = "none"
		return nil
	}
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("proxy invalide : %w", err)
	}
	switch u.Scheme {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return fmt.Errorf("schéma de proxy %q non pris en charge", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("proxy sans hôte")
	}
	b.proxy = value
	return nil
}

// ClearProxy supprime le proxy explicite. Le proxy Mint automatique redevient
// applicable s'il est actif.
func (b *Builder) ClearProxy() { b.proxy = "" }

// CAFile retourne le certificat CA utilisé pour curl.
func (b *Builder) CAFile() string { return b.caFile }

// SetCAFile configure un certificat CA pour curl. La vérification TLS reste
// active : ce fichier remplace simplement le bundle de confiance.
func (b *Builder) SetCAFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("chemin de certificat vide")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("certificat CA introuvable : %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("%q est un répertoire, pas un fichier PEM", path)
	}
	b.caFile = path
	return nil
}

// ClearCAFile revient au bundle de confiance par défaut de curl.
func (b *Builder) ClearCAFile() { b.caFile = "" }

// IsEmpty indique qu'aucune configuration significative n'a été préparée.
func (b *Builder) IsEmpty() bool {
	return b.rawURL == "" && len(b.headers) == 0 && !b.bodySet && b.proxy == "" && b.caFile == "" && (b.method == "" || b.method == "GET")
}

// Reset remet le constructeur dans son état initial.
func (b *Builder) Reset() {
	*b = Builder{method: "GET"}
}

// Import remplit le constructeur depuis une capture. Aucune requête n'est
// envoyée. Les en-têtes, le corps et l'URL sont copiés à l'identique.
func (b *Builder) Import(c CapturedRequest) error {
	if c.BodyTruncated {
		return fmt.Errorf("capture incomplète : le corps de la requête est tronqué, importation refusée")
	}
	if err := b.SetURL(c.URL); err != nil {
		return err
	}
	if err := b.SetMethod(c.Method); err != nil {
		return err
	}
	b.headers = nil
	for _, h := range c.Headers {
		name := strings.TrimSpace(h.Name)
		value := h.Value
		if err := ValidateHeader(name, value); err != nil {
			return fmt.Errorf("en-tête capturé invalide (%s) : %w", name, err)
		}
		// Les en-têtes sont conservés tels quels, doublons compris.
		b.headers = append(b.headers, Header{Name: name, Value: value})
	}
	if c.BodyPresent {
		b.body = append([]byte(nil), c.Body...)
		b.bodySet = true
	} else {
		b.body = nil
		b.bodySet = false
	}
	return nil
}

// ResolveProxy applique la priorité : proxy explicite, puis proxy Mint actif,
// puis comportement de curl (variables d'environnement).
func (b *Builder) ResolveProxy(mintURL string, mintActive bool) ProxyResolution {
	switch {
	case b.proxy == "none":
		return ProxyResolution{None: true, Source: "explicite, proxy désactivé (crl.proxy none)"}
	case b.proxy != "":
		return ProxyResolution{URL: b.proxy, Source: "explicite (crl.proxy)"}
	case mintActive && mintURL != "":
		return ProxyResolution{URL: mintURL, Source: "Mint (automatique)"}
	default:
		return ProxyResolution{Source: "non configuré (curl utilisera son environnement)"}
	}
}

// CurlArgs construit les arguments curl garantissant la fidélité de l'URL et
// l'usage effectif du proxy choisi. Les en-têtes sensibles ne sont pas
// journalisés ; ils sont passés à curl sans passer par le shell.
func (b *Builder) CurlArgs(res ProxyResolution) ([]string, error) {
	if b.rawURL == "" {
		return nil, fmt.Errorf("aucune URL configurée : utilisez crl.host <url>")
	}
	args := []string{
		"--globoff",
		"--path-as-is",
		"-sS",
		"-X", b.method,
	}
	if res.None {
		args = append(args, "--noproxy", "*")
	} else if res.URL != "" {
		args = append(args, "--proxy", res.URL, "--noproxy", "")
	}
	if b.caFile != "" {
		args = append(args, "--cacert", b.caFile)
	}

	hasContentType := false
	for _, h := range b.headers {
		lname := strings.ToLower(h.Name)
		switch lname {
		case "content-length", "transfer-encoding", "connection", "proxy-connection", "expect":
			// Ces en-têtes sont recalculés ou interdits par curl.
			continue
		case "content-type":
			hasContentType = true
		}
		args = append(args, "-H", h.Name+": "+h.Value)
	}
	if b.bodySet {
		if !hasContentType {
			// Empêche curl d'ajouter application/x-www-form-urlencoded.
			args = append(args, "-H", "Content-Type:")
		}
		args = append(args, "--data-binary", "@-")
	}
	args = append(args, b.rawURL)
	return args, nil
}

// BodyReader retourne un lecteur sur le corps, pour stdin de curl.
func (b *Builder) BodyReader() *bytes.Reader {
	return bytes.NewReader(b.body)
}

// ValidateHeader vérifie la syntaxe d'un en-tête.
func ValidateHeader(name, value string) error {
	if name == "" {
		return fmt.Errorf("nom d'en-tête vide")
	}
	if len(name) > 256 {
		return fmt.Errorf("nom d'en-tête trop long")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return fmt.Errorf("nom d'en-tête invalide %q", name)
		}
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("valeur d'en-tête contenant un caractère interdit")
	}
	return nil
}

// ParseHeaderLine découpe "Nom: valeur".
func ParseHeaderLine(line string) (string, string, error) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", fmt.Errorf("format attendu : Nom: valeur")
	}
	name := strings.TrimSpace(line[:i])
	value := strings.TrimSpace(line[i+1:])
	if err := ValidateHeader(name, value); err != nil {
		return "", "", err
	}
	return name, value, nil
}

func normalizeMethod(method string) (string, error) {
	m := strings.ToUpper(strings.TrimSpace(method))
	if m == "" {
		return "", fmt.Errorf("méthode HTTP vide")
	}
	for i, r := range m {
		switch {
		case r >= 'A' && r <= 'Z':
		case i > 0 && (r == '-' || r == '_' || (r >= '0' && r <= '9')):
		default:
			return "", fmt.Errorf("méthode HTTP invalide %q", method)
		}
	}
	return m, nil
}

// MaskProxyCredentials masque un éventuel mot de passe dans une URL de proxy.
func MaskProxyCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return raw
	}
	u.User = url.UserPassword(u.User.Username(), "***")
	return u.String()
}
