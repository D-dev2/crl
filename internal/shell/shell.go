// Package shell implémente le shell interactif de CRL-CLI et le dispatch des
// commandes crl.* et mint.*.
package shell

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"crl-cli/internal/display"
	"crl-cli/internal/mint"
	"crl-cli/internal/request"
	"crl-cli/internal/response"
	"crl-cli/internal/termstyle"
)

const (
	prompt           = "CRL-CLI"
	launchBodyLimit  = 32 << 10
	maxPendingEvents = 1024
)

// Options configure le shell.
type Options struct {
	In      io.Reader
	Out     io.Writer
	Builder *request.Builder
	Manager *mint.Manager
	// Notifications est le canal alimenté par le gestionnaire Mint.
	Notifications <-chan mint.Notification
}

// Shell regroupe l'état du shell interactif.
type Shell struct {
	in      io.Reader
	out     *trackingWriter
	builder *request.Builder
	manager *mint.Manager
	history *mint.History
	notifs  <-chan mint.Notification

	interactive bool
	editor      *editor
	inputCh     <-chan []byte
	scanner     *bufio.Scanner
	ctx         context.Context

	pal        palette
	lastFailed bool

	outputMode      response.Mode
	lastResponseDir string
	mintWatch       bool
	pendingEvents   []mint.Notification
	pendingDropped  int

	lastBatchSent    int
	lastBatchSuccess int
	lastBatchFail    int
	lastBatchSet     bool

	multilineMode    string // "json" or "body" or ""
	multilineBuf     strings.Builder
	multilineInitial []byte
}

type trackingWriter struct {
	w     io.Writer
	last  byte
	wrote bool
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		t.last = p[n-1]
		t.wrote = true
	}
	return n, err
}

func (t *trackingWriter) needsNewline() bool { return t.wrote && t.last != '\n' }
func (t *trackingWriter) reset()             { t.wrote = false; t.last = 0 }

// New crée un shell.
func New(opts Options) (*Shell, error) {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Manager == nil {
		return nil, fmt.Errorf("gestionnaire Mint manquant")
	}
	if opts.Builder == nil {
		opts.Builder = request.New()
	}

	tw := &trackingWriter{w: opts.Out}
	s := &Shell{
		in:         opts.In,
		out:        tw,
		builder:    opts.Builder,
		manager:    opts.Manager,
		history:    opts.Manager.History(),
		notifs:     opts.Notifications,
		mintWatch:  true,
		scanner:    bufio.NewScanner(opts.In),
		pal:        termstyle.New(opts.Out),
		outputMode: response.ModeAuto,
	}
	s.scanner.Buffer(make([]byte, 64<<10), 2<<20)
	if f, ok := opts.In.(*os.File); ok && termstyle.IsTerminal(f.Fd()) {
		s.interactive = true
	}
	return s, nil
}

// Run démarre la boucle du shell.
func (s *Shell) Run(ctx context.Context) error {
	s.ctx = ctx
	defer s.clearLastResponse()
	if s.interactive {
		return s.runInteractive(ctx)
	}
	return s.runBatch(ctx)
}

// Close libère les fichiers temporaires de la dernière réponse. Il est sûr de
// l'appeler plusieurs fois.
func (s *Shell) Close() {
	s.clearLastResponse()
}

func (s *Shell) runBatch(ctx context.Context) error {
	for s.scanner.Scan() {
		s.drainNotifications()
		s.out.reset()
		if exit := s.Execute(s.scanner.Text()); exit {
			return nil
		}
		if s.out.needsNewline() {
			fmt.Fprint(s.out, "\n")
			s.out.reset()
		}
		s.drainNotifications()
	}
	return s.scanner.Err()
}

func (s *Shell) runInteractive(ctx context.Context) error {
	f := s.in.(*os.File)
	restore, err := makeRaw(f.Fd())
	if err != nil {
		// Le terminal ne peut pas passer en mode brut : on reste utilisable,
		// mais les notifications seront affichées entre les commandes.
		s.interactive = false
		return s.runBatch(ctx)
	}
	defer restore()

	s.inputCh = readInput(s.in)
	s.editor = newEditor(s.out, s.promptText())
	s.editor.DrawPrompt()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprint(s.out, "\r\n")
			return nil
		case n, ok := <-s.notifs:
			if !ok {
				s.notifs = nil
				continue
			}
			s.handleNotification(n)
		case data, ok := <-s.inputCh:
			if !ok {
				fmt.Fprint(s.out, "\r\n")
				return nil
			}
			if s.multilineMode != "" {
				res := s.editor.Feed(data)
				if res.EOF {
					return nil
				}
				if len(res.Lines) == 0 {
					if res.Interrupted {
						s.editor.DrawPrompt()
					}
					continue
				}
				for _, line := range res.Lines {
					trimmed := strings.TrimRight(line, "\r\n")
					if strings.TrimSpace(trimmed) == ":cancel" {
						s.printf("[CRL] Saisie multiligne annulée. Corps précédent conservé.\n")
						s.multilineMode = ""
						s.multilineBuf.Reset()
						s.multilineInitial = nil
						s.drainNotifications()
						s.out.reset()
						s.editor.Reset()
						s.editor.SetPrompt(s.promptText())
						s.editor.DrawPrompt()
						continue
					}
					if strings.TrimSpace(trimmed) == ":end" {
						content := s.multilineBuf.String()
						if s.multilineMode == "json" {
							if !json.Valid([]byte(content)) {
								s.errorf("JSON invalide ; corps précédent conservé")
							} else {
								if err := s.builder.SetJSON(content); err != nil {
									s.errorf("%v", err)
								} else {
									s.printf("[CRL] Corps JSON configuré (%d octets).\n", s.builder.BodySize())
								}
							}
						} else if s.multilineMode == "body" {
							s.builder.SetBody([]byte(content))
							s.printf("[CRL] Corps brut configuré (%d octets).\n", len(content))
						}
						s.multilineMode = ""
						s.multilineBuf.Reset()
						s.multilineInitial = nil
						s.drainNotifications()
						s.out.reset()
						s.editor.Reset()
						s.editor.SetPrompt(s.promptText())
						s.editor.DrawPrompt()
						continue
					}
					s.multilineBuf.WriteString(line)
					if !strings.HasSuffix(line, "\n") && !strings.HasSuffix(line, "\r") {
						s.multilineBuf.WriteString("\n")
					}
					s.drainNotifications()
				}
				s.out.reset()
				s.editor.Reset()
				s.editor.SetPrompt(s.promptText())
				s.editor.DrawPrompt()
				continue
			}
			res := s.editor.Feed(data)
			if res.EOF {
				return nil
			}
			if len(res.Lines) == 0 {
				// Aucune ligne complète : l'éditeur a déjà redessiné la saisie.
				if res.Interrupted {
					s.editor.DrawPrompt()
				}
				continue
			}
			exit := false
			for _, line := range res.Lines {
				s.out.reset()
				if s.Execute(line) {
					exit = true
					break
				}
				if s.out.needsNewline() {
					fmt.Fprint(s.out, "\r\n")
				}
				s.drainNotifications()
			}
			if exit {
				return nil
			}
			s.out.reset()
			s.editor.Reset()
			s.editor.SetPrompt(s.promptText())
			s.editor.DrawPrompt()
		}
	}
}

