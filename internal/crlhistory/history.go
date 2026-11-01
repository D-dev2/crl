// Package crlhistory implémente un historique indépendant de Mint pour les
// exécutions de requêtes CRL-CLI. Chaque exécution reçoit un identifiant
// stable, distinct des identifiants de captures Mint, et conserve la requête
// et la réponse complètes dans un répertoire utilisateur persistant.
package crlhistory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"crl-cli/internal/request"
)

// Header est un en-tête HTTP ordonné conservé dans l'historique.
type Header struct {
	Name  string
	Value string
}

// Message est une requête ou une réponse HTTP complète.
type Message struct {
	Method  string
	URL     string
	Proto   string
	Headers []Header
	Body    []byte
}

// Execution décrit une exécution CRL enregistrée.
type Execution struct {
	ID         int
	StartedAt  time.Time
	Request    Message
	Response   *Message
	Duration   time.Duration
	StatusCode int
	Err        string
}

// storeEntry est la forme sérialisée d'une exécution sur disque.
type storeEntry struct {
	ID         int       `json:"id"`
	StartedAt  string    `json:"started_at"`
	Method     string    `json:"method"`
	URL        string    `json:"url"`
	Proto      string    `json:"proto,omitempty"`
	StatusCode int       `json:"status_code,omitempty"`
	DurationNs int64     `json:"duration_ns,omitempty"`
	Err        string    `json:"err,omitempty"`
	Request    storeMsg  `json:"request"`
	Response   *storeMsg `json:"response,omitempty"`
}

type storeMsg struct {
	Method  string   `json:"method"`
	URL     string   `json:"url"`
	Proto   string   `json:"proto,omitempty"`
	Headers []Header `json:"headers"`
	Body    string   `json:"body"`
}

// indexEntry est une ligne légère de l'index d'exécutions.
type indexEntry struct {
	ID         int    `json:"id"`
	StartedAt  string `json:"started_at"`
	Method     string `json:"method"`
	URL        string `json:"url"`
	StatusCode int    `json:"status_code,omitempty"`
	Err        string `json:"err,omitempty"`
}

// Store est l'historique persistant des exécutions CRL.
type Store struct {
	mu     sync.Mutex
	dir    string
	nextID int
	list   []*Execution
	index  *indexFile
}

// indexFile conserve la liste légère des exécutions pour rechargement rapide.
type indexFile struct {
	mu      sync.Mutex
	path    string
	entries []indexEntry
	nextID  int
}

// defaultDir retourne le répertoire utilisateur par défaut (~/.crl-cli),
// surchargeable par la variable d'environnement CRCLI_DATA_DIR.
func defaultDir() string {
	if v := os.Getenv("CRCLI_DATA_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "crl-cli")
	}
	return filepath.Join(home, ".crl-cli")
}

// Open ouvre (ou crée) le magasin d'historique dans dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		dir = defaultDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("création du répertoire d'historique : %w", err)
	}
	idx, err := openIndex(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, nextID: idx.nextID, index: idx}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir retourne le répertoire utilisé par le magasin.
func (s *Store) Dir() string { return s.dir }

func (s *Store) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	ids := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		ex, err := readExecution(s.dir, id)
		if err != nil {
			continue // une exécution corrompue ne bloque pas l'historique
		}
		s.list = append(s.list, ex)
		if id >= s.nextID {
			s.nextID = id + 1
		}
	}
	return nil
}

// Close est un no-op conservé pour la cohérence de l'API.
func (s *Store) Close() error { return nil }

