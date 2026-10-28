package mint

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// HistoryConfig borne la mémoire utilisée par l'historique des captures.
type HistoryConfig struct {
	// MaxCaptures est le nombre maximum d'échanges conservés.
	MaxCaptures int
	// MaxBodyBytes est la taille maximale d'un corps de requête ou de
	// réponse. Elle est aussi appliquée par le pont et par l'addon.
	MaxBodyBytes int64
	// MaxTotalBodyBytes borne la somme des corps conservés en mémoire.
	MaxTotalBodyBytes int64
}

const (
	DefaultMaxCaptures       = 2000
	DefaultMaxBodyBytes      = 2 << 20  // 2 Mio
	DefaultMaxTotalBodyBytes = 64 << 20 // 64 Mio
)

// DefaultHistoryConfig retourne une configuration mémoire raisonnable.
func DefaultHistoryConfig() HistoryConfig {
	return HistoryConfig{
		MaxCaptures:       DefaultMaxCaptures,
		MaxBodyBytes:      DefaultMaxBodyBytes,
		MaxTotalBodyBytes: DefaultMaxTotalBodyBytes,
	}
}

func (c HistoryConfig) normalized() HistoryConfig {
	if c.MaxCaptures <= 0 {
		c.MaxCaptures = DefaultMaxCaptures
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.MaxTotalBodyBytes <= 0 {
		c.MaxTotalBodyBytes = DefaultMaxTotalBodyBytes
	}
	return c
}

// History conserve les captures en mémoire. Toutes les méthodes sont sûres
// vis-à-vis des accès concurrents (shell, pont HTTP, notifications).
type History struct {
	mu sync.Mutex

	cfg         HistoryConfig
	byID        map[int]*Capture
	byFlow      map[string]*Capture
	order       []int
	nextID      int
	total       int64
	filter      string
	selected    int
	selectedIDs []int
}

// NewHistory crée un historique vide.
func NewHistory(cfg HistoryConfig) *History {
	cfg = cfg.normalized()
	return &History{
		cfg:    cfg,
		byID:   make(map[int]*Capture),
		byFlow: make(map[string]*Capture),
		nextID: 1,
	}
}

// Config retourne la configuration effective de l'historique.
func (h *History) Config() HistoryConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// AddRequest enregistre ou met à jour la requête d'un flux.
// Le booléen indique s'il s'agit d'une nouvelle capture.
func (h *History) AddRequest(ev BridgeEvent) (Capture, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	scheme, host, path, rawQuery := splitURL(ev.URL)
	bodySize := ev.BodySize
	if bodySize <= 0 {
		bodySize = int64(len(ev.Body))
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}

	c, exists := h.byFlow[ev.FlowID]
	if !exists || ev.FlowID == "" {
		c = &Capture{
			ID:        h.nextID,
			FlowID:    ev.FlowID,
			StartedAt: ev.Timestamp,
		}
		h.nextID++
		h.byID[c.ID] = c
		h.order = append(h.order, c.ID)
		if ev.FlowID != "" {
			h.byFlow[ev.FlowID] = c
		}
	} else {
		// Mise à jour d'un flux déjà capturé : on remplace les corps et on
		// réajuste le compteur mémoire sans changer l'ID.
		if !c.Request.Received && !ev.Timestamp.IsZero() && ev.Timestamp.Before(c.StartedAt) {
			c.StartedAt = ev.Timestamp
		}
		h.total -= int64(len(c.Request.Body))
		if c.Response != nil {
			h.total -= int64(len(c.Response.Body))
		}
	}

	c.Request = RequestData{
		Method:        ev.Method,
		URL:           ev.URL,
		Scheme:        scheme,
		Host:          host,
		Path:          path,
		RawQuery:      rawQuery,
		HTTPVersion:   ev.HTTPVersion,
		Headers:       cloneHeaders(ev.Headers),
		Body:          ev.Body,
		BodySize:      bodySize,
		BodyTruncated: ev.BodyTruncated,
		Received:      true,
	}
	h.total += int64(len(c.Request.Body))
	if c.Response != nil {
		h.total += int64(len(c.Response.Body))
	}

	h.enforceLimitsLocked()
	return c.snapshot(), !exists
}

// AddResponse associe une réponse à un flux. Si la requête n'est pas encore
// arrivée, un enregistrement incomplet est créé puis complété plus tard.
func (h *History) AddResponse(ev BridgeEvent) Capture {
	h.mu.Lock()
	defer h.mu.Unlock()

	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	c := h.getOrCreateFlowLocked(ev.FlowID, ev.Timestamp)

	if c.Response != nil {
		h.total -= int64(len(c.Response.Body))
	}
	bodySize := ev.BodySize
	if bodySize <= 0 {
		bodySize = int64(len(ev.Body))
	}
	c.Response = &ResponseData{
		StatusCode:    ev.StatusCode,
		Reason:        ev.Reason,
		HTTPVersion:   ev.HTTPVersion,
		Headers:       cloneHeaders(ev.Headers),
		Body:          ev.Body,
		BodySize:      bodySize,
		BodyTruncated: ev.BodyTruncated,
	}
	h.total += int64(len(ev.Body))
	c.Err = ""

	switch {
	case ev.Duration > 0:
		c.Duration = ev.Duration
	case !c.StartedAt.IsZero() && ev.Timestamp.After(c.StartedAt):
		c.Duration = ev.Timestamp.Sub(c.StartedAt)
	}

	h.enforceLimitsLocked()
	return c.snapshot()
}

// AddError enregistre une erreur de transport pour un flux.
func (h *History) AddError(flowID, message string, at time.Time) Capture {
	h.mu.Lock()
	defer h.mu.Unlock()

	if at.IsZero() {
		at = time.Now()
	}
	c := h.getOrCreateFlowLocked(flowID, at)
	c.Err = message
	if c.Duration <= 0 && !c.StartedAt.IsZero() && at.After(c.StartedAt) {
		c.Duration = at.Sub(c.StartedAt)
	}
	return c.snapshot()
}

func (h *History) getOrCreateFlowLocked(flowID string, at time.Time) *Capture {
	if c, ok := h.byFlow[flowID]; ok && flowID != "" {
		return c
	}
	c := &Capture{
		ID:        h.nextID,
		FlowID:    flowID,
		StartedAt: at,
	}
	h.nextID++
	h.byID[c.ID] = c
	h.order = append(h.order, c.ID)
	if flowID != "" {
		h.byFlow[flowID] = c
	}
	return c
}

func (h *History) enforceLimitsLocked() {
	for len(h.order) > h.cfg.MaxCaptures || (h.total > h.cfg.MaxTotalBodyBytes && len(h.order) > 1) {
		oldest := h.order[0]
		h.order = h.order[1:]
		c, ok := h.byID[oldest]
		if !ok {
			continue
		}
		delete(h.byID, oldest)
		if c.FlowID != "" {
			delete(h.byFlow, c.FlowID)
		}
		h.total -= int64(len(c.Request.Body))
		if c.Response != nil {
			h.total -= int64(len(c.Response.Body))
		}
		if h.selected == oldest {
			h.selected = 0
		}
		for i, id := range h.selectedIDs {
			if id == oldest {
				h.selectedIDs = append(h.selectedIDs[:i], h.selectedIDs[i+1:]...)
				break
			}
		}
		if h.total < 0 {
			h.total = 0
		}
	}
}

// Get retourne un instantané de la capture demandée.
func (h *History) Get(id int) (Capture, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.byID[id]
	if !ok {
		return Capture{}, false
	}
	return c.snapshot(), true
}

// List retourne toutes les captures, dans l'ordre de capture.
func (h *History) List() []Capture {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listLocked(false)
}

// Filtered retourne les captures correspondant au filtre courant.
func (h *History) Filtered() []Capture {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listLocked(true)
}

func (h *History) listLocked(applyFilter bool) []Capture {
	filter := strings.ToLower(strings.TrimSpace(h.filter))
	out := make([]Capture, 0, len(h.order))
	for _, id := range h.order {
		c, ok := h.byID[id]
		if !ok {
			continue
		}
		if applyFilter && filter != "" {
			haystack := strings.ToLower(c.Request.Host + " " + c.Request.URL + " " + c.Request.Path + " " + c.Request.RawQuery)
			if !strings.Contains(haystack, filter) {
				continue
			}
		}
		out = append(out, c.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SetFilter définit le filtre local. Une chaîne vide le supprime.
func (h *History) SetFilter(filter string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.filter = strings.TrimSpace(filter)
}

// Filter retourne le filtre courant.
func (h *History) Filter() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.filter
}

// Select définit la capture active. La sélection est indépendante du filtre.
func (h *History) Select(id int) (Capture, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.byID[id]
	if !ok {
		return Capture{}, false
	}
	h.selected = id
	h.selectedIDs = []int{id}
	return c.snapshot(), true
}

// SelectMany validates and stores a persistent ordered selection.
func (h *History) SelectMany(ids []int) ([]Capture, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := make(map[int]bool, len(ids))
	out := make([]Capture, 0, len(ids))
	for _, id := range ids {
		c, ok := h.byID[id]
		if !ok {
			return nil, fmt.Errorf("capture #%d introuvable", id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, c.snapshot())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("sélection vide")
	}
	h.selectedIDs = h.selectedIDs[:0]
	for _, c := range out {
		h.selectedIDs = append(h.selectedIDs, c.ID)
	}
	h.selected = out[0].ID
	return out, nil
}

// SelectedMany returns snapshots of all still available selected captures.
func (h *History) SelectedMany() []Capture {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Capture, 0, len(h.selectedIDs))
	for _, id := range h.selectedIDs {
		if c := h.byID[id]; c != nil {
			out = append(out, c.snapshot())
		}
	}
	return out
}

// ClearSelection removes the persistent selection.
func (h *History) ClearSelection() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.selected = 0
	h.selectedIDs = nil
}

// Selected retourne la capture active.
func (h *History) Selected() (Capture, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.byID[h.selected]
	if !ok {
		return Capture{}, false
	}
	return c.snapshot(), true
}

// Clear supprime toutes les captures. Les identifiants ne sont pas réutilisés.
// Le filtre et l'éventuelle sélection sont réinitialisés.
func (h *History) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byID = make(map[int]*Capture)
	h.byFlow = make(map[string]*Capture)
	h.order = nil
	h.total = 0
	h.selected = 0
	h.selectedIDs = nil
}

// Counts retourne le nombre de requêtes enregistrées et de réponses reçues.
func (h *History) Counts() (requests, responses int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.byID {
		if c.Request.Received {
			requests++
		}
		if c.Response != nil {
			responses++
		}
	}
	return requests, responses
}

// TotalBodyBytes retourne la mémoire actuellement occupée par les corps.
func (h *History) TotalBodyBytes() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

// splitURL extrait les composants d'une URL HTTP sans la reconstruire.
func splitURL(raw string) (scheme, host, path, rawQuery string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", ""
	}
	scheme = strings.ToLower(u.Scheme)
	host = u.Host
	path = u.EscapedPath()
	if path == "" {
		path = "/"
	}
	rawQuery = u.RawQuery
	return scheme, host, path, rawQuery
}

// ValidateURL vérifie qu'une URL de capture est exploitable.
func ValidateURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("URL vide")
	}
	if len(raw) > 16<<10 {
		return fmt.Errorf("URL trop longue (%d octets)", len(raw))
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("URL contenant un caractère de contrôle")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL invalide : %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("schéma non pris en charge %q (attendu http ou https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL sans hôte")
	}
	return nil
}