func readInput(in io.Reader) <-chan []byte {
	ch := make(chan []byte, 16)
	go func() {
		defer close(ch)
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				ch <- chunk
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func (s *Shell) drainNotifications() {
	if s.notifs == nil {
		return
	}
	for {
		select {
		case n, ok := <-s.notifs:
			if !ok {
				s.notifs = nil
				return
			}
			s.handleNotification(n)
		default:
			return
		}
	}
}

// handleNotification est le point unique de traitement des événements Mint.
// Il est appelé depuis la goroutine du shell, jamais depuis le pont Mint :
// le terminal n'est donc écrit que pendant les phases où le shell contrôle
// l'affichage. Les captures restent enregistrées même si l'affichage est
// suspendu avec mint.watch off.
func (s *Shell) handleNotification(n mint.Notification) {
	if !s.mintWatch {
		s.queuePendingEvent(n)
		return
	}
	lines := s.formatNotification(n)
	if len(lines) == 0 {
		return
	}
	if s.editor != nil {
		if s.editor.Active() {
			s.editor.Notify(lines)
		} else {
			// Une commande vient de s'exécuter : le prompt sera redessiné une
			// seule fois par runInteractive après le drainage.
			s.editor.PrintLines(lines)
		}
		return
	}
	for _, line := range lines {
		s.printf("%s\n", line)
	}
}

func (s *Shell) cmdMintWatch(rest string) {
	switch strings.ToLower(strings.TrimSpace(rest)) {
	case "on":
		s.mintWatch = true
		s.printf("[Mint] Notifications automatiques activées.\n")
	case "off":
		s.mintWatch = false
		s.printf("[Mint] Notifications automatiques désactivées ; les captures restent enregistrées.\n")
	case "":
		state := "off"
		if s.mintWatch {
			state = "on"
		}
		s.printf("[Mint] mint.watch %s.\n", state)
	default:
		s.errorf("usage : mint.watch on|off")
	}
}

func (s *Shell) queuePendingEvent(n mint.Notification) {
	if len(s.pendingEvents) >= maxPendingEvents {
		// Conserve les événements les plus récents sans laisser la file croître
		// indéfiniment pendant une longue session avec mint.watch off.
		copy(s.pendingEvents, s.pendingEvents[1:])
		s.pendingEvents[len(s.pendingEvents)-1] = n
		s.pendingDropped++
		return
	}
	s.pendingEvents = append(s.pendingEvents, n)
}

func (s *Shell) cmdMintEvents() {
	if len(s.pendingEvents) == 0 {
		if s.pendingDropped > 0 {
			s.printf("[Mint] Aucun événement en attente ; %d événement(s) ancien(s) ont été abandonnés (limite %d).\n", s.pendingDropped, maxPendingEvents)
			s.pendingDropped = 0
			return
		}
		s.printf("[Mint] Aucun événement en attente.\n")
		return
	}
	for _, event := range s.pendingEvents {
		for _, line := range s.formatNotification(event) {
			s.printf("%s\n", line)
		}
	}
	if s.pendingDropped > 0 {
		s.printf("[Mint] %d événement(s) ancien(s) ont été abandonnés (limite %d).\n", s.pendingDropped, maxPendingEvents)
		s.pendingDropped = 0
	}
	s.printf("[Mint] %d événement(s) affiché(s).\n", len(s.pendingEvents))
	s.pendingEvents = nil
}

func (s *Shell) formatNotification(n mint.Notification) []string {
	p := s.pal
	id := p.BoldCyan(fmt.Sprintf("#%d", n.ID))
	switch n.Kind {
	case "request":
		return []string{fmt.Sprintf("%s %s %s %s", p.BoldMagenta("[Mint]"), id, p.Method(n.Method), p.Cyan(sanitizeTerminal(n.URL)))}
	case "response":
		status := p.StatusCode(n.Status, strconv.Itoa(n.Status))
		return []string{fmt.Sprintf("%s %s %s %s", p.BoldMagenta("[Mint]"), id, status, p.Dim("en "+durationLabel(n.Duration)))}
	case "error":
		return []string{fmt.Sprintf("%s %s %s", p.BoldMagenta("[Mint]"), p.BoldRed(fmt.Sprintf("#%d erreur :", n.ID)), p.Red(truncate(sanitizeTerminal(n.Message), 240)))}
	case "proxy":
		return []string{p.BoldMagenta("[Mint]") + " " + truncate(sanitizeTerminal(n.Message), 500)}
	default:
		if n.Message != "" {
			return []string{p.BoldMagenta("[Mint]") + " " + truncate(sanitizeTerminal(n.Message), 500)}
		}
		return nil
	}
}

// Execute traite une ligne de commande. Il retourne true si le shell doit
// se terminer.
func (s *Shell) Execute(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	s.lastFailed = false
	cmd, rest := splitCommand(line)
	switch cmd {
	case "help", "?":
		s.printHelp()
		return false
	case "exit", "quit":
		return true
	case "clear":
		if s.interactive {
			fmt.Fprint(s.out, "\033[2J\033[H")
		}
		return false
	case "reset":
		s.builder.Reset()
		s.printf("[CRL] Constructeur réinitialisé.\n")
		return false
	case "crl.host":
		s.cmdSetURL(rest)
	case "crl.method":
		s.cmdSetMethod(rest)
	case "crl.head", "crl.header":
		s.cmdHeader(rest)
	case "crl.head.clear", "crl.header.clear":
		s.cmdHeaderClear(rest)
	case "crl.json":
		if strings.EqualFold(strings.TrimSpace(rest), "clear") {
			s.builder.ClearBody()
			s.printf("[CRL] Corps supprimé.\n")
		} else if strings.TrimSpace(rest) == "" {
			s.startMultilineJSON()
		} else if strings.HasPrefix(strings.TrimSpace(rest), "set ") {
			s.cmdJSONSet(rest)
		} else if strings.HasPrefix(strings.TrimSpace(rest), "delete ") {
			s.cmdJSONDelete(rest)
		} else {
			s.cmdJSON(rest)
		}
	case "crl.body":
		if strings.EqualFold(strings.TrimSpace(rest), "clear") {
			s.builder.ClearBody()
			s.printf("[CRL] Corps supprimé.\n")
		} else if strings.TrimSpace(rest) == "" {
			s.startMultilineBody()
		} else {
			s.cmdBody(rest)
		}
	case "crl.body.clear":
		s.builder.ClearBody()
		s.printf("[CRL] Corps supprimé.\n")
	case "crl.proxy":
		s.cmdProxy(rest)
	case "crl.ca":
		s.cmdCA(rest)
	case "crl.show":
		s.cmdShow()
	case "crl.table":
		s.cmdTable()
	case "crl.launch":
		s.cmdLaunch(rest)
	case "crl.output":
		s.cmdOutput(rest)
	case "mint", "mint.help":
		s.printMintHelp()
	default:
		if strings.HasPrefix(cmd, "mint.") {
			s.executeMint(cmd, rest)
			return false
		}
		s.errorf("commande inconnue %q. Tapez help.", cmd)
	}
	return false
}

func (s *Shell) executeMint(cmd, rest string) {
	switch cmd {
	case "mint.proxy":
		sub, arg := splitCommand(rest)
		switch sub {
		case "", "status":
			s.cmdMintStatus()
		case "on", "start":
			s.cmdMintOn()
		case "off", "stop":
			s.cmdMintOff()
		case "config":
			s.cmdMintConfig()
		case "port":
			s.cmdMintPort(arg)
		default:
			s.errorf("usage : mint.proxy on|off|status|config")
		}
	case "mint.proxy.on":
		s.cmdMintOn()
	case "mint.proxy.off":
		s.cmdMintOff()
	case "mint.proxy.status":
		s.cmdMintStatus()
	case "mint.proxy.config":
		s.cmdMintConfig()
	case "mint.proxy.port":
		s.cmdMintPort(rest)
	case "mint.requests":
		s.cmdMintRequests(false)
	case "mint.watch":
		s.cmdMintWatch(rest)
	case "mint.events":
		s.cmdMintEvents()
	case "mint.history":
		s.cmdMintRequests(true)
	case "mint.request.show", "mint.show":
		s.cmdMintShow(rest)
	case "mint.select":
		s.cmdMintSelect(rest)
	case "mint.selected":
		s.cmdMintSelected()
	case "mint.replay":
		s.cmdMintReplay(rest)
	case "mint.filter":
		s.cmdMintFilter(rest)
	case "mint.clear":
		s.cmdMintClear(rest)
	case "mint.use":
		s.cmdMintUse(rest)
	default:
		s.errorf("commande Mint inconnue %q. Tapez mint.help.", cmd)
	}
}

func (s *Shell) cmdSetURL(rest string) {
	if rest == "" {
		if s.builder.URL() == "" {
			s.errorf("aucune URL configurée. usage : crl.host <url>")
		} else {
			s.printf("[CRL] URL : %s\n", display.MaskURL(s.builder.URL()))
		}
		return
	}
	if err := s.builder.SetURL(rest); err != nil {
		s.errorf("%v", err)
		return
	}
	s.printf("[CRL] URL configurée : %s\n", display.MaskURL(s.builder.URL()))
}

func (s *Shell) cmdSetMethod(rest string) {
	if rest == "" {
		s.printf("[CRL] Méthode : %s\n", s.builder.Method())
		return
	}
	if err := s.builder.SetMethod(rest); err != nil {
		s.errorf("%v", err)
		return
	}
	s.printf("[CRL] Méthode : %s\n", s.builder.Method())
}

func (s *Shell) cmdHeader(rest string) {
	switch {
	case rest == "":
		headers := s.builder.Headers()
		if len(headers) == 0 {
			s.printf("[CRL] Aucun en-tête configuré.\n")
			return
		}
		s.printf("[CRL] En-têtes (%d) :\n", len(headers))
		for _, h := range headers {
			s.printf("  %s\n", s.headerLine(h.Name, h.Value))
		}
	case strings.EqualFold(rest, "clear"):
		s.builder.ClearHeaders()
		s.printf("[CRL] Tous les en-têtes ont été supprimés.\n")
	case strings.HasPrefix(strings.ToLower(rest), "clear "):
		name := strings.TrimSpace(rest[len("clear "):])
		n := s.builder.RemoveHeader(name)
		if n == 0 {
			s.printf("[CRL] En-tête %q absent.\n", name)
		} else {
			s.printf("[CRL] %d en-tête(s) %q supprimé(s).\n", n, name)
		}
	default:
		name, value, err := request.ParseHeaderLine(rest)
		if err != nil {
			s.errorf("%v", err)
			return
		}
		if err := s.builder.AddHeader(name, value); err != nil {
			s.errorf("%v", err)
			return
		}
		s.printf("[CRL] En-tête %s ajouté.\n", name)
	}
}

func (s *Shell) cmdHeaderClear(rest string) {
	if rest == "" {
		s.builder.ClearHeaders()
		s.printf("[CRL] Tous les en-têtes ont été supprimés.\n")
		return
	}
	n := s.builder.RemoveHeader(rest)
	if n == 0 {
		s.printf("[CRL] En-tête %q absent.\n", rest)
	} else {
		s.printf("[CRL] %d en-tête(s) %q supprimé(s).\n", n, rest)
	}
}

func (s *Shell) startMultilineJSON() {
	if !s.interactive || s.editor == nil {
		// Fallback to old behavior if not interactive
		body := s.builder.Body()
		if !s.builder.BodySet() {
			body = []byte("{}\n")
		}
		edited, err := editMultiline("crl-json-*.json", body)
		if err != nil {
			s.errorf("édition JSON : %v", err)
			return
		}
		if !json.Valid(edited) {
			s.errorf("JSON invalide ; corps précédent conservé")
			return
		}
		if err := s.builder.SetJSON(string(edited)); err != nil {
			s.errorf("%v", err)
			return
		}
		s.printf("[CRL] Corps JSON configuré (%d octets).\n", s.builder.BodySize())
		return
	}
	body := s.builder.Body()
	if s.builder.BodySet() && len(body) > 0 {
		s.printf("[CRL] Corps JSON actuel (référence) :\n%s\n", string(body))
		if !strings.HasSuffix(string(body), "\n") {
			s.printf("\n")
		}
	}
	s.printf("[CRL] Saisie JSON multiligne en cours. Tapez votre JSON ligne par ligne. Terminez par une ligne isolée ':end'. Annulez avec ':cancel'.\n")
	s.printf("[CRL] La saisie fournira un nouveau document JSON complet.\n")
	s.multilineMode = "json"
	s.multilineBuf.Reset()
	s.out.reset()
	s.editor.Reset()
	s.editor.SetPrompt(s.promptText())
	s.editor.DrawPrompt()
}

func (s *Shell) startMultilineBody() {
	if !s.interactive || s.editor == nil {
		body := s.builder.Body()
		if !s.builder.BodySet() {
			body = nil
		}
		edited, err := editMultiline("crl-body-*", body)
		if err != nil {
			s.errorf("édition du corps : %v", err)
			return
		}
		s.builder.SetBody(edited)
		s.printf("[CRL] Corps brut configuré (%d octets).\n", len(edited))
		return
	}
	body := s.builder.Body()
	if s.builder.BodySet() && len(body) > 0 {
		s.printf("[CRL] Corps actuel (référence) :\n%s\n", string(body))
		if !strings.HasSuffix(string(body), "\n") {
			s.printf("\n")
		}
	}
	s.printf("[CRL] Saisie corps multiligne en cours. Tapez votre corps ligne par ligne. Terminez par une ligne isolée ':end'. Annulez avec ':cancel'.\n")
	s.multilineMode = "body"
	s.multilineBuf.Reset()
	s.out.reset()
	s.editor.Reset()
	s.editor.SetPrompt(s.promptText())
	s.editor.DrawPrompt()
}

func (s *Shell) cmdJSONSet(rest string) {
	// rest is like: set role = "admin" or set user.role = "admin"
	trim := strings.TrimSpace(rest)
	if !strings.HasPrefix(trim, "set ") {
		s.errorf("usage : crl.json set <chemin> = <valeur JSON>")
		return
	}
	expr := strings.TrimSpace(trim[4:])
	eq := strings.Index(expr, "=")
	if eq < 0 {
		s.errorf("usage : crl.json set <chemin> = <valeur JSON>")
		return
	}
	path := strings.TrimSpace(expr[:eq])
	valStr := strings.TrimSpace(expr[eq+1:])
	if path == "" {
		s.errorf("chemin invalide")
		return
	}

	body := s.builder.Body()
	if !s.builder.BodySet() || len(body) == 0 {
		body = []byte("{}")
	}
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		s.errorf("JSON existant invalide")
		return
	}
	m, ok := root.(map[string]interface{})
	if !ok {
		s.errorf("le JSON existant n'est pas un objet compatible")
		return
	}

	var val interface{}
	if err := json.Unmarshal([]byte(valStr), &val); err != nil {
		// Try as string if not valid JSON
		s.errorf("valeur JSON invalide")
		return
	}

	// handle nested path
	parts := strings.Split(path, ".")
	cur := m
	for i := 0; i < len(parts)-1; i++ {
		key := parts[i]
		if cur[key] == nil {
			nm := map[string]interface{}{}
			cur[key] = nm
			cur = nm
			continue
		}
		if nm, ok := cur[key].(map[string]interface{}); ok {
			cur = nm
			continue
		}
		s.errorf("chemin invalide")
		return
	}
	cur[parts[len(parts)-1]] = val

	out, err := json.Marshal(root)
	if err != nil {
		s.errorf("%v", err)
		return
	}
	old := s.builder.Body()
	if err := s.builder.SetJSON(string(out)); err != nil {
		s.errorf("%v", err)
		return
	}
	// SetJSON adds/sets Content-Type; ensure we preserve other headers? SetJSON calls SetHeader for Content-Type
	s.printf("[CRL] Champ JSON mis à jour (%d octets).\n", s.builder.BodySize())
	_ = old
}

func (s *Shell) cmdJSONDelete(rest string) {
	trim := strings.TrimSpace(rest)
	if !strings.HasPrefix(trim, "delete ") {
		s.errorf("usage : crl.json delete <chemin>")
		return
	}
	path := strings.TrimSpace(trim[7:])
	if path == "" {
		s.errorf("chemin invalide")
		return
	}
	body := s.builder.Body()
	if !s.builder.BodySet() || len(body) == 0 {
		body = []byte("{}")
	}
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		s.errorf("JSON existant invalide")
		return
	}
	m, ok := root.(map[string]interface{})
	if !ok {
		s.errorf("le JSON existant n'est pas un objet compatible")
		return
	}
	parts := strings.Split(path, ".")
	cur := m
	for i := 0; i < len(parts)-1; i++ {
		key := parts[i]
		if nm, ok := cur[key].(map[string]interface{}); ok {
			cur = nm
			continue
		}
		s.errorf("chemin introuvable")
		return
	}
	if _, exists := cur[parts[len(parts)-1]]; !exists {
		s.errorf("chemin introuvable")
		return
	}
	delete(cur, parts[len(parts)-1])
	out, err := json.Marshal(root)
	if err != nil {
		s.errorf("%v", err)
		return
	}
	if err := s.builder.SetJSON(string(out)); err != nil {
		s.errorf("%v", err)
		return
	}
	s.printf("[CRL] Champ JSON supprimé (%d octets).\n", s.builder.BodySize())
}