// Add enregistre une exécution et lui affecte un identifiant stable. Elle est
// persistée sur disque avant le retour.
func (s *Store) Add(ex Execution) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	ex.ID = id
	if ex.StartedAt.IsZero() {
		ex.StartedAt = time.Now()
	}
	dir := filepath.Join(s.dir, strconv.Itoa(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	if err := writeExecution(dir, ex); err != nil {
		return 0, err
	}
	s.list = append(s.list, &ex)
	if err := s.index.append(indexEntry{
		ID:         id,
		StartedAt:  ex.StartedAt.Format(time.RFC3339Nano),
		Method:     ex.Request.Method,
		URL:        ex.Request.URL,
		StatusCode: ex.StatusCode,
		Err:        ex.Err,
	}); err != nil {
		return id, err
	}
	return id, nil
}

// List retourne toutes les exécutions, du plus récent au plus ancien.
func (s *Store) List() []*Execution {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Execution, len(s.list))
	copy(out, s.list)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// Get retourne l'exécution d'identifiant id.
func (s *Store) Get(id int) (*Execution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ex := range s.list {
		if ex.ID == id {
			return ex, true
		}
	}
	return nil, false
}

// Count retourne le nombre d'exécutions enregistrées.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}

// Export archive une exécution dans un fichier JSON autonome et retourne son
// chemin. Le fichier est lisible sans le magasin d'origine.
func (s *Store) Export(id int) (string, error) {
	s.mu.Lock()
	ex, ok := s.getLocked(id)
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("exécution #%d introuvable", id)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, fmt.Sprintf("export-%d.json", id))
	data, err := json.MarshalIndent(ex, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) getLocked(id int) (*Execution, bool) {
	for _, ex := range s.list {
		if ex.ID == id {
			return ex, true
		}
	}
	return nil, false
}

// FromBuilderRequest construit un Message à partir d'un constructeur.
func FromBuilderRequest(b *request.Builder) Message {
	return Message{
		Method:  b.Method(),
		URL:     b.URL(),
		Headers: headerSlice(b.Headers()),
		Body:    b.Body(),
	}
}

func headerSlice(hs []request.Header) []Header {
	out := make([]Header, 0, len(hs))
	for _, h := range hs {
		out = append(out, Header{Name: h.Name, Value: h.Value})
	}
	return out
}

// ---- lecture/écriture disque ----

func readExecution(dir string, id int) (*Execution, error) {
	ed := filepath.Join(dir, strconv.Itoa(id))
	meta, err := os.ReadFile(filepath.Join(ed, "meta.json"))
	if err != nil {
		return nil, err
	}
	var ent storeEntry
	if err := json.Unmarshal(meta, &ent); err != nil {
		return nil, err
	}
	started, _ := time.Parse(time.RFC3339Nano, ent.StartedAt)
	ex := &Execution{
		ID:         ent.ID,
		StartedAt:  started,
		Duration:   time.Duration(ent.DurationNs),
		StatusCode: ent.StatusCode,
		Err:        ent.Err,
		Request: Message{
			Method:  ent.Request.Method,
			URL:     ent.Request.URL,
			Proto:   ent.Request.Proto,
			Headers: ent.Request.Headers,
			Body:    []byte(ent.Request.Body),
		},
	}
	if ent.Response != nil {
		ex.Response = &Message{
			Method:  ent.Response.Method,
			URL:     ent.Response.URL,
			Proto:   ent.Response.Proto,
			Headers: ent.Response.Headers,
			Body:    []byte(ent.Response.Body),
		}
	}
	// Les fichiers .body, s'ils existent, remplacent le corps inline stocké
	// dans meta.json (corps volumineux conservés à part).
	if data, err := os.ReadFile(filepath.Join(ed, "request.body")); err == nil {
		ex.Request.Body = data
	}
	if ex.Response != nil {
		if data, err := os.ReadFile(filepath.Join(ed, "response.body")); err == nil {
			ex.Response.Body = data
		}
	}
	return ex, nil
}

func writeExecution(dir string, ex Execution) error {
	ent := storeEntry{
		ID:         ex.ID,
		StartedAt:  ex.StartedAt.Format(time.RFC3339Nano),
		Method:     ex.Request.Method,
		URL:        ex.Request.URL,
		Proto:      ex.Request.Proto,
		StatusCode: ex.StatusCode,
		DurationNs: ex.Duration.Nanoseconds(),
		Err:        ex.Err,
		Request: storeMsg{
			Method:  ex.Request.Method,
			URL:     ex.Request.URL,
			Proto:   ex.Request.Proto,
			Headers: ex.Request.Headers,
			Body:    string(ex.Request.Body),
		},
	}
	if ex.Response != nil {
		ent.Response = &storeMsg{
			Method:  ex.Response.Method,
			URL:     ex.Response.URL,
			Proto:   ex.Response.Proto,
			Headers: ex.Response.Headers,
			Body:    string(ex.Response.Body),
		}
	}
	data, err := json.MarshalIndent(ent, "", "  ")
	if err != nil {
		return err
	}
	metaPath := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		return err
	}
	if len(ex.Request.Body) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "request.body"), ex.Request.Body, 0o600); err != nil {
			return err
		}
	}
	if ex.Response != nil && len(ex.Response.Body) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "response.body"), ex.Response.Body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ---- index ----

func openIndex(path string) (*indexFile, error) {
	idx := &indexFile{path: path, nextID: 1}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return idx, nil
	}
	var raw struct {
		Entries []indexEntry `json:"entries"`
		NextID  int          `json:"next_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	idx.entries = raw.Entries
	if raw.NextID > idx.nextID {
		idx.nextID = raw.NextID
	}
	for _, e := range idx.entries {
		if e.ID >= idx.nextID {
			idx.nextID = e.ID + 1
		}
	}
	return idx, nil
}

func (i *indexFile) append(e indexEntry) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.entries = append(i.entries, e)
	if e.ID >= i.nextID {
		i.nextID = e.ID + 1
	}
	data, err := json.MarshalIndent(struct {
		Entries []indexEntry `json:"entries"`
		NextID  int          `json:"next_id"`
	}{i.entries, i.nextID}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(i.path, data, 0o600)
}
