package mint

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	bridgeMaxHeaderCount = 256
	bridgeMaxHeaderBytes = 1 << 20
	bridgeMaxURLLength   = 16 << 10
	bridgeMaxErrorLength = 8 << 10
	bridgeQueueSize      = 2048
	bridgeShutdownGrace  = 2 * time.Second
)

// Bridge reçoit les événements de l'addon Python via un petit serveur HTTP
// lié exclusivement à 127.0.0.1. Chaque événement doit présenter un jeton de
// session aléatoire. Le jeton n'est jamais journalisé.
type Bridge struct {
	token    string
	listener net.Listener
	server   *http.Server
	events   chan BridgeEvent
	done     chan struct{}

	closeOnce sync.Once
	wg        sync.WaitGroup

	maxBody  int64
	maxEvent int64
}

// NewBridge démarre un pont sur un port loopback libre.
func NewBridge(maxBody int64) (*Bridge, error) {
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("génération du jeton de session : %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("écoute du pont local : %w", err)
	}

	b := &Bridge{
		token:    hex.EncodeToString(tokenBytes),
		listener: listener,
		events:   make(chan BridgeEvent, bridgeQueueSize),
		done:     make(chan struct{}),
		maxBody:  maxBody,
		// Un corps est encodé en base64, auquel s'ajoutent les en-têtes et
		// les métadonnées. Cette marge évite de rejeter un événement valide.
		maxEvent: maxBody*2 + (2 << 20),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/events", b.handleEvent)
	b.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		_ = b.server.Serve(listener)
	}()
	return b, nil
}

// Addr retourne l'adresse loopback du pont.
func (b *Bridge) Addr() string { return b.listener.Addr().String() }

// URL retourne l'URL de base du pont.
func (b *Bridge) URL() string { return "http://" + b.Addr() }

// Host et Port séparent l'adresse pour l'addon.
func (b *Bridge) Host() string {
	host, _, _ := net.SplitHostPort(b.Addr())
	return host
}

func (b *Bridge) Port() int {
	_, portStr, _ := net.SplitHostPort(b.Addr())
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	return port
}

// Events retourne le canal des événements validés. Il est fermé par Close.
func (b *Bridge) Events() <-chan BridgeEvent { return b.events }

// Close arrête le serveur et ferme le canal d'événements. Idempotent.
func (b *Bridge) Close() error {
	var closeErr error
	b.closeOnce.Do(func() {
		close(b.done)
		ctx, cancel := context.WithTimeout(context.Background(), bridgeShutdownGrace)
		defer cancel()
		closeErr = b.server.Shutdown(ctx)
		_ = b.listener.Close()
		b.wg.Wait()
		close(b.events)
	})
	return closeErr
}

func (b *Bridge) handleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "méthode non autorisée", http.StatusMethodNotAllowed)
		return
	}
	if !b.authorized(r.Header.Get("Authorization")) {
		http.Error(w, "jeton de session invalide", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, b.maxEvent)
	decoder := json.NewDecoder(r.Body)
	var wire wireEvent
	if err := decoder.Decode(&wire); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "événement trop volumineux", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "JSON invalide", http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		http.Error(w, "JSON invalide", http.StatusBadRequest)
		return
	}

	event, err := b.normalize(wire)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	select {
	case b.events <- event:
		w.WriteHeader(http.StatusNoContent)
	case <-b.done:
		http.Error(w, "pont en cours de fermeture", http.StatusServiceUnavailable)
	}
}

func (b *Bridge) authorized(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(provided), []byte(b.token)) == 1
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra interface{}
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("données JSON supplémentaires")
	}
	return err
}