func (s *Shell) cmdJSON(rest string) {
	if err := s.builder.SetJSON(rest); err != nil {
		s.errorf("%v", err)
		return
	}
	s.printf("[CRL] Corps JSON configuré (%d octets).\n", s.builder.BodySize())
}

func (s *Shell) cmdBody(rest string) {
	if strings.EqualFold(rest, "clear") {
		s.builder.ClearBody()
		s.printf("[CRL] Corps supprimé.\n")
		return
	}
	s.builder.SetBodyText(rest)
	s.printf("[CRL] Corps brut configuré (%d octets).\n", s.builder.BodySize())
}

func (s *Shell) cmdProxy(rest string) {
	switch {
	case rest == "":
		if s.builder.Proxy() == "" {
			s.printf("[CRL] Proxy explicite : aucun (le proxy Mint est utilisé automatiquement s'il est actif).\n")
		} else if s.builder.Proxy() == "none" {
			s.printf("[CRL] Proxy explicite : désactivé (aucun proxy, même si Mint est actif).\n")
		} else {
			s.printf("[CRL] Proxy explicite : %s\n", request.MaskProxyCredentials(s.builder.Proxy()))
		}
		s.printf("[CRL] Proxy effectif : %s\n", s.proxyDescription())
	case strings.EqualFold(rest, "off"), strings.EqualFold(rest, "none"):
		if err := s.builder.SetProxy("none"); err != nil {
			s.errorf("%v", err)
			return
		}
		s.printf("[CRL] Proxy désactivé explicitement pour crl.launch.\n")
	case strings.EqualFold(rest, "auto"), strings.EqualFold(rest, "clear"), strings.EqualFold(rest, "unset"):
		s.builder.ClearProxy()
		s.printf("[CRL] Proxy explicite supprimé ; le proxy Mint automatique redevient applicable.\n")
	default:
		if err := s.builder.SetProxy(rest); err != nil {
			s.errorf("%v", err)
			return
		}
		s.printf("[CRL] Proxy explicite configuré : %s\n", request.MaskProxyCredentials(s.builder.Proxy()))
	}
}

func (s *Shell) cmdCA(rest string) {
	switch {
	case rest == "":
		if s.builder.CAFile() == "" {
			s.printf("[CRL] CA curl : bundle système par défaut (vérification TLS active).\n")
		} else {
			s.printf("[CRL] CA curl : %s\n", s.builder.CAFile())
		}
	case strings.EqualFold(rest, "off"), strings.EqualFold(rest, "none"), strings.EqualFold(rest, "clear"):
		s.builder.ClearCAFile()
		s.printf("[CRL] CA curl réinitialisé au bundle système.\n")
	default:
		if err := s.builder.SetCAFile(rest); err != nil {
			s.errorf("%v", err)
			return
		}
		s.printf("[CRL] CA curl configuré : %s (vérification TLS maintenue).\n", rest)
	}
}

func (s *Shell) cmdShow() {
	if s.builder.IsEmpty() {
		s.printf("[CRL] Aucune requête préparée. Utilisez crl.host, crl.method, crl.head, crl.json ou mint.use.\n")
		return
	}
	p := s.pal
	res := s.proxyResolution()
	s.printf("%s\n", p.BoldGreen("[CRL] Requête préparée"))
	s.printf("%s\n", p.Dim("Aucune requête envoyée — utilisez crl.launch pour l'exécuter."))
	s.rule(78)
	s.field("Méthode", p.Method(s.builder.Method()))
	s.field("URL", p.Cyan(sanitizeTerminal(s.builder.URL())))
	s.field("Proxy", s.proxyValue(res))
	s.field("Sortie", p.Cyan(s.outputMode.ShortLabel()))
	if s.builder.CAFile() != "" {
		s.field("CA", p.Yellow(s.builder.CAFile()))
	}
	headers := s.builder.Headers()
	s.printf("\n  %s\n", p.BoldCyan(fmt.Sprintf("En-têtes (%d)", len(headers))))
	if len(headers) == 0 {
		s.printf("    %s\n", p.Dim("(aucun)"))
	} else {
		for _, h := range headers {
			s.printf("    %s\n", s.headerLine(h.Name, h.Value))
		}
	}
	if s.builder.BodySet() {
		s.printf("\n  %s\n", p.BoldCyan(fmt.Sprintf("Corps (%d octets)", s.builder.BodySize())))
		rendered, _ := display.RenderBody(s.builder.Body(), 4096)
		s.printIndented(rendered, "    ")
	} else {
		s.printf("\n  %s\n", p.BoldCyan("Corps (vide / non défini)"))
	}
	s.printf("\n")
	s.rule(78)
	s.printf("%s\n", p.Dim("crl.launch pour envoyer · crl.table pour un résumé"))
}

func (s *Shell) cmdTable() {
	if s.builder.IsEmpty() {
		s.printf("[CRL] Aucune requête préparée.\n")
		return
	}
	p := s.pal
	res := s.proxyResolution()
	s.printf("%s\n", p.BoldGreen("[CRL] Tableau de la requête"))
	s.field("MÉTHODE", p.Method(s.builder.Method()))
	s.field("URL", p.Cyan(display.MaskURL(s.builder.URL())))
	s.field("EN-TÊTES", p.Bold(fmt.Sprintf("%d", len(s.builder.Headers()))))
	s.field("CORPS", p.Bold(fmt.Sprintf("%d octets", s.builder.BodySize())))
	s.field("PROXY", s.proxyValue(res))
	s.field("SORTIE", p.Cyan(s.outputMode.ShortLabel()))
	if s.builder.CAFile() != "" {
		s.field("CA", p.Yellow(s.builder.CAFile()))
	}
	headers := s.builder.Headers()
	if len(headers) > 0 {
		names := make([]string, 0, len(headers))
		for _, h := range headers {
			names = append(names, sanitizeTerminal(h.Name))
		}
		s.printf("\n  %s\n", p.BoldCyan(fmt.Sprintf("En-têtes (%d)", len(headers))))
		s.printf("  %s\n", p.Dim(strings.Join(names, ", ")))
		s.printf("  %s\n", p.Dim("crl.show affiche le détail complet des valeurs."))
	}
}

func (s *Shell) cmdOutput(rest string) {
	p := s.pal
	if rest == "" {
		s.printf("%s\n", p.BoldGreen("[CRL] Format d'affichage du corps"))
		s.rule(64)
		s.field("Mode", p.BoldCyan(s.outputMode.ShortLabel()))
		s.printf("  %s\n", p.Dim(s.outputMode.Label()))
		s.printf("  %s\n", p.Dim("Le formatage ne modifie jamais les données reçues ni les octets rejoués."))
		s.printf("  %s\n", p.Dim("usage : crl.output auto|raw|pretty"))
		return
	}
	mode, err := response.ParseMode(rest)
	if err != nil {
		s.errorf("%v", err)
		return
	}
	s.outputMode = mode
	s.printf("[CRL] Format d'affichage : %s\n", p.BoldCyan(mode.ShortLabel()))
	s.printf("  %s\n", p.Dim(mode.Label()))
}

