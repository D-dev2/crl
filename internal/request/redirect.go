// Package request : règles de suivi des redirections HTTP.
package request

import (
	"fmt"
	"net/url"
	"strings"
)

// DefaultMaxRedirects est la limite appliquée par crl.launch et mint.replay
// pour éviter les boucles de redirection.
const DefaultMaxRedirects = 10

// FollowableRedirect indique si CRL-CLI suit automatiquement ce statut.
func FollowableRedirect(code int) bool {
	switch code {
	case 301, 302, 303, 307, 308:
		return true
	default:
		return false
	}
}

// RedirectMethodBody calcule la méthode et la conservation du corps pour la
// requête suivante :
//   - 301 et 302 : un POST/PUT/etc. devient GET et le corps est abandonné ;
//     GET et HEAD conservent leur méthode et leur éventuel corps ;
//   - 303 : devient GET, sauf HEAD qui reste HEAD, et le corps est abandonné ;
//   - 307 et 308 : méthode et corps sont conservés à l'identique.
func RedirectMethodBody(code int, method string) (string, bool) {
	m := strings.ToUpper(strings.TrimSpace(method))
	switch code {
	case 303:
		if m == "HEAD" {
			return "HEAD", false
		}
		return "GET", false
	case 301, 302:
		if m == "GET" || m == "HEAD" {
			return m, true
		}
		return "GET", false
	default:
		return m, true
	}
}

// ResolveRedirect résout une valeur Location absolue ou relative contre l'URL
// courante. Seuls les schémas http et https sont acceptés.
func ResolveRedirect(base, location string) (string, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", fmt.Errorf("en-tête Location vide")
	}
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return "", fmt.Errorf("URL de base invalide %q", base)
	}
	ref, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("Location invalide : %w", err)
	}
	target := baseURL.ResolveReference(ref)
	if target.Scheme != "http" && target.Scheme != "https" {
		return "", fmt.Errorf("redirection vers le schéma %q non prise en charge", target.Scheme)
	}
	if target.Host == "" {
		return "", fmt.Errorf("redirection sans hôte")
	}
	return target.String(), nil
}

// RedirectKey normalise une URL pour détecter une boucle de redirection. Le
// fragment est ignoré, le schéma et l'hôte sont comparés sans tenir compte de
// la casse, et le port effectif est rendu explicite.
func RedirectKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return originOf(u) + path + "?" + u.RawQuery
}

// SameOrigin compare le schéma, l'hôte et le port effectif des deux URL.
func SameOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil || ua.Scheme == "" || ua.Host == "" {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil || ub.Scheme == "" || ub.Host == "" {
		return false
	}
	return originOf(ua) == originOf(ub)
}

func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + host + ":" + port
}

// crossOriginCredentialHeaders liste les en-têtes qui ne doivent jamais être
// transmis automatiquement à une autre origine lors d'une redirection.
var crossOriginCredentialHeaders = []string{
	"Authorization",
	"Proxy-Authorization",
	"Cookie",
	"Cookie2",
	"X-Api-Key",
}

// StripCrossOriginCredentials retire les identifiants de la requête suivante
// lorsque la redirection change d'origine. La requête source n'est pas modifiée.
func StripCrossOriginCredentials(b *Builder) {
	if b == nil {
		return
	}
	for _, name := range crossOriginCredentialHeaders {
		b.RemoveHeader(name)
	}
}

// DropBodyHeaders retire les en-têtes qui décrivent un corps abandonné.
func DropBodyHeaders(b *Builder) {
	if b == nil {
		return
	}
	for _, name := range []string{
		"Content-Type",
		"Content-Encoding",
		"Content-Language",
		"Content-Length",
		"Transfer-Encoding",
	} {
		b.RemoveHeader(name)
	}
}