// wireEvent est le format JSON accepté depuis l'addon. Aucun champ inconnu
// n'est interprété, ce qui permet de faire évoluer l'addon sans casser Go.
type wireEvent struct {
	Type          string   `json:"type"`
	Version       int      `json:"version,omitempty"`
	FlowID        string   `json:"flow_id,omitempty"`
	Timestamp     string   `json:"timestamp,omitempty"`
	Method        string   `json:"method,omitempty"`
	URL           string   `json:"url,omitempty"`
	HTTPVersion   string   `json:"http_version,omitempty"`
	Headers       []Header `json:"headers,omitempty"`
	BodyBase64    string   `json:"body_b64,omitempty"`
	BodySize      int64    `json:"body_size,omitempty"`
	BodyTruncated bool     `json:"body_truncated,omitempty"`
	StatusCode    int      `json:"status_code,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	DurationMS    float64  `json:"duration_ms,omitempty"`
	Error         string   `json:"error,omitempty"`
	Message       string   `json:"message,omitempty"`
}

func (b *Bridge) normalize(w wireEvent) (BridgeEvent, error) {
	ev := BridgeEvent{
		Type:          strings.ToLower(strings.TrimSpace(w.Type)),
		Version:       w.Version,
		FlowID:        strings.TrimSpace(w.FlowID),
		Headers:       w.Headers,
		BodySize:      w.BodySize,
		BodyTruncated: w.BodyTruncated,
		StatusCode:    w.StatusCode,
		Reason:        cleanText(w.Reason, 512),
		Error:         cleanText(w.Error, bridgeMaxErrorLength),
		Message:       cleanText(w.Message, 512),
	}

	if w.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, w.Timestamp)
		if err != nil {
			return BridgeEvent{}, fmt.Errorf("horodatage invalide")
		}
		ev.Timestamp = t
	}

	if len(ev.Headers) > bridgeMaxHeaderCount {
		return BridgeEvent{}, fmt.Errorf("trop d'en-têtes (%d, maximum %d)", len(ev.Headers), bridgeMaxHeaderCount)
	}
	var headerBytes int
	for i := range ev.Headers {
		name := strings.TrimSpace(ev.Headers[i].Name)
		value := ev.Headers[i].Value
		if !validHeaderName(name) {
			return BridgeEvent{}, fmt.Errorf("nom d'en-tête invalide")
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return BridgeEvent{}, fmt.Errorf("valeur d'en-tête invalide")
		}
		if len(value) > 64<<10 {
			return BridgeEvent{}, fmt.Errorf("valeur d'en-tête trop longue")
		}
		headerBytes += len(name) + len(value)
		ev.Headers[i].Name = name
	}
	if headerBytes > bridgeMaxHeaderBytes {
		return BridgeEvent{}, fmt.Errorf("en-têtes trop volumineux")
	}

	if w.BodyBase64 != "" {
		body, err := base64.StdEncoding.DecodeString(w.BodyBase64)
		if err != nil {
			return BridgeEvent{}, fmt.Errorf("corps base64 invalide")
		}
		if int64(len(body)) > b.maxBody {
			return BridgeEvent{}, fmt.Errorf("corps trop volumineux (%d octets, maximum %d)", len(body), b.maxBody)
		}
		ev.Body = body
	}
	if ev.BodySize < int64(len(ev.Body)) {
		ev.BodySize = int64(len(ev.Body))
	}
	if ev.BodySize > int64(len(ev.Body)) && !ev.BodyTruncated {
		return BridgeEvent{}, fmt.Errorf("corps annoncé incomplet sans marqueur de troncature")
	}
	if ev.BodyTruncated && int64(len(ev.Body)) > b.maxBody {
		return BridgeEvent{}, fmt.Errorf("corps tronqué trop volumineux")
	}

	if w.DurationMS < 0 {
		return BridgeEvent{}, fmt.Errorf("durée négative")
	}
	if w.DurationMS > float64(7*24*time.Hour/time.Millisecond) {
		return BridgeEvent{}, fmt.Errorf("durée aberrante")
	}
	ev.Duration = time.Duration(w.DurationMS * float64(time.Millisecond))

	switch ev.Type {
	case "ready":
		if ev.Version <= 0 {
			ev.Version = 1
		}
	case "request":
		if ev.FlowID == "" {
			return BridgeEvent{}, fmt.Errorf("flow_id manquant")
		}
		if len(ev.FlowID) > 256 {
			return BridgeEvent{}, fmt.Errorf("flow_id trop long")
		}
		if len(w.URL) > bridgeMaxURLLength {
			return BridgeEvent{}, fmt.Errorf("URL trop longue")
		}
		if err := ValidateURL(w.URL); err != nil {
			return BridgeEvent{}, err
		}
		method, err := NormalizeMethod(w.Method)
		if err != nil {
			return BridgeEvent{}, err
		}
		ev.Method = method
		ev.URL = w.URL
		ev.HTTPVersion = cleanText(w.HTTPVersion, 32)
	case "response":
		if ev.FlowID == "" {
			return BridgeEvent{}, fmt.Errorf("flow_id manquant")
		}
		if ev.StatusCode < 100 || ev.StatusCode > 599 {
			return BridgeEvent{}, fmt.Errorf("statut HTTP invalide %d", ev.StatusCode)
		}
		ev.HTTPVersion = cleanText(w.HTTPVersion, 32)
	case "error":
		if ev.FlowID == "" {
			return BridgeEvent{}, fmt.Errorf("flow_id manquant")
		}
		if ev.Error == "" {
			ev.Error = "erreur de transport inconnue"
		}
	default:
		return BridgeEvent{}, fmt.Errorf("type d'événement inconnu %q", ev.Type)
	}

	return ev, nil
}

func validHeaderName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

func cleanText(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}