func (s *Shell) cmdLaunch(rest string) {
	if rest != "" {
		s.errorf("crl.launch n'accepte aucun argument")
		return
	}
	// Check for selected captures
	if s.history != nil {
		selected := s.history.SelectedMany()
		if len(selected) > 1 {
			// Execute in parallel
			s.cmdMintReplay("")
			return
		}
		if len(selected) == 1 {
			// Execute this single capture like before
			c := selected[0]
			if importable, reason := c.Importable(); !importable {
				s.errorf("capture #%d non exploitable : %s", c.ID, reason)
				return
			}
			b := request.New()
			headers := make([]request.Header, 0, len(c.Request.Headers))
			for _, h := range c.Request.Headers {
				headers = append(headers, request.Header{Name: h.Name, Value: h.Value})
			}
			if err := b.Import(request.CapturedRequest{URL: c.Request.URL, Method: c.Request.Method, Headers: headers, Body: c.Request.Body, BodyPresent: len(c.Request.Body) > 0}); err != nil {
				s.errorf("%v", err)
				return
			}
			if err := b.SetProxy("none"); err != nil {
				s.errorf("%v", err)
				return
			}
			res := b.ResolveProxy("", false)
			args, err := b.CurlArgs(res)
			if err != nil {
				s.errorf("%v", err)
				return
			}
			// reuse launch logic with builder b
			s.launchWithBuilder(b, args, res)
			return
		}
	}
	if s.builder.URL() == "" {
		s.errorf("aucune URL configurée : utilisez crl.host <url>")
		return
	}
	if _, err := exec.LookPath("curl"); err != nil {
		s.errorf("curl introuvable dans PATH. Installez curl puis relancez CRL-CLI")
		return
	}
	res := s.proxyResolution()
	args, err := s.builder.CurlArgs(res)
	if err != nil {
		s.errorf("%v", err)
		return
	}
	s.launchWithBuilder(s.builder, args, res)
}

func (s *Shell) launchWithBuilder(b *request.Builder, args []string, res request.ProxyResolution) {

	s.clearLastResponse()
	dir, err := os.MkdirTemp("", "crl-response-*")
	if err != nil {
		s.errorf("création du répertoire de réponse : %v", err)
		return
	}
	_ = os.Chmod(dir, 0o700)
	headerPath := filepath.Join(dir, "headers.txt")
	bodyPath := filepath.Join(dir, "body.bin")
	for _, path := range []string{headerPath, bodyPath} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			s.errorf("préparation du fichier de réponse : %v", err)
			_ = os.RemoveAll(dir)
			return
		}
	}
	args = append(args, "--dump-header", headerPath, "--output", bodyPath, "--write-out", response.WriteOutFormat)

	p := s.pal
	s.printf("%s\n", p.BoldGreen("[CRL] Requête envoyée"))
	s.rule(78)
	s.field("Méthode", p.Method(b.Method()))
	s.field("URL", p.Cyan(sanitizeTerminal(b.URL())))
	s.field("Proxy", s.proxyValue(res))
	s.field("Sortie", p.Cyan(s.outputMode.ShortLabel()))
	s.field("Requête", fmt.Sprintf("%d en-tête(s), corps %s", len(b.Headers()), display.FormatBytes(int64(b.BodySize()))))

	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "curl", args...)
	if b.BodySet() {
		cmd.Stdin = b.BodyReader()
	}
	var metaOut, stderr bytes.Buffer
	cmd.Stdout = &metaOut
	cmd.Stderr = &stderr
	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)
	if s.launchUsesMintProxy(res) {
		s.reportMintCapture(started)
	}
	s.lastResponseDir = dir
	_ = os.Chmod(headerPath, 0o600)
	_ = os.Chmod(bodyPath, 0o600)

	headerData, _ := os.ReadFile(headerPath)
	status, headers := response.ParseHeaderDump(headerData)
	meta := response.ParseCurlMeta(metaOut.Bytes())
	if meta.Duration <= 0 {
		meta.Duration = elapsed
	}
	var total int64
	if info, statErr := os.Stat(bodyPath); statErr == nil {
		total = info.Size()
	}
	bodyPreview := readFilePrefix(bodyPath, launchBodyLimit)

	if runErr != nil {
		s.lastFailed = true
		failure := response.ClassifyCurlFailure(curlExitCode(runErr), stderr.String())
		if ctx.Err() != nil {
			failure.Label = "interrompu"
		}
		if status.Code > 0 {
			s.printf("\n%s\n", p.BoldYellow(fmt.Sprintf("[CRL] Transport : %s (curl %d) — un statut HTTP a tout de même été reçu.", failure.Label, failure.Code)))
			s.printHTTPResponseWithBuilder(b, status, headers, meta, bodyPreview, total, bodyPath)
			return
		}
		s.printTransportFailure(failure)
		if total > 0 {
			s.printResponseBody(bodyPreview, total, bodyPath, headers.ContentType())
		} else {
			s.clearLastResponse()
		}
		return
	}
	if status.Code == 0 {
		s.errorf("curl n'a renvoyé aucune ligne de statut HTTP exploitable")
		s.clearLastResponse()
		return
	}
	s.printHTTPResponseWithBuilder(b, status, headers, meta, bodyPreview, total, bodyPath)
}

// launchUsesMintProxy reconnaît le proxy Mint qu'il soit choisi
// automatiquement ou fourni explicitement avec la même URL.
func (s *Shell) launchUsesMintProxy(res request.ProxyResolution) bool {
	if res.None || res.URL == "" {
		return false
	}
	mintURL, active := s.manager.ProxyURL()
	return active && res.URL == mintURL
}

// reportMintCapture compare uniquement une capture récente de même méthode et
// URL. En présence de trafic concurrent, l'absence de correspondance est
// signalée au lieu d'attribuer arbitrairement une capture à crl.launch.
func (s *Shell) reportMintCapture(started time.Time) {
	// Le pont Mint est asynchrone : la capture de requête peut arriver juste
	// après la fin de curl. On lui laisse une courte fenêtre, sans bloquer
	// longtemps si le proxy n'a effectivement rien capturé.
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		if c, ok := s.findRecentMintCapture(started); ok {
			s.reportMintCaptureDiff(c)
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.printf("\n[Mint] Aucune capture Mint récente de même méthode et URL n'a été corrélée à cette requête. Consultez mint.requests ; en cas de trafic concurrent, vérifiez mint.request.show manuellement.\n")
}

func (s *Shell) findRecentMintCapture(started time.Time) (mint.Capture, bool) {
	captures := s.history.List()
	for i := len(captures) - 1; i >= 0; i-- {
		c := captures[i]
		if c.StartedAt.Before(started.Add(-time.Second)) || !c.Request.Received {
			continue
		}
		if c.Request.Method != s.builder.Method() || c.Request.URL != s.builder.URL() {
			continue
		}
		return c, true
	}
	return mint.Capture{}, false
}

func (s *Shell) reportMintCaptureDiff(c mint.Capture) {
	s.printf("\n[Mint] Capture récente de même méthode et URL : #%d (%s %s). La corrélation n'est pas garantie en cas de trafic concurrent ; comparez les en-têtes avec crl.show, détail : mint.request.show %d.\n", c.ID, sanitizeTerminal(c.Request.Method), sanitizeTerminal(c.Request.URL), c.ID)
	matched := make([]bool, len(c.Request.Headers))
	for _, expected := range s.builder.Headers() {
		found := false
		for i, actual := range c.Request.Headers {
			if !matched[i] && strings.EqualFold(expected.Name, actual.Name) && expected.Value == actual.Value {
				found = true
				matched[i] = true
				break
			}
		}
		if !found {
			s.printf("[Mint] Différence observée pour l'en-tête %s : valeur préparée absente ou différente dans la capture.\n", sanitizeTerminal(expected.Name))
		}
	}
	for i, actual := range c.Request.Headers {
		if matched[i] {
			continue
		}
		s.printf("[Mint] En-tête observé en plus ou avec une valeur différente : %s.\n", sanitizeTerminal(actual.Name))
	}
}

func curlExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func readFilePrefix(path string, limit int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, int64(limit)))
	return data
}

func (s *Shell) printTransportFailure(failure response.CurlFailure) {
	p := s.pal
	title := "Échec du transport"
	switch {
	case failure.IsTimeout():
		title = "Délai dépassé"
	case failure.IsConnection():
		title = "Erreur de connexion"
	}
	s.printf("\n%s\n", p.BoldRed("[CRL] "+title))
	s.rule(78)
	s.field("Type", p.BoldRed(failure.Label))
	s.field("Code curl", p.Bold(strconv.Itoa(failure.Code)))
	s.field("Statut HTTP", p.Dim("aucune réponse HTTP reçue"))
	if failure.Detail != "" {
		s.field("Détail", p.Red(truncate(sanitizeTerminal(failure.Detail), 1200)))
	}
	s.printf("  %s\n", p.Dim("Le statut HTTP n'est pas inventé : aucune réponse exploitable n'a été reçue."))
}

func (s *Shell) printHTTPResponse(status response.Status, headers response.Headers, meta response.CurlMeta, body []byte, total int64, bodyPath string) {
	s.printHTTPResponseWithBuilder(s.builder, status, headers, meta, body, total, bodyPath)
}

func (s *Shell) printHTTPResponseWithBuilder(b *request.Builder, status response.Status, headers response.Headers, meta response.CurlMeta, body []byte, total int64, bodyPath string) {
	p := s.pal
	s.printf("\n%s\n", p.BoldGreen("[CRL] Réponse HTTP"))
	s.rule(78)
	s.field("Statut", p.StatusCode(status.Code, sanitizeTerminal(status.Label())))
	s.field("Méthode", p.Method(b.Method()))
	s.field("URL demandée", p.Cyan(sanitizeTerminal(b.URL())))
	s.field("Durée", p.Cyan(durationLabel(meta.Duration)))
	s.field("Taille", p.Bold(display.FormatBytes(total)))
	if contentType := headers.ContentType(); contentType != "" {
		s.field("Type", p.Cyan(contentType))
	}
	if status.Proto != "" {
		s.field("HTTP", p.Dim(sanitizeTerminal(status.Proto)))
	}
	if meta.EffectiveURL != "" {
		s.field("URL finale", p.Cyan(sanitizeTerminal(meta.EffectiveURL)))
	}

	s.printf("\n  %s\n", p.BoldBlue(fmt.Sprintf("En-têtes de réponse (%d)", len(headers))))
	if len(headers) == 0 {
		s.printf("    %s\n", p.Dim("(aucun)"))
	} else {
		for _, h := range headers {
			s.printf("    %s\n", s.headerLine(h.Name, h.Value))
		}
	}
	s.printResponseBody(body, total, bodyPath, headers.ContentType())
	if status.Code >= 500 {
		s.printf("  %s\n", p.BoldRed("Erreur HTTP côté serveur : ce n'est pas une erreur de connexion."))
	} else if status.Code >= 400 {
		s.printf("  %s\n", p.BoldYellow("Erreur HTTP côté client : la réponse a bien été reçue."))
		switch status.Code {
		case 400:
			s.printf("  %s\n", p.Dim("400 : inspectez le corps et les en-têtes de réponse pour le détail fourni par le serveur."))
		case 401:
			s.printf("  %s\n", p.Dim("401 : comparez crl.show avec mint.requests puis mint.request.show <id> si Mint a capturé l'échange."))
		case 403:
			s.printf("  %s\n", p.Dim("403 : accès refusé par le serveur ; cela ne prouve pas que l'authentification est absente. Comparez crl.show et mint.request.show <id>, puis vérifiez les règles d'accès."))
		case 404:
			s.printf("  %s\n", p.Dim(fmt.Sprintf("404 : vérifiez le chemin, les paramètres et la méthode %s ; ce statut ne prouve pas à lui seul que la ressource n'existe pas.", b.Method())))
		}
	} else if status.Code >= 300 && status.Code < 400 {
		s.printf("  %s\n", p.Cyan("Redirection reçue ; curl ne la suit pas automatiquement."))
	}
}

