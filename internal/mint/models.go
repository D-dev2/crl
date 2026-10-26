package mint

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Header est un en-tête HTTP ordonné. Les doublons sont conservés.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// RequestData représente la requête HTTP d'une capture.
type RequestData struct {
	Method      string
	URL         string
	Scheme      string
	Host        string
	Path        string
	RawQuery    string
	HTTPVersion string
	Headers     []Header
	Body        []byte
	// BodySize est la taille d'origine annoncée par l'addon. Elle peut être
	// supérieure à len(Body) lorsque BodyTruncated est vrai.
	BodySize      int64
	BodyTruncated bool
	// Received distingue une requête réellement reçue d'un enregistrement
	// créé par une réponse arrivée anormalement tôt.
	Received bool
}

// ResponseData représente la réponse HTTP d'une capture.
type ResponseData struct {
	StatusCode    int
	Reason        string
	HTTPVersion   string
	Headers       []Header
	Body          []byte
	BodySize      int64
	BodyTruncated bool
}

// Capture est un échange HTTP conservé en mémoire. L'ID est stable pendant
// toute la session : il ne dépend pas de la position dans un tableau.
type Capture struct {
	ID        int
	FlowID    string
	StartedAt time.Time
	Request   RequestData
	Response  *ResponseData
	Duration  time.Duration
	Err       string
}

// RequestComplete indique si la partie requête peut être rejouée sans perte.
func (c Capture) RequestComplete() bool {
	if !c.Request.Received || c.Request.Method == "" || c.Request.URL == "" {
		return false
	}
	return !c.Request.BodyTruncated
}

// ResponseComplete indique si une réponse a été reçue et n'a pas été tronquée.
func (c Capture) ResponseComplete() bool {
	return c.Response != nil && !c.Response.BodyTruncated
}

// Importable indique si la capture peut alimenter le constructeur CRL-CLI.
// La réponse peut être absente (requête encore en attente) : cela n'empêche
// pas de rejouer la requête, mais l'utilisateur en est averti.
func (c Capture) Importable() (bool, string) {
	if !c.Request.Received {
		return false, "la requête n'a pas encore été reçue"
	}
	if c.Request.BodyTruncated {
		return false, fmt.Sprintf(
			"le corps de la requête est incomplet (%d octets annoncés, %d conservés) : importation refusée",
			c.Request.BodySize, len(c.Request.Body))
	}
	if c.Request.Method == "" {
		return false, "la méthode HTTP est absente"
	}
	if c.Request.URL == "" {
		return false, "l'URL est absente"
	}
	return true, ""
}

// PathAndQuery retourne le chemin et la chaîne de requête sans perdre
// l'encodage ni l'ordre des paramètres.
func (c Capture) PathAndQuery() string {
	path := c.Request.Path
	if path == "" {
		path = "/"
	}
	if c.Request.RawQuery != "" {
		return path + "?" + c.Request.RawQuery
	}
	return path
}

// StatusLabel rend le statut destiné aux tableaux.
func (c Capture) StatusLabel() string {
	switch {
	case c.Err != "":
		return "ERROR"
	case c.Response != nil:
		return strconv.Itoa(c.Response.StatusCode)
	default:
		return "PENDING"
	}
}

// DurationLabel rend la durée d'un échange, ou "-" si elle est inconnue.
func (c Capture) DurationLabel() string {
	if c.Duration <= 0 {
		if c.Response != nil {
			return "0ms"
		}
		return "-"
	}
	if c.Duration < time.Millisecond {
		return fmt.Sprintf("%dµs", c.Duration.Microseconds())
	}
	if c.Duration < time.Second {
		return fmt.Sprintf("%dms", c.Duration.Milliseconds())
	}
	return c.Duration.Round(time.Millisecond).String()
}

// IncompleteReasons liste les raisons pour lesquelles une capture n'est pas
// totalement exploitable. Sert aux avertissements, jamais à masquer un fait.
func (c Capture) IncompleteReasons() []string {
	var reasons []string
	if !c.Request.Received {
		reasons = append(reasons, "requête non reçue")
	} else if c.Request.BodyTruncated {
		reasons = append(reasons, fmt.Sprintf(
			"corps de requête tronqué (%d/%d octets conservés)",
			len(c.Request.Body), c.Request.BodySize))
	}
	if c.Response == nil {
		reasons = append(reasons, "réponse en attente")
	} else if c.Response.BodyTruncated {
		reasons = append(reasons, fmt.Sprintf(
			"corps de réponse tronqué (%d/%d octets conservés)",
			len(c.Response.Body), c.Response.BodySize))
	}
	if c.Err != "" {
		reasons = append(reasons, "erreur de transport : "+c.Err)
	}
	return reasons
}

// StatusLabelText rend un statut lisible avec sa raison.
func (c Capture) StatusLabelText() string {
	if c.Err != "" {
		return "ERREUR"
	}
	if c.Response == nil {
		return "PENDING"
	}
	if c.Response.Reason != "" {
		return fmt.Sprintf("%d %s", c.Response.StatusCode, c.Response.Reason)
	}
	return strconv.Itoa(c.Response.StatusCode)
}

func cloneHeaders(headers []Header) []Header {
	if headers == nil {
		return nil
	}
	out := make([]Header, len(headers))
	copy(out, headers)
	return out
}

// snapshot retourne une copie sûre pour la lecture hors verrou. Les corps ne
// sont jamais modifiés après leur création : partager leur tranche est donc
// sans danger et évite des copies de plusieurs mégaoctets.
func (c *Capture) snapshot() Capture {
	if c == nil {
		return Capture{}
	}
	out := *c
	out.Request.Headers = cloneHeaders(c.Request.Headers)
	if c.Response != nil {
		resp := *c.Response
		resp.Headers = cloneHeaders(c.Response.Headers)
		out.Response = &resp
	}
	return out
}

// BridgeEvent est l'événement normalisé, après décodage et validation, qui
// circule entre le pont HTTP et l'historique.
type BridgeEvent struct {
	Type          string
	Version       int
	FlowID        string
	Timestamp     time.Time
	Method        string
	URL           string
	HTTPVersion   string
	Headers       []Header
	Body          []byte
	BodySize      int64
	BodyTruncated bool

	StatusCode int
	Reason     string
	Duration   time.Duration

	Error   string
	Message string
}

// Notification est un événement destiné à l'affichage asynchrone du shell.
type Notification struct {
	Kind     string // request, response, error, proxy
	Level    string // info, warning, error
	ID       int
	Method   string
	URL      string
	Status   int
	Duration time.Duration
	Message  string
}

// ListenAddress retourne l'adresse d'écoute formatée.
func ListenAddress(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

// NormalizeMethod met une méthode HTTP en majuscules et valide sa syntaxe.
func NormalizeMethod(method string) (string, error) {
	m := strings.ToUpper(strings.TrimSpace(method))
	if m == "" {
		return "", fmt.Errorf("méthode HTTP vide")
	}
	for i, r := range m {
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if i > 0 && (r == '-' || r == '_' || (r >= '0' && r <= '9')) {
			continue
		}
		return "", fmt.Errorf("méthode HTTP invalide %q", method)
	}
	return m, nil
}