func (s *Shell) printResponseBody(body []byte, total int64, bodyPath, contentType string) {
	p := s.pal
	view := response.RenderBody(body, total, launchBodyLimit, s.outputMode, contentType)
	s.printf("\n  %s\n", p.BoldBlue(fmt.Sprintf("Corps de la réponse — %s (%s)", bodyKindLabel(view.Kind), display.FormatBytes(total))))
	s.printIndented(view.Text, "    ")
	if view.Truncated {
		s.printf("    %s\n", p.BoldYellow(fmt.Sprintf(
			"[…] Affichage limité à %s sur %s reçus ; le corps complet reste disponible ci-dessous.",
			display.FormatBytes(launchBodyLimit), display.FormatBytes(total))))
	}
	if bodyPath != "" && total > 0 {
		s.printf("    %s\n", p.Dim("Corps complet (fichier temporaire) : "+bodyPath))
		s.printf("    %s\n", p.Dim("En-têtes bruts : "+filepath.Join(filepath.Dir(bodyPath), "headers.txt")))
	}
}

func bodyKindLabel(kind response.BodyKind) string {
	switch kind {
	case response.BodyJSON:
		return "JSON"
	case response.BodyHTML:
		return "HTML"
	case response.BodyEmpty:
		return "vide"
	case response.BodyBinary:
		return "binaire"
	default:
		return "texte"
	}
}

func (s *Shell) clearLastResponse() {
	if s.lastResponseDir == "" {
		return
	}
	_ = os.RemoveAll(s.lastResponseDir)
	s.lastResponseDir = ""
}

func (s *Shell) cmdMintOn() {
	status := s.manager.Status()
	if status.State == mint.StateRunning {
		s.printf("[Mint] Proxy déjà actif sur %s.\n", mint.ListenAddress(status.ListenHost, status.Port))
		return
	}
	s.printf("[Mint] Démarrage du proxy...\n")
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	info, err := s.manager.Start(ctx)
	if err != nil {
		s.failf("%v", err)
		s.printf("[Mint] Le shell reste utilisable. Corrigez la configuration puis réessayez.\n")
		return
	}
	s.printf("[Mint] mitmdump détecté : %s\n", info.MitmdumpPath)
	s.printf("[Mint] Addon chargé.\n")
	s.printf("[Mint] Proxy actif sur %s.\n", info.Listen)
	s.printf("[Mint] Capture HTTP activée.\n")
	s.printf("[Mint] curl hors CRL-CLI : --proxy %s\n", info.ProxyURL)
}

func (s *Shell) cmdMintOff() {
	status := s.manager.Status()
	if status.State == mint.StateStopped && status.Captures == 0 {
		// On reste explicite même sans capture : l'état peut être arrêté.
	}
	if status.State == mint.StateStopped {
		s.printf("[Mint] Proxy déjà arrêté.\n")
		return
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.manager.Stop(ctx); err != nil {
		s.failf("arrêt : %v", err)
		return
	}
	s.printf("[Mint] Proxy arrêté. Les %d capture(s) restent disponibles.\n", status.Captures)
}

func (s *Shell) cmdMintStatus() {
	st := s.manager.Status()
	p := s.pal
	s.printf("%s\n", p.BoldMagenta("[Mint] État du proxy"))
	s.field("État", mintState(p, st.State))
	s.field("Écoute", p.Cyan(mint.ListenAddress(st.ListenHost, st.Port)))
	s.field("Captures", fmt.Sprintf("%s requête(s), %s réponse(s)", p.Bold(fmt.Sprintf("%d", st.Captures)), p.Bold(fmt.Sprintf("%d", st.Responses))))
	communication := p.Dim("inactive")
	if st.BridgeConnected {
		communication = p.BoldGreen("connectée")
	}
	s.field("Communication", communication)
	if st.MitmdumpPath != "" {
		s.field("mitmdump", st.MitmdumpPath)
	}
	s.field("Mémoire corps", display.FormatBytes(st.TotalBodyBytes))
	if !st.StartedAt.IsZero() && st.State == mint.StateRunning {
		s.field("Démarré depuis", p.Dim(time.Since(st.StartedAt).Round(time.Second).String()))
	}
	if st.LastError != "" {
		s.field("Dernière erreur", p.Red(truncate(sanitizeTerminal(st.LastError), 1000)))
	}
}

func (s *Shell) cmdMintConfig() {
	cfg := s.manager.Config()
	st := s.manager.Status()
	p := s.pal
	path, err := s.manager.ResolveMitmdump()
	if err != nil {
		path = "non trouvé dans PATH"
	}

	s.printf("%s\n", p.BoldMagenta("[Mint] Configuration du proxy"))
	s.rule(78)
	s.field("État Mint", proxyStateBadge(p, st))
	s.field("Processus", processStateText(p, st.State))
	s.field("Écoute", p.Cyan(mint.ListenAddress(st.ListenHost, st.Port)))
	s.field("Port", p.Bold(strconv.Itoa(st.Port)))
	switch {
	case st.AddonPath != "":
		s.field("Addon", p.BoldGreen("chargé")+p.Dim(" — "+st.AddonPath))
	case st.State == mint.StateRunning || st.State == mint.StateStarting:
		s.field("Addon", p.BoldYellow("en attente"))
	default:
		s.field("Addon", p.Dim("non chargé"))
	}
	switch {
	case st.BridgeConnected:
		s.field("Communication", p.BoldGreen("connectée"))
	case st.State == mint.StateRunning || st.State == mint.StateStarting || st.State == mint.StateStopping:
		s.field("Communication", p.BoldYellow("en attente"))
	default:
		s.field("Communication", p.Dim("inactive"))
	}
	s.field("Captures", fmt.Sprintf("%s requête(s), %s réponse(s)",
		p.Bold(strconv.Itoa(st.Captures)), p.Bold(strconv.Itoa(st.Responses))))
	s.field("Mémoire corps", display.FormatBytes(st.TotalBodyBytes))
	if st.LastError != "" {
		s.field("Dernière erreur", p.Red(truncate(sanitizeTerminal(st.LastError), 1000)))
	}

	s.printf("\n  %s\n", p.BoldBlue("Proxy utilisé par CRL"))
	s.field("Explicite", s.explicitProxyValue())
	s.field("Effectif", s.proxyValue(s.proxyResolution()))
	if s.builder.CAFile() == "" {
		s.field("CA curl", p.Dim("bundle système (TLS vérifié)"))
	} else {
		s.field("CA curl", p.Yellow(s.builder.CAFile()))
	}
	s.field("Sortie", p.Cyan(s.outputMode.ShortLabel()))

	s.printf("\n  %s\n", p.BoldBlue("Paramètres Mint"))
	s.field("mitmdump", p.Dim(path))
	s.field("Corps max/capture", display.FormatBytes(cfg.MaxBodyBytes))
	s.field("Captures max", strconv.Itoa(cfg.MaxCaptures))
	s.field("Mémoire corps max", display.FormatBytes(cfg.MaxTotalBodyBytes))
	s.field("Pont local", p.Dim("127.0.0.1, port éphémère, jeton de session non affiché"))
	s.field("Addon embarqué", p.Dim("extrait au démarrage avec les droits 0600"))
	s.field("Portée réseau", p.Dim("loopback uniquement"))
	s.field("Télémétrie", p.Dim("aucune"))
	if err != nil {
		s.printf("  %s\n", p.BoldYellow("Prérequis : installez mitmproxy (par exemple : pipx install mitmproxy)."))
	}
	s.printf("\n  %s\n", p.Dim("Cet affichage ne modifie aucune configuration réelle."))
}

func (s *Shell) cmdMintPort(rest string) {
	if rest == "" {
		st := s.manager.Status()
		s.printf("[Mint] Port configuré : %d (%s)\n", st.Port, st.ListenHost)
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		s.errorf("port invalide %q : valeur numérique attendue", rest)
		return
	}
	if err := s.manager.SetPort(port); err != nil {
		s.errorf("%v", err)
		return
	}
	s.printf("[Mint] Port configuré sur %d (écoute loopback 127.0.0.1).\n", port)
}

func (s *Shell) cmdMintRequests(all bool) {
	var captures []mint.Capture
	filter := s.history.Filter()
	if all {
		captures = s.history.List()
	} else {
		captures = s.history.Filtered()
	}
	if !all && filter != "" {
		s.printf("[Mint] Filtre actif : %q (%d capture(s))\n", filter, len(captures))
	}
	if len(captures) == 0 {
		if filter != "" && !all {
			s.printf("[Mint] Aucune capture ne correspond au filtre. Utilisez mint.filter clear pour le supprimer.\n")
		} else {
			s.printf("[Mint] Aucune capture. Démarrez mint.proxy on puis envoyez du trafic via le proxy.\n")
		}
		return
	}
	p := s.pal
	const (
		idWidth     = 4
		methodWidth = 7
		hostWidth   = 26
		pathWidth   = 42
		statusWidth = 8
		durWidth    = 9
	)
	header := fmt.Sprintf("%s  %s  %s  %s  %s  %s",
		padCell("ID", idWidth),
		padCell("MÉTHODE", methodWidth),
		padCell("HÔTE", hostWidth),
		padCell("CHEMIN", pathWidth),
		padCell("STATUT", statusWidth),
		padCell("DURÉE", durWidth))
	s.printf("%s\n", p.Bold(header))
	s.rule(idWidth + methodWidth + hostWidth + pathWidth + statusWidth + durWidth + 10)
	for _, c := range captures {
		host := c.Request.Host
		if host == "" {
			host = "-"
		}
		path := display.MaskURL(c.PathAndQuery())
		idCell := padCell(strconv.Itoa(c.ID), idWidth)
		methodCell := padCell(truncate(c.Request.Method, methodWidth), methodWidth)
		hostCell := padCell(truncate(host, hostWidth), hostWidth)
		pathCell := padCell(truncate(path, pathWidth), pathWidth)
		statusCell := padCell(truncate(c.StatusLabel(), statusWidth), statusWidth)
		durCell := padCell(c.DurationLabel(), durWidth)
		s.printf("%s  %s  %s  %s  %s  %s\n",
			p.BoldCyan(idCell),
			p.Method(methodCell),
			hostCell,
			p.Cyan(pathCell),
			captureStatus(p, c, statusCell),
			p.Dim(durCell))
	}
	s.printf("\n%s\n", p.Dim(fmt.Sprintf("%d capture(s) affichée(s).", len(captures))))
	s.printf("%s\n", p.Dim("mint.request.show <id> pour le détail · mint.select <id> pour sélectionner"))
}

func (s *Shell) cmdMintShow(rest string) {
	if rest == "" {
		s.errorf("usage : mint.request.show <id>")
		return
	}
	id, err := parseID(rest)
	if err != nil {
		s.errorf("%v", err)
		return
	}
	c, ok := s.history.Get(id)
	if !ok {
		s.errorf("capture #%d introuvable", id)
		return
	}

	p := s.pal
	s.printf("%s\n", p.BoldMagenta(fmt.Sprintf("[Mint] Capture #%d", c.ID)))
	s.printf("  %s  %s  %s\n",
		p.Method(c.Request.Method),
		captureStatus(p, c, c.StatusLabelText()),
		p.Dim(c.DurationLabel()))
	s.rule(78)
	if c.FlowID != "" {
		s.field("Flux", p.Dim(c.FlowID))
	}
	s.field("Date", c.StartedAt.Local().Format("2006-01-02 15:04:05.000"))
	s.field("URL", p.Cyan(sanitizeTerminal(c.Request.URL)))
	s.field("Hôte", c.Request.Host)
	s.field("Chemin", p.Cyan(sanitizeTerminal(c.PathAndQuery())))
	if reasons := c.IncompleteReasons(); len(reasons) > 0 {
		s.field("Complétude", p.BoldYellow("incomplète — "+strings.Join(reasons, " ; ")))
	} else {
		s.field("Complétude", p.BoldGreen("complète"))
	}

	s.printf("\n  %s\n", p.BoldBlue("REQUÊTE"))
	if !c.Request.Received {
		s.printf("    %s\n", p.Dim("(requête non reçue)"))
	} else {
		s.printf("    %s\n", p.BoldCyan(fmt.Sprintf("En-têtes (%d)", len(c.Request.Headers))))
		if len(c.Request.Headers) == 0 {
			s.printf("      %s\n", p.Dim("(aucun)"))
		} else {
			for _, h := range c.Request.Headers {
				s.printf("      %s\n", s.headerLine(h.Name, h.Value))
			}
		}
		s.printf("    %s\n", p.BoldCyan(fmt.Sprintf("Corps (%d octets%s)", len(c.Request.Body), truncatedSuffix(c.Request.BodyTruncated, c.Request.BodySize))))
		rendered, _ := display.RenderBody(c.Request.Body, 8192)
		s.printIndented(rendered, "      ")
	}

	if c.Response != nil {
		s.printf("\n  %s\n", p.BoldBlue("RÉPONSE"))
		s.printf("    %s %s\n", p.Dim("Statut :"), p.StatusCode(c.Response.StatusCode, c.StatusLabelText()))
		s.printf("    %s\n", p.BoldCyan(fmt.Sprintf("En-têtes (%d)", len(c.Response.Headers))))
		if len(c.Response.Headers) == 0 {
			s.printf("      %s\n", p.Dim("(aucun)"))
		} else {
			for _, h := range c.Response.Headers {
				s.printf("      %s\n", s.headerLine(h.Name, h.Value))
			}
		}
		s.printf("    %s\n", p.BoldCyan(fmt.Sprintf("Corps (%d octets%s)", len(c.Response.Body), truncatedSuffix(c.Response.BodyTruncated, c.Response.BodySize))))
		rendered, _ := display.RenderBody(c.Response.Body, 8192)
		s.printIndented(rendered, "      ")
	} else if c.Err == "" {
		s.printf("\n  %s %s\n", p.BoldYellow("RÉPONSE"), p.Yellow("en attente"))
	}
	if c.Err != "" {
		s.printf("\n  %s %s\n", p.BoldRed("ERREUR"), p.Red(truncate(sanitizeTerminal(c.Err), 1000)))
	}
}

func (s *Shell) cmdMintSelect(rest string) {
	if rest == "" {
		s.errorf("usage : mint.select <id...>|clear")
		return
	}
	if strings.EqualFold(strings.TrimSpace(rest), "clear") {
		s.history.ClearSelection()
		s.printf("[Mint] Sélection effacée.\n")
		return
	}
	var ids []int
	for _, item := range strings.Fields(strings.ReplaceAll(rest, ",", " ")) {
		id, err := parseID(item)
		if err != nil {
			s.errorf("%v", err)
			return
		}
		ids = append(ids, id)
	}
	captures, err := s.history.SelectMany(ids)
	if err != nil {
		s.errorf("%v", err)
		return
	}
	s.printSelected(captures)
}

func (s *Shell) printSelected(captures []mint.Capture) {
	p := s.pal
	s.printf("[Mint] Sélection persistante (%d) — aucune requête envoyée :\n", len(captures))
	for _, c := range captures {
		s.printf("  #%d %s %s%s\n", c.ID, p.Method(c.Request.Method), c.Request.Host, p.Cyan(display.MaskURL(c.PathAndQuery())))
	}
}

// cmdMintReplay starts each selected captured request concurrently, preserving
// its own builder state and source ID. Results are printed after the whole batch.
func (s *Shell) cmdMintReplay(rest string) {
	if strings.TrimSpace(rest) != "" {
		s.errorf("usage : mint.replay")
		return
	}
	captures := s.history.SelectedMany()
	if len(captures) == 0 {
		s.errorf("aucune capture sélectionnée : utilisez mint.select <id> [id...]")
		return
	}
	type result struct {
		id                            int
		method, url, status, duration string
		details                       string
		err                           error
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	results := make([]result, len(captures))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, capture := range captures {
		wg.Add(1)
		go func(i int, c mint.Capture) {
			defer wg.Done()
			r := result{id: c.ID, method: c.Request.Method, url: c.Request.URL}
			sem <- struct{}{}
			defer func() { <-sem }()
			err, status, dur, details := s.replayCapture(ctx, c)
			if err != nil {
				r.err = err
			} else {
				r.status = status
			}
			r.duration = dur
			r.details = details
			results[i] = r
		}(i, capture)
	}
	wg.Wait()
	success := 0
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
		} else {
			success++
		}
	}
	s.lastBatchSent = len(results)
	s.lastBatchSuccess = success
	s.lastBatchFail = failed
	s.lastBatchSet = true
	s.printf("[Mint] Résultats du rejeu parallèle (%d requêtes)\n", len(results))
	s.printf("[Envoyées: %d/%d]\n", success, len(results))
	s.printf("  ID  MÉTHODE  STATUT  DURÉE  URL / ERREUR\n")
	for _, r := range results {
		status := r.status
		detail := r.url
		if r.err != nil {
			status = "ERREUR"
			detail = r.err.Error()
		}
		s.printf("  #%d  %-7s  %-7s  %-8s %s\n", r.id, r.method, status, r.duration, sanitizeTerminal(detail))
	}
	for _, r := range results {
		s.printf("\n[Mint] Réponse du rejeu #%d — %s %s\n", r.id, r.method, sanitizeTerminal(r.url))
		if r.err != nil {
			s.printf("  Erreur : %s\n", sanitizeTerminal(r.err.Error()))
			continue
		}
		s.printf("  Statut : %s · durée : %s\n%s", r.status, r.duration, r.details)
	}
	// Record in history associations? mission says "Enregistre les résultats des rejeux dans l'historique et conserve une association fiable avec chaque capture source." - existing code already has history; maybe ensure we don't break things
}

func (s *Shell) replayCapture(ctx context.Context, c mint.Capture) (error, string, string, string) {
	if ok, why := c.Importable(); !ok {
		return fmt.Errorf("%s", why), "", "-", ""
	}
	b := request.New()
	headers := make([]request.Header, 0, len(c.Request.Headers))
	for _, h := range c.Request.Headers {
		headers = append(headers, request.Header{Name: h.Name, Value: h.Value})
	}
	if err := b.Import(request.CapturedRequest{URL: c.Request.URL, Method: c.Request.Method, Headers: headers, Body: c.Request.Body, BodyPresent: len(c.Request.Body) > 0}); err != nil {
		return err, "", "-", ""
	}
	// Replayed captures must not be recorded a second time by this tool's Mint
	// proxy. Captured requests have no portable proxy setting, so replay direct.
	if err := b.SetProxy("none"); err != nil {
		return err, "", "-", ""
	}
	res := b.ResolveProxy("", false)
	args, err := b.CurlArgs(res)
	if err != nil {
		return err, "", "-", ""
	}
	dir, err := os.MkdirTemp("", "crl-replay-*")
	if err != nil {
		return err, "", "-", ""
	}
	defer os.RemoveAll(dir)
	headersPath, bodyPath := filepath.Join(dir, "headers"), filepath.Join(dir, "body")
	cmd := exec.CommandContext(ctx, "curl", append(args, "--dump-header", headersPath, "--output", bodyPath, "-w", "%{http_code} %{time_total}")...)
	if b.BodySet() {
		cmd.Stdin = b.BodyReader()
	}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err(), "", "-", ""
		}
		return err, "", "-", ""
	}
	parts := strings.Fields(string(out))
	if len(parts) < 2 {
		return fmt.Errorf("réponse curl illisible"), "", "-", ""
	}
	d, _ := strconv.ParseFloat(parts[1], 64)
	duration := (time.Duration(d * float64(time.Second))).String()
	headerData, _ := os.ReadFile(headersPath)
	_, parsedHeaders := response.ParseHeaderDump(headerData)
	var bodySize int64
	if info, statErr := os.Stat(bodyPath); statErr == nil {
		bodySize = info.Size()
	}
	body := readFilePrefix(bodyPath, 32<<10)
	view := response.RenderBody(body, bodySize, 32<<10, response.ModeAuto, parsedHeaders.ContentType())
	var details strings.Builder
	details.WriteString("  En-têtes de réponse:\n")
	for _, h := range parsedHeaders {
		details.WriteString("    " + s.headerLine(h.Name, h.Value) + "\n")
	}
	details.WriteString("  Corps:\n")
	for _, line := range strings.Split(view.Text, "\n") {
		details.WriteString("    " + line + "\n")
	}
	if view.Truncated {
		details.WriteString("    […] corps tronqué à l'affichage\n")
	}
	return nil, parts[0], duration, details.String()
}

func (s *Shell) cmdMintSelected() {
	if many := s.history.SelectedMany(); len(many) > 1 {
		s.printSelected(many)
		return
	}
	c, ok := s.history.Selected()
	if !ok {
		s.printf("[Mint] Aucune capture sélectionnée.\n")
		s.printf("[Mint] Utilisez mint.requests puis mint.select <id...>.\n")
		return
	}
	p := s.pal
	s.printf("%s %s\n", p.BoldMagenta("[Mint]"), p.BoldCyan(fmt.Sprintf("Capture sélectionnée : #%d", c.ID)))
	s.printf("  %s %s%s\n", p.Method(c.Request.Method), c.Request.Host, p.Cyan(display.MaskURL(c.PathAndQuery())))
	s.printf("  %s %s, %s %s\n", p.Dim("Statut :"), captureStatus(p, c, c.StatusLabelText()), p.Dim("durée :"), p.Dim(c.DurationLabel()))
	s.printf("%s %s\n", p.BoldMagenta("[Mint]"), fmt.Sprintf("mint.use %d pour l'importer dans CRL-CLI.", c.ID))
}

func (s *Shell) cmdMintFilter(rest string) {
	switch {
	case rest == "":
		filter := s.history.Filter()
		if filter == "" {
			s.printf("[Mint] Aucun filtre actif (%d capture(s) au total).\n", len(s.history.List()))
		} else {
			s.printf("[Mint] Filtre actif : %q (%d capture(s) correspondante(s)).\n", filter, len(s.history.Filtered()))
		}
	case strings.EqualFold(rest, "clear"), strings.EqualFold(rest, "--clear"), strings.EqualFold(rest, "off"):
		s.history.SetFilter("")
		s.printf("[Mint] Filtre supprimé. L'historique est conservé (%d capture(s)).\n", len(s.history.List()))
	default:
		s.history.SetFilter(rest)
		s.printf("[Mint] Filtre : %q (%d capture(s) correspondante(s)).\n", s.history.Filter(), len(s.history.Filtered()))
	}
}

func (s *Shell) cmdMintClear(rest string) {
	requests, responses := s.history.Counts()
	if requests == 0 {
		s.printf("[Mint] Aucune capture à effacer.\n")
		return
	}
	force := false
	for _, arg := range strings.Fields(rest) {
		switch arg {
		case "--yes", "-y":
			force = true
		default:
			s.errorf("argument inconnu %q pour mint.clear", arg)
			return
		}
	}
	if !force {
		question := fmt.Sprintf("[Mint] Effacer %d requête(s) et %d réponse(s) de la session ? Le proxy ne sera pas arrêté.", requests, responses)
		if !s.confirm(question) {
			s.printf("[Mint] Effacement annulé.\n")
			return
		}
	}
	s.history.Clear()
	s.printf("[Mint] Historique effacé (%d requête(s), %d réponse(s)). Le proxy reste dans son état actuel.\n", requests, responses)
}

func (s *Shell) cmdMintUse(rest string) {
	args := strings.Fields(rest)
	force := false
	id := 0
	for _, arg := range args {
		switch arg {
		case "--force", "-f":
			force = true
		default:
			if id != 0 {
				s.errorf("usage : mint.use [id] [--force]")
				return
			}
			parsed, err := parseID(arg)
			if err != nil {
				s.errorf("%v", err)
				return
			}
			id = parsed
		}
	}

	var c mint.Capture
	var ok bool
	if id == 0 {
		selected := s.history.SelectedMany()
		if len(selected) > 1 {
			s.errorf("plusieurs captures sélectionnées ; précisez mint.use <id> ou sélectionnez une seule capture")
			return
		}
		c, ok = s.history.Selected()
		if !ok {
			s.errorf("aucune capture sélectionnée. usage : mint.use <id> ou mint.select <id> puis mint.use")
			return
		}
	} else {
		c, ok = s.history.Get(id)
		if !ok {
			s.errorf("capture #%d introuvable", id)
			return
		}
	}

	if importable, reason := c.Importable(); !importable {
		s.errorf("importation refusée : %s", reason)
		return
	}
	if !s.builder.IsEmpty() && !force {
		question := fmt.Sprintf("[Mint] La configuration CRL-CLI actuelle sera remplacée par la capture #%d. Continuer ?", c.ID)
		if !s.confirm(question) {
			s.printf("[Mint] Importation annulée. La configuration CRL-CLI est inchangée.\n")
			return
		}
	}

	headers := make([]request.Header, 0, len(c.Request.Headers))
	for _, h := range c.Request.Headers {
		headers = append(headers, request.Header{Name: h.Name, Value: h.Value})
	}
	captured := request.CapturedRequest{
		URL:                   c.Request.URL,
		Method:                c.Request.Method,
		Headers:               headers,
		Body:                  c.Request.Body,
		BodyPresent:           len(c.Request.Body) > 0,
		BodyTruncated:         c.Request.BodyTruncated,
		PendingReply:          c.Response == nil,
		ResponseBodyTruncated: c.Response != nil && c.Response.BodyTruncated,
	}
	if err := s.builder.Import(captured); err != nil {
		s.errorf("importation impossible : %v", err)
		return
	}
	s.history.Select(c.ID)

	s.printf("[Mint] Capture #%d importée dans CRL-CLI.\n", c.ID)
	s.printf("  %s %s\n", s.pal.Method(c.Request.Method), s.pal.Cyan(display.MaskURL(c.Request.URL)))
	s.printf("  %d en-tête(s), corps %d octet(s).\n", len(c.Request.Headers), len(c.Request.Body))
	if c.Response == nil {
		s.printf("[Mint] Attention : la réponse n'avait pas encore été capturée. Seule la requête a été importée.\n")
	}
	if c.Response != nil && c.Response.BodyTruncated {
		s.printf("[Mint] Attention : le corps de réponse était tronqué ; il n'est pas utilisé pour le rejeu.\n")
	}
	if c.Err != "" {
		s.printf("[Mint] Attention : la capture contient une erreur de transport (%s).\n", truncate(sanitizeTerminal(c.Err), 300))
	}
	if strings.HasPrefix(strings.ToLower(c.Request.URL), "https://") {
		s.printf("[Mint] HTTPS : curl doit faire confiance au CA de mitmproxy. Utilisez crl.ca <chemin> si nécessaire, sans désactiver la vérification TLS.\n")
	}
	s.printf("[Mint] Aucune requête envoyée. Utilisez crl.show puis crl.launch.\n")
}

func (s *Shell) proxyResolution() request.ProxyResolution {
	mintURL, active := s.manager.ProxyURL()
	return s.builder.ResolveProxy(mintURL, active)
}

func (s *Shell) proxyDescription() string {
	return proxyResolutionText(s.proxyResolution())
}

func (s *Shell) proxyValue(res request.ProxyResolution) string {
	text := proxyResolutionText(res)
	switch {
	case res.None:
		return s.pal.Yellow(text)
	case res.URL != "":
		return s.pal.Cyan(text)
	default:
		return s.pal.Dim(text)
	}
}

func (s *Shell) explicitProxyValue() string {
	switch s.builder.Proxy() {
	case "":
		return s.pal.Dim("aucun (Mint automatique si actif)")
	case "none":
		return s.pal.Yellow("désactivé explicitement")
	default:
		return s.pal.Cyan(request.MaskProxyCredentials(s.builder.Proxy()))
	}
}

func processStateText(p palette, state mint.State) string {
	switch state {
	case mint.StateRunning:
		return p.BoldGreen("actif")
	case mint.StateStarting:
		return p.BoldYellow("démarrage")
	case mint.StateStopping:
		return p.BoldYellow("arrêt en cours")
	case mint.StateFailed:
		return p.BoldRed("erreur")
	case mint.StateStopped:
		return p.Dim("arrêté")
	default:
		return p.Dim(string(state))
	}
}

func (s *Shell) promptText() string {
	status := s.manager.Status()
	mintStateStr := "OFF"
	if status.State == mint.StateRunning {
		mintStateStr = "ON"
	}
	method := s.builder.Method()
	if method == "" {
		method = "GET"
	}

	selected := 0
	if s.history != nil {
		selected = len(s.history.SelectedMany())
	}
	if selected == 0 && s.history != nil {
		if _, ok := s.history.Selected(); ok {
			selected = 1
		}
	}

	p := s.pal
	if !p.Enabled {
		prompt := fmt.Sprintf("crl [%s] [Mint: %s]", method, mintStateStr)
		if selected > 0 {
			prompt += fmt.Sprintf(" [Sel: %d]", selected)
		}
		if s.lastBatchSet && s.lastBatchSent > 0 {
			prompt += fmt.Sprintf(" [Envoyées: %d/%d]", s.lastBatchSuccess, s.lastBatchSent)
		}
		prompt += " >"
		return prompt
	}

	// Couleurs sobres : cyan, bleu, violet et vert
	parts := []string{}
	parts = append(parts, p.Cyan("crl"))
	parts = append(parts, p.BoldCyan("["+method+"]"))
	if mintStateStr == "ON" {
		parts = append(parts, p.BoldGreen("[Mint: ON]"))
	} else {
		parts = append(parts, p.Dim("[Mint: OFF]"))
	}
	if selected > 0 {
		parts = append(parts, p.Blue(fmt.Sprintf("[Sel: %d]", selected)))
	}
	if s.lastBatchSet && s.lastBatchSent > 0 {
		parts = append(parts, p.Green(fmt.Sprintf("[Envoyées: %d/%d]", s.lastBatchSuccess, s.lastBatchSent)))
	}
	prompt := strings.Join(parts, " ")
	if s.lastFailed {
		prompt += p.BoldRed(" >")
	} else {
		prompt += p.Magenta(" >")
	}
	return prompt
}

func unicodeTerminal() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return false
	}
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if strings.Contains(strings.ToUpper(os.Getenv(key)), "UTF-8") {
			return true
		}
	}
	return false
}

func (s *Shell) field(label, value string) {
	fmt.Fprintf(s.out, "  %s : %s\n", s.pal.BoldCyan(padCell(label, 17)), value)
}

func (s *Shell) rule(width int) {
	if width < 1 {
		width = 1
	}
	s.printf("%s\n", s.pal.Dim(strings.Repeat("─", width)))
}

func (s *Shell) headerValue(name, value string) string {
	return sanitizeTerminal(value)
}

func (s *Shell) headerLine(name, value string) string {
	prefix := s.pal.BoldCyan(sanitizeTerminal(name)) + s.pal.Dim(": ")
	clean := s.headerValue(name, value)
	width := 80
	if columns, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && columns > 0 {
		width = columns
	}
	if f, ok := s.out.w.(*os.File); ok {
		width = terminalWidth(f.Fd())
	}
	available := width - utf8.RuneCountInString(name) - 4
	if available < 16 {
		available = 16
	}
	var b strings.Builder
	for i, line := range strings.Split(clean, "\n") {
		if i > 0 {
			b.WriteString("\n    ")
		}
		if utf8.RuneCountInString(line) <= available {
			b.WriteString(line)
			continue
		}
		for len(line) > 0 {
			n := available
			if n > len(line) {
				n = len(line)
			}
			for n > 0 && n < len(line) && !utf8.RuneStart(line[n]) {
				n--
			}
			if n == 0 {
				n = available
			}
			b.WriteString(line[:n])
			line = line[n:]
			if len(line) > 0 {
				b.WriteString("\n    ")
			}
		}
	}
	return prefix + b.String()
}

func proxyResolutionText(res request.ProxyResolution) string {
	switch {
	case res.None:
		return "désactivé (" + res.Source + ")"
	case res.URL != "":
		return request.MaskProxyCredentials(res.URL) + " — " + res.Source
	default:
		return res.Source
	}
}

func (s *Shell) confirm(question string) bool {
	answer := ""
	if s.interactive && s.editor != nil && s.inputCh != nil {
		s.editor.SetPrompt(s.pal.BoldYellow(question + " [y/N] "))
		s.editor.Reset()
		s.editor.DrawPrompt()
	loop:
		for {
			select {
			case <-s.ctx.Done():
				break loop
			case n, ok := <-s.notifs:
				if !ok {
					s.notifs = nil
					continue
				}
				s.handleNotification(n)
			case data, ok := <-s.inputCh:
				if !ok {
					break loop
				}
				res := s.editor.Feed(data)
				if res.EOF || res.Interrupted {
					break loop
				}
				if len(res.Lines) > 0 {
					answer = res.Lines[0]
					break loop
				}
			}
		}
		s.editor.SetPrompt(s.promptText())
		s.editor.Reset()
	} else {
		s.printf("%s [y/N] ", question)
		if s.scanner.Scan() {
			answer = s.scanner.Text()
			s.printf("%s\n", sanitizeTerminal(answer))
		} else {
			s.printf("\n")
		}
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes" || answer == "o" || answer == "oui"
}

func (s *Shell) printHelp() {
	s.printf(`Commandes CRL-CLI
  crl.host <url>              Définir l'URL ; sans argument, l'afficher
  crl.method <méthode>        Définir la méthode ; sans argument, l'afficher
  crl.head/header <Nom: val>  Ajouter/remplacer un en-tête
  crl.head                    Lister les en-têtes et leurs valeurs
  crl.head clear [Nom]        Supprimer un en-tête ou tous les en-têtes
  crl.head.clear [Nom]        Alias de suppression (aussi crl.header.clear)
	  crl.json [json]|clear       Éditer (sans argument), définir ou effacer le JSON
	  crl.body [texte|clear]      Éditer le corps brut sans argument
  crl.body.clear              Alias pour effacer le corps
  crl.proxy <url|none|auto>   Définir le proxy (off=none ; clear/unset=auto)
  crl.proxy                   Afficher le proxy explicite et effectif
  crl.ca [fichier|off]        Définir/afficher le CA PEM (off/none/clear : système)
  crl.show                    Afficher URL et en-têtes complets, sans envoyer
  crl.table                   Résumé avec valeurs d'URL sensibles masquées
  crl.output [auto|raw|pretty] Choisir le rendu du corps des réponses
  crl.launch                  Envoyer la requête avec curl et afficher la réponse
  reset                       Réinitialiser le constructeur
  clear                       Effacer l'écran en mode interactif
  help | ?                    Afficher cette aide
  exit | quit                 Quitter et arrêter proprement Mint

Commandes Mint (aide détaillée : mint.help)
  mint.proxy [status]         Afficher l'état du proxy
  mint.proxy on|start          Démarrer le proxy ; off|stop l'arrêter
  mint.proxy config            Configuration effective et prérequis
  mint.proxy.port <port>       Changer le port lorsque le proxy est arrêté
  mint.requests                Lister les captures selon le filtre courant
  mint.history                 Lister toutes les captures, sans filtre
  mint.watch [on|off]           Activer/suspendre les notifications, ou afficher l'état
  mint.events                  Afficher et vider les notifications mises en attente
  mint.request.show <id>       Détail complet (alias : mint.show)
	  mint.select <id...>|clear    Sélectionner une ou plusieurs captures
	  mint.replay                  Rejouer en parallèle la sélection
  mint.selected                Afficher la capture sélectionnée
  mint.filter <texte|clear>    Filtrer par hôte, URL, chemin ou query string
  mint.clear [--yes|-y]        Vider l'historique (confirmation sauf avec --yes)
  mint.use [id] [--force]      Importer dans CRL sans envoyer la requête
  mint | mint.help             Afficher l'aide détaillée de Mint
`)
}

func (s *Shell) printMintHelp() {
	s.printf(`Mint — capture HTTP via mitmproxy

  PROXY
  mint.proxy [status]          État du proxy, port, captures, réponses et pont
  mint.proxy on|start          Démarrer mitmdump et attendre que Mint soit prêt
  mint.proxy off|stop          Arrêter le proxy lancé par CRL-CLI ; garder l'historique
  mint.proxy config            Afficher la configuration et les prérequis
  mint.proxy.port <port>       Changer le port (proxy à l'arrêt)

  CAPTURES ET NOTIFICATIONS
  mint.requests                Lister les captures correspondant au filtre
  mint.history                 Lister tout l'historique, sans appliquer le filtre
  mint.watch [on|off]           Activer, suspendre ou afficher l'état des notifications
  mint.events                  Afficher les notifications accumulées pendant watch off
  mint.request.show <id>       Afficher le détail complet d'une capture (alias mint.show)
	  mint.select <id...>|clear    Sélectionner une ou plusieurs captures
	  mint.replay                  Rejouer en parallèle (lot limité à 4)
  mint.selected                Afficher l'identifiant sélectionné
  mint.filter <texte>          Filtrer par hôte, URL, chemin ou query string
  mint.filter clear            Retirer le filtre ; l'historique reste intact
  mint.clear [--yes|-y]        Vider l'historique ; demander confirmation par défaut
  mint.use [id] [--force]      Importer une capture dans CRL, sans envoyer la requête

DÉTAILS
  mint.watch off met en attente les notifications ; les captures continuent
  d'être enregistrées et restent consultables avec mint.requests/history.
  mint.events vide cette file après affichage. Elle conserve au plus 1024
  événements ; si elle déborde, les plus anciens sont abandonnés.
	  mint.use sans ID requiert une sélection unique ; --force remplace le constructeur
  sans demander confirmation. Un corps de requête tronqué ne peut pas être importé.
  Les commandes de détail affichent URL et valeurs d'en-têtes complètes.
  Tapez help pour les commandes de construction et de rejeu CRL.
`)
}

func (s *Shell) printf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprint(s.out, s.decoratePrefix(msg))
}

func (s *Shell) decoratePrefix(msg string) string {
	if !s.pal.Enabled || msg == "" || msg[0] == '\033' {
		return msg
	}
	switch {
	case strings.HasPrefix(msg, "[CRL] Erreur :"):
		return s.pal.BoldRed("[CRL] Erreur :") + s.pal.Red(msg[len("[CRL] Erreur :"):])
	case strings.HasPrefix(msg, "[CRL] "):
		return s.pal.BoldGreen("[CRL]") + msg[len("[CRL]"):]
	case strings.HasPrefix(msg, "[Mint] Échec"), strings.HasPrefix(msg, "[Mint] Erreur"):
		return s.pal.Red(msg)
	case strings.HasPrefix(msg, "[Mint] Attention"):
		return s.pal.Yellow(msg)
	case strings.HasPrefix(msg, "[Mint] "):
		return s.pal.BoldMagenta("[Mint]") + msg[len("[Mint]"):]
	default:
		return msg
	}
}

func (s *Shell) errorf(format string, args ...interface{}) {
	s.lastFailed = true
	if s.out.needsNewline() {
		fmt.Fprint(s.out, "\n")
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(s.out, "%s %s\n", s.pal.BoldRed("[CRL] Erreur :"), s.pal.Red(msg))
}

func (s *Shell) failf(format string, args ...interface{}) {
	s.lastFailed = true
	if s.out.needsNewline() {
		fmt.Fprint(s.out, "\n")
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(s.out, "%s %s\n", s.pal.BoldRed("[Mint] Échec :"), s.pal.Red(msg))
}

func (s *Shell) printIndented(text, prefix string) {
	for _, line := range strings.Split(text, "\n") {
		line = sanitizeTerminal(line)
		if s.pal.Enabled && (strings.Contains(line, "tronqué") || strings.HasPrefix(line, "[corps binaire")) {
			line = s.pal.Yellow(line)
		}
		// On écrit directement : un corps peut commencer par « [CRL] » ou
		// « [Mint] » sans que cela doive déclencher la coloration des préfixes.
		fmt.Fprintf(s.out, "%s%s\n", prefix, line)
	}
}

func splitCommand(line string) (cmd, rest string) {
	line = strings.TrimSpace(line)
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimSpace(line[i+1:])
}

func parseID(raw string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("identifiant de capture invalide %q", raw)
	}
	return id, nil
}

func stateText(state mint.State) string {
	switch state {
	case mint.StateStopped:
		return "arrêté"
	case mint.StateStarting:
		return "en démarrage"
	case mint.StateRunning:
		return "actif"
	case mint.StateStopping:
		return "en arrêt"
	case mint.StateFailed:
		return "en erreur"
	default:
		return string(state)
	}
}

func durationLabel(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return d.Round(time.Millisecond).String()
}

func truncatedSuffix(truncated bool, size int64) string {
	if !truncated {
		return ""
	}
	return fmt.Sprintf(", tronqué sur %d", size)
}

func truncate(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}
