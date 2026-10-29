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
	"net/url"
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
	// requestEdits holds independent working copies of captured requests.
	requestEdits        map[int]*request.Builder
	requestBodyReplaced map[int]bool
	activeMintID        int
	replayBodies        map[int]string
	replayDirs          []string
	notifs              <-chan mint.Notification

	interactive bool
	editor      *editor
	textArea    *textArea
	textKind    string
	textBuilder *request.Builder
	textTarget  int
	textNotices []string
	textResult  string
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
}

type trackingWriter struct {
	w     io.Writer
	last  byte
	wrote bool

	// block décrit la sortie d'une commande. Le premier octet réellement
	// affiché est précédé d'une ligne vide, et la fin de la commande est
	// normalisée avec une nouvelle ligne. Cela évite que le prompt suivant
	// soit collé à la dernière ligne d'un résultat, sans ajouter de \n\n
	// dans chaque fonction de rendu.
	block        bool
	blockStarted bool
	tail         []byte
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	if !t.block || t.blockStarted {
		return t.write(p)
	}

	// Les rendus existants commencent parfois par un ou deux sauts de ligne.
	// On les absorbe dans la séparation centralisée pour ne pas produire de
	// ligne vide en double.
	start := 0
	for start < len(p) && (p[start] == '\n' || p[start] == '\r') {
		start++
	}
	if start == len(p) {
		// Un premier Write composé uniquement de sauts de ligne ne doit pas
		// démarrer la sortie de la commande.
		return len(p), nil
	}

	// La ligne du prompt vient d'être validée : une ligne vide sépare la
	// commande de son résultat.
	if _, err := t.write([]byte("\r\n")); err != nil {
		return 0, err
	}
	t.blockStarted = true
	if _, err := t.write(p[start:]); err != nil {
		// Les octets initiaux ont été absorbés : on les compte comme écrits
		// pour que l'appelant ne réessaie pas de les envoyer.
		return len(p), err
	}
	return len(p), nil
}

func (t *trackingWriter) write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		t.last = p[n-1]
		t.wrote = true
		t.tail = append(t.tail, p[:n]...)
		if len(t.tail) > 4 {
			t.tail = t.tail[len(t.tail)-4:]
		}
	}
	return n, err
}

func (t *trackingWriter) needsNewline() bool { return t.wrote && t.last != '\n' }

func (t *trackingWriter) reset() {
	t.wrote = false
	t.last = 0
	t.tail = nil
}

func (t *trackingWriter) beginBlock() {
	t.block = true
	t.blockStarted = false
}

// finishBlock normalise la fin d'une commande : nouvelle ligne garantie puis
// une ligne vide avant le prompt suivant. Si le rendu a déjà terminé par une
// ligne vide, aucun saut supplémentaire n'est ajouté.
func (t *trackingWriter) finishBlock() {
	if !t.block || !t.blockStarted {
		t.abortBlock()
		return
	}
	if t.last != '\n' {
		_, _ = t.write([]byte("\r\n"))
	}
	if !t.endsWithBlankLine() {
		_, _ = t.write([]byte("\r\n"))
	}
	t.abortBlock()
}

func (t *trackingWriter) abortBlock() {
	t.block = false
	t.blockStarted = false
}

func (t *trackingWriter) endsWithBlankLine() bool {
	// Un saut de ligne Windows contient \r\n : on compte les \n consécutifs
	// en ignorant les \r intermédiaires.
	newlines := 0
	for i := len(t.tail) - 1; i >= 0; i-- {
		switch t.tail[i] {
		case '\n':
			newlines++
		case '\r':
			continue
		default:
			return newlines >= 2
		}
	}
	return newlines >= 2
}

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
		in:                  opts.In,
		out:                 tw,
		builder:             opts.Builder,
		manager:             opts.Manager,
		history:             opts.Manager.History(),
		requestEdits:        make(map[int]*request.Builder),
		requestBodyReplaced: make(map[int]bool),
		replayBodies:        make(map[int]string),
		notifs:              opts.Notifications,
		mintWatch:           true,
		scanner:             bufio.NewScanner(opts.In),
		pal:                 termstyle.New(opts.Out),
		outputMode:          response.ModeAuto,
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
	s.clearReplayArtifacts()
}

func (s *Shell) runBatch(ctx context.Context) error {
	for s.scanner.Scan() {
		s.drainNotifications()
		if exit := s.runCommand(s.scanner.Text()); exit {
			return nil
		}
		s.drainNotifications()
	}
	return s.scanner.Err()
}

// runCommand exécute une ligne en centralisant la séparation visuelle entre la
// commande, sa sortie et le prompt suivant. Les fonctions de rendu ne gèrent
// donc pas chacune leur propre marge finale.
func (s *Shell) runCommand(line string) bool {
	s.out.reset()
	s.out.beginBlock()
	exit := s.Execute(line)
	if s.textArea != nil {
		// L'éditeur multiligne possède déjà le terminal : il ne faut pas lui
		// ajouter une ligne vide après son cadre.
		s.out.abortBlock()
	} else {
		s.out.finishBlock()
	}
	s.out.reset()
	return exit
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
		var escapeTimer <-chan time.Time
		if s.textArea != nil && s.textArea.EscapePending() {
			remaining := 80*time.Millisecond - time.Since(s.textArea.escapeAt)
			if remaining < 0 {
				remaining = 0
			}
			escapeTimer = time.After(remaining)
		}
		select {
		case <-escapeTimer:
			if s.textArea != nil && s.textArea.CancelPendingEscape() {
				s.closeTextArea(false)
			}
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
			if s.textArea != nil {
				s.resizeTextArea()
				action := s.textArea.Feed(data)
				switch action {
				case textAreaSave:
					s.commitTextArea()
				case textAreaCancel:
					s.closeTextArea(false)
				default:
					s.textArea.Draw(s.pal, unicodeTerminal())
				}
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
				if s.runCommand(line) {
					exit = true
					break
				}
				if s.textArea != nil {
					break
				}
				s.drainNotifications()
			}
			if exit {
				return nil
			}
			if s.textArea != nil {
				continue
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
	if s.textArea != nil {
		s.textNotices = append(s.textNotices, lines...)
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
		s.printHelp(rest)
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
		s.activeMintID = 0
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
			s.showJSONState()
		} else if words := shellWords(rest); len(words) > 1 && (strings.EqualFold(words[1], "set") || strings.EqualFold(words[1], "delete") || strings.EqualFold(words[1], "edit") || strings.EqualFold(words[1], "clear")) {
			s.cmdTargetJSON(rest)
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
			s.showBodyState()
		} else if words := shellWords(rest); len(words) > 1 && (strings.EqualFold(words[1], "edit") || strings.EqualFold(words[1], "clear") || strings.EqualFold(words[1], "set")) {
			s.cmdTargetBody(rest)
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
		s.cmdShow(rest)
	case "crl.table":
		s.cmdTable()
	case "crl.launch":
		s.cmdLaunch(rest)
	case "crl.output":
		s.cmdOutput(rest)
	case "mint", "mint.help":
		s.printMintHelp()
	case "crl.help":
		s.printCrlHelp()
	case "mint.unselect":
		s.cmdMintUnselect(rest)
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
	case "mint.replay.body":
		s.cmdMintReplayBody(rest)
	case "mint.filter":
		s.cmdMintFilter(rest)
	case "mint.clear":
		s.cmdMintClear(rest)
	case "mint.use":
		s.cmdMintUse(rest)
	case "mint.unselect":
		s.cmdMintUnselect(rest)
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
	if words := shellWords(rest); len(words) >= 3 && strings.EqualFold(words[1], "set") {
		captures, err := s.resolveTargets(strings.Join(words[2:], " "))
		if err != nil {
			s.errorf("%v", err)
			return
		}
		s.applyCaptureChanges(captures, func(b *request.Builder) error { return b.SetMethod(words[0]) })
		return
	}
	if err := s.builder.SetMethod(rest); err != nil {
		s.errorf("%v", err)
		return
	}
	s.printf("[CRL] Méthode : %s\n", s.builder.Method())
}

func (s *Shell) cmdHeader(rest string) {
	words := shellWords(rest)
	if len(words) > 0 && (strings.EqualFold(words[0], "set") || strings.EqualFold(words[0], "delete") || strings.EqualFold(words[0], "show")) {
		s.cmdTargetHeader(words)
		return
	}
	switch {
	case rest == "":
		headers := s.builder.Headers()
		if len(headers) == 0 {
			s.printf("[CRL] Aucun en-tête configuré.\n")
			return
		}
		s.printf("[CRL] En-têtes (%d) :\n", len(headers))
		for _, h := range headers {
			s.printf("%s\n", s.headerLine(h.Name, h.Value, "  "))
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

func (s *Shell) startJSONEditor() {
	if !s.interactive || s.editor == nil {
		body := s.builder.Body()
		if !s.builder.BodySet() {
			body = []byte("{}\n")
		}
		for {
			edited, err := editMultiline("crl-json-*.json", body)
			if err != nil {
				s.errorf("édition JSON : %v ; corps précédent conservé", err)
				return
			}
			if err := validateJSON(edited); err != nil {
				s.errorf("%v ; le document édité est conservé pour correction", err)
				body = edited
				continue
			}
			if err := s.builder.SetJSON(string(edited)); err != nil {
				s.errorf("%v", err)
				return
			}
			s.printf("[CRL] Corps JSON configuré (%d octets).\n", s.builder.BodySize())
			return
		}
	}
	initial := s.builder.Body()
	if !s.builder.BodySet() || len(initial) == 0 {
		initial = []byte("{}\n")
	}
	s.openTextArea("json", s.builder, 0, initial)
}

func (s *Shell) startBodyEditor() {
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
	initial := s.builder.Body()
	s.openTextArea("body", s.builder, 0, initial)
	return
}

func (s *Shell) openTextArea(kind string, b *request.Builder, targetID int, initial []byte) {
	if !s.interactive || s.editor == nil {
		pattern := "crl-body-*"
		if kind == "json" {
			pattern = "crl-json-*.json"
		}
		content := append([]byte(nil), initial...)
		for {
			edited, err := editMultiline(pattern, content)
			if err != nil {
				s.errorf("édition : %v ; corps précédent conservé", err)
				return
			}
			if kind == "json" {
				if err := validateJSON(edited); err != nil {
					s.errorf("%v ; le document édité est conservé pour correction", err)
					content = edited
					continue
				}
				if err := b.SetJSON(string(edited)); err != nil {
					s.errorf("%v", err)
					return
				}
			} else {
				b.SetBody(edited)
			}
			if targetID > 0 {
				s.requestBodyReplaced[targetID] = true
			}
			s.printf("[CRL] Corps %s appliqué (%d octets).\n", kind, len(edited))
			return
		}
	}
	target := "constructeur actif"
	title := "CRL JSON EDITOR"
	if kind == "body" {
		title = "CRL BODY EDITOR"
	}
	if kind == "json" && len(initial) == 0 {
		initial = []byte("{}\n")
	}
	if targetID > 0 {
		target = fmt.Sprintf("requête #%d", targetID)
	}
	s.textKind, s.textBuilder, s.textTarget = kind, b, targetID
	s.textArea = newTextArea(s.out, title, target, initial)
	s.resizeTextArea()
	s.out.reset()
	s.textArea.Draw(s.pal, unicodeTerminal())
}

// resizeTextArea consulte à nouveau le terminal avant chaque rafraîchissement.
// Cela couvre les redimensionnements sans installer un second gestionnaire de
// signaux ni conserver une taille devenue obsolète.
func (s *Shell) resizeTextArea() {
	if s.textArea == nil {
		return
	}
	if f, ok := s.in.(*os.File); ok {
		s.textArea.SetSize(terminalWidth(f.Fd()), terminalHeight(f.Fd()))
	}
}

func (s *Shell) commitTextArea() {
	content := []byte(s.textArea.Text())
	if s.textKind == "json" {
		if err := validateJSON(content); err != nil {
			s.textArea.message = err.Error()
			s.resizeTextArea()
			s.textArea.Draw(s.pal, unicodeTerminal())
			return
		}
		if err := s.textBuilder.SetJSON(string(content)); err != nil {
			s.textArea.message = err.Error()
			s.resizeTextArea()
			s.textArea.Draw(s.pal, unicodeTerminal())
			return
		}
	} else {
		s.textBuilder.SetBody(content)
	}
	if s.textTarget > 0 {
		s.requestBodyReplaced[s.textTarget] = true
		s.textResult = fmt.Sprintf("[CRL] Corps de la requête #%d modifié (%d octets).", s.textTarget, len(content))
	} else {
		s.textResult = fmt.Sprintf("[CRL] Corps %s appliqué (%d octets).", map[string]string{"json": "JSON", "body": "brut"}[s.textKind], len(content))
	}
	s.closeTextArea(true)
}

func (s *Shell) closeTextArea(applied bool) {
	if s.textArea == nil {
		return
	}
	// Efface le panneau depuis son origine réelle, sans changer le mode du
	// terminal : le shell conserve la main sur le mode brut et son lecteur.
	s.textArea.Clear()
	s.out.reset()
	s.textArea = nil
	s.textBuilder = nil
	s.textKind = ""
	s.textTarget = 0
	if applied && s.textResult != "" {
		s.printf("%s\n", s.textResult)
		s.textResult = ""
	} else if !applied {
		s.printf("[CRL] Édition annulée ; corps précédent conservé.\n")
	}
	if s.editor != nil {
		s.editor.Reset()
		s.editor.SetPrompt(s.promptText())
		for _, line := range s.textNotices {
			s.editor.PrintLines([]string{line})
		}
		s.textNotices = nil
		// Le panneau a été effacé hors du bloc de commande : on rétablit ici
		// la même ligne vide avant le prompt que pour les autres commandes.
		s.printf("\n")
		s.editor.DrawPrompt()
	}
}

func validateJSON(data []byte) error {
	var value json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			prefix := data[:maxInt(0, minInt(int(syntax.Offset)-1, len(data)))]
			line := strings.Count(string(prefix), "\n") + 1
			lastNL := strings.LastIndexByte(string(prefix), '\n')
			column := len([]rune(string(prefix[lastNL+1:]))) + 1
			return fmt.Errorf("JSON invalide, ligne %d colonne %d : %s", line, column, syntax.Error())
		}
		return fmt.Errorf("JSON invalide : %w", err)
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Shell) cmdJSONSet(rest string) {
	trim := strings.TrimSpace(rest)
	if !strings.HasPrefix(trim, "set ") {
		s.errorf("usage : crl.json set <chemin> = <valeur JSON>")
		return
	}
	if err := updateJSONBuilder(s.builder, "set", strings.TrimSpace(trim[4:])); err != nil {
		s.errorf("%v", err)
		return
	}
	s.printf("[CRL] Champ JSON mis à jour (%d octets).\n", s.builder.BodySize())
}

func (s *Shell) cmdTargetJSON(rest string) {
	words := shellWords(rest)
	if len(words) < 2 {
		s.errorf("usage : crl.json <cible> set|delete|clear|edit …")
		return
	}
	captures, err := s.resolveTargets(words[0])
	if err != nil {
		s.errorf("%v", err)
		return
	}
	operation := strings.TrimSpace(rest[len(words[0]):])
	switch strings.ToLower(words[1]) {
	case "edit":
		if len(words) != 2 {
			s.errorf("usage : crl.json <cible> edit")
			return
		}
		if len(captures) > 1 {
			s.errorf("l'édition JSON ciblée est individuelle ; choisissez un ID précis")
			return
		}
		b, e := s.requestBuilderFor(captures[0])
		if e != nil {
			s.errorf("%v", e)
			return
		}
		s.openTextArea("json", b, captures[0].ID, b.Body())
	case "clear":
		s.applyCaptureChanges(captures, func(b *request.Builder) error { b.ClearBody(); return nil })
		for _, c := range captures {
			s.requestBodyReplaced[c.ID] = true
		}
	case "set", "delete":
		pathExpr := strings.TrimSpace(operation[len(words[1]):])
		s.applyCaptureChanges(captures, func(b *request.Builder) error { return updateJSONBuilder(b, strings.ToLower(words[1]), pathExpr) })
	default:
		s.errorf("opération JSON inconnue : %s", words[1])
	}
}

func (s *Shell) cmdTargetBody(rest string) {
	words := shellWords(rest)
	if len(words) < 2 {
		s.errorf("usage : crl.body <cible> edit|clear|set [texte]")
		return
	}
	captures, err := s.resolveTargets(words[0])
	if err != nil {
		s.errorf("%v", err)
		return
	}
	switch strings.ToLower(words[1]) {
	case "clear":
		s.applyCaptureChanges(captures, func(b *request.Builder) error { b.ClearBody(); return nil })
		for _, c := range captures {
			s.requestBodyReplaced[c.ID] = true
		}
	case "set":
		if len(words) < 3 {
			s.errorf("texte du corps manquant")
			return
		}
		text := strings.Join(words[2:], " ")
		s.applyCaptureChanges(captures, func(b *request.Builder) error { b.SetBodyText(text); return nil })
		for _, c := range captures {
			s.requestBodyReplaced[c.ID] = true
		}
	case "edit":
		if len(captures) != 1 {
			s.errorf("l'édition interactive du corps est individuelle ; choisissez un ID précis")
			return
		}
		b, e := s.requestBuilderFor(captures[0])
		if e != nil {
			s.errorf("%v", e)
			return
		}
		s.openTextArea("body", b, captures[0].ID, b.Body())
	default:
		s.errorf("opération de corps inconnue : %s", words[1])
	}
}

func updateJSONBuilder(b *request.Builder, operation, expression string) error {
	body := b.Body()
	if !b.BodySet() || len(body) == 0 {
		body = []byte("{}")
	}
	var root any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return fmt.Errorf("JSON existant invalide: %w", err)
	}
	path := ""
	var value any
	if operation == "set" {
		eq := strings.Index(expression, "=")
		if eq < 0 {
			return fmt.Errorf("usage : set <chemin> = <valeur JSON")
		}
		path = strings.TrimSpace(expression[:eq])
		raw := strings.TrimSpace(expression[eq+1:])
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return fmt.Errorf("valeur JSON invalide: %w", err)
		}
	} else {
		path = strings.TrimSpace(expression)
	}
	if path == "" {
		return fmt.Errorf("chemin JSON vide")
	}
	mutated, err := mutateJSONPath(root, strings.Split(path, "."), operation, value)
	if err != nil {
		return err
	}
	root = mutated
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return b.SetJSON(string(encoded))
}

func mutateJSONPath(current any, parts []string, operation string, value any) (any, error) {
	if len(parts) == 0 || parts[0] == "" {
		return current, fmt.Errorf("chemin JSON invalide")
	}
	if len(parts) > 1 {
		switch node := current.(type) {
		case map[string]any:
			child, ok := node[parts[0]]
			if !ok && operation == "set" {
				child = map[string]any{}
			}
			if !ok && operation == "delete" {
				return current, fmt.Errorf("chemin %q introuvable", parts[0])
			}
			updated, err := mutateJSONPath(child, parts[1:], operation, value)
			if err != nil {
				return current, err
			}
			node[parts[0]] = updated
			return node, nil
		case []any:
			index, err := strconv.Atoi(parts[0])
			if err != nil || index < 0 || index >= len(node) {
				return current, fmt.Errorf("index de tableau %q invalide", parts[0])
			}
			updated, err := mutateJSONPath(node[index], parts[1:], operation, value)
			if err != nil {
				return current, err
			}
			node[index] = updated
			return node, nil
		default:
			return current, fmt.Errorf("chemin %q introuvable ou scalaire", parts[0])
		}
	}
	switch node := current.(type) {
	case map[string]any:
		if operation == "delete" {
			if _, ok := node[parts[0]]; !ok {
				return current, fmt.Errorf("propriété %q absente", parts[0])
			}
			delete(node, parts[0])
		} else {
			node[parts[0]] = value
		}
		return node, nil
	case []any:
		index, err := strconv.Atoi(parts[0])
		if err != nil || index < 0 || index > len(node) {
			return current, fmt.Errorf("index de tableau %q invalide", parts[0])
		}
		if operation == "delete" {
			if index >= len(node) {
				return current, fmt.Errorf("index de tableau %q absent", parts[0])
			}
			return append(node[:index], node[index+1:]...), nil
		}
		if index == len(node) {
			return append(node, value), nil
		}
		node[index] = value
		return node, nil
	default:
		return current, fmt.Errorf("la racine JSON doit être un objet ou un tableau")
	}
}

func (s *Shell) cmdJSONDelete(rest string) {
	trim := strings.TrimSpace(rest)
	if !strings.HasPrefix(trim, "delete ") {
		s.errorf("usage : crl.json delete <chemin>")
		return
	}
	if err := updateJSONBuilder(s.builder, "delete", strings.TrimSpace(trim[7:])); err != nil {
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

func (s *Shell) showJSONState() {
	body := s.builder.Body()
	if !s.builder.BodySet() || len(body) == 0 {
		s.printf("[CRL] Aucun JSON configuré. usage : crl.json <JSON>\n")
		return
	}
	s.printf("[CRL] JSON actuel (%d octets) :\n", len(body))
	s.printIndented(string(body), "  ")
}

func (s *Shell) showBodyState() {
	body := s.builder.Body()
	if !s.builder.BodySet() || len(body) == 0 {
		s.printf("[CRL] Aucun corps configuré. usage : crl.body <texte>\n")
		return
	}
	s.printf("[CRL] Corps actuel (%d octets) :\n", len(body))
	s.printIndented(string(body), "  ")
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

func (s *Shell) cmdShow(rest string) {
	if s.history != nil {
		selected := s.history.SelectedMany()
		if strings.TrimSpace(rest) != "" {
			words := shellWords(rest)
			if len(words) == 0 {
				s.errorf("usage : crl.show [id [body [offset]]]")
				return
			}
			if strings.EqualFold(words[0], "body") {
				offset := 0
				var err error
				if len(words) > 2 {
					s.errorf("usage : crl.show body [offset]")
					return
				}
				if len(words) == 2 {
					offset, err = strconv.Atoi(words[1])
					if err != nil || offset < 0 {
						s.errorf("offset invalide")
						return
					}
				}
				s.showActiveBody(offset)
				return
			}
			if strings.EqualFold(words[0], "active") {
				if len(words) != 1 {
					s.errorf("usage : crl.show active")
					return
				}
				s.showActiveRequest()
				return
			}
			id, err := parseID(words[0])
			if err != nil {
				s.errorf("%v", err)
				return
			}
			c, ok := s.history.Get(id)
			if !ok {
				s.errorf("requête #%d introuvable", id)
				return
			}
			offset := 0
			if len(words) > 1 {
				if words[1] != "body" {
					s.errorf("usage : crl.show <id> [body [offset]]")
					return
				}
				if len(words) > 2 {
					offset, err = strconv.Atoi(words[2])
					if err != nil || offset < 0 {
						s.errorf("offset invalide")
						return
					}
				}
			}
			s.showCapture(c, offset)
			return
		}
		if len(selected) > 0 {
			p := s.pal
			s.printf("[CRL] Requêtes sélectionnées (%d) — détails\n", len(selected))
			for _, c := range selected {
				s.showCapture(c, 0)
			}
			s.printf("\n%s\n", p.Dim("crl.show <id> pour une requête · crl.launch / mint.replay pour envoyer"))
			return
		}
	}
	if s.builder.IsEmpty() {
		s.printf("[CRL] Aucune requête préparée. Utilisez crl.host, crl.method, crl.head, crl.json ou mint.use.\n")
		return
	}
	p := s.pal
	res := s.proxyResolution()
	s.printf("%s\n", p.BoldGreen("[CRL] Requête préparée"))
	s.printf("%s\n", p.Dim("Aucune requête envoyée — utilisez crl.launch pour l'exécuter."))
	s.rule(s.termWidth())
	s.field("Méthode", p.Method(s.builder.Method()))
	s.field("URL", p.Cyan(display.MaskURL(sanitizeTerminal(s.builder.URL()))))
	if s.activeMintID > 0 {
		s.field("Source", fmt.Sprintf("Mint · capture #%d (copie active)", s.activeMintID))
	}
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
			s.printf("%s\n", s.headerLine(h.Name, h.Value, "    "))
		}
	}
	if s.builder.BodySet() {
		s.printf("\n  %s\n", p.BoldCyan(fmt.Sprintf("Corps (%d octets)", s.builder.BodySize())))
		rendered, truncated := display.RenderBody(display.MaskBody(s.builder.Body()), 4096)
		s.printIndented(rendered, "    ")
		if truncated {
			s.printf("    […] Corps limité à 4 Kio.\n")
		}
	} else {
		s.printf("\n  %s\n", p.BoldCyan("Corps (vide / non défini)"))
	}
	s.printf("\n")
	s.rule(s.termWidth())
	s.printf("%s\n", p.Dim("crl.launch pour envoyer · crl.table pour un résumé"))
}

func (s *Shell) showActiveRequest() {
	if s.builder.IsEmpty() {
		s.printf("[CRL] Aucune requête active préparée.\n")
		return
	}
	history := s.history
	s.history = nil
	s.cmdShow("")
	s.history = history
}

func (s *Shell) showActiveBody(offset int) {
	body := display.MaskBody(s.builder.Body())
	if !s.builder.BodySet() {
		s.printf("[CRL] Aucun corps configuré.\n")
		return
	}
	if offset < 0 || offset >= len(body) {
		s.errorf("offset hors limites (%d octets affichés)", len(body))
		return
	}
	end := offset + launchBodyLimit
	if end > len(body) {
		end = len(body)
	}
	for offset > 0 && offset < len(body) && !utf8.RuneStart(body[offset]) {
		offset++
	}
	for end > offset && end < len(body) && !utf8.RuneStart(body[end]) {
		end--
	}
	rendered, _ := display.RenderBody(body[offset:end], 0)
	s.printf("[CRL] Corps actif — octets %d-%d/%d\n", offset, end, len(body))
	s.printIndented(rendered, "  ")
	if end < len(body) {
		s.printf("Suite : crl.show body %d\n", end)
	}
}

func (s *Shell) showCapture(c mint.Capture, offset int) {
	p := s.pal
	b, err := s.requestBuilderFor(c)
	if err != nil {
		s.errorf("requête #%d : %v", c.ID, err)
		return
	}

	width := s.termWidth()
	title := fmt.Sprintf(" REQUEST #%d ", c.ID)
	top := "╭─" + title + strings.Repeat("─", max(0, width-display.VisibleWidth("╭─"+title)))
	bottom := "╰" + strings.Repeat("─", max(0, width-1))

	s.printf("\n%s\n", top)
	s.printf("%s\n", s.headerLine("Méthode", p.Method(b.Method()), "│ "))
	s.printf("%s\n", s.headerLine("URL", display.MaskURL(sanitizeTerminal(b.URL())), "│ "))
	s.printf("%s\n", s.headerLine("Source", fmt.Sprintf("Mint · capture #%d", c.ID), "│ "))
	s.printf("%s\n", s.headerLine("Capturée", c.StartedAt.Local().Format("2006-01-02 15:04:05.000"), "│ "))

	u, _ := url.Parse(b.URL())
	if u != nil {
		pathStr := u.EscapedPath()
		if pathStr == "" {
			pathStr = "/"
		}
		if u.RawQuery != "" {
			pathStr += "?" + u.RawQuery
		}
		s.printf("%s\n", s.headerLine("Chemin", display.MaskURL(sanitizeTerminal(pathStr)), "│ "))
	}

	headers := b.Headers()
	ctype := ""
	s.printf("│\n")
	s.printf("│ EN-TÊTES (%d)\n", len(headers))
	if len(headers) == 0 {
		s.printf("│   (aucun)\n")
	}
	for _, h := range headers {
		s.printf("%s\n", s.headerLine(h.Name, h.Value, "│   "))
		if strings.EqualFold(h.Name, "Content-Type") {
			ctype = h.Value
		}
	}
	if ctype != "" {
		s.printf("%s\n", s.headerLine("Type de contenu", sanitizeTerminal(ctype), "│ "))
	}

	body := b.Body()
	displayBody := display.MaskBody(body)
	s.printf("│\n")
	s.printf("│ CORPS (%s)\n", display.FormatBytes(int64(len(body))))
	if c.Request.BodyTruncated {
		s.printf("│   [capture tronquée : %d octets conservés sur %d]\n", len(body), c.Request.BodySize)
	}
	if len(body) == 0 {
		s.printf("│   Aucun corps\n")
	} else if offset > 0 || len(body) > launchBodyLimit {
		if offset >= len(displayBody) {
			s.printf("│   Offset hors limites (%d octets affichables)\n", len(displayBody))
		} else {
			end := offset + launchBodyLimit
			if end > len(displayBody) {
				end = len(displayBody)
			}
			page := displayBody[offset:end]
			rendered, _ := display.RenderBody(page, 0)
			bodyWidth := max(10, width-4)
			for _, line := range strings.Split(rendered, "\n") {
				for _, wline := range display.WrapText(line, bodyWidth) {
					s.printf("│   %s\n", wline)
				}
			}
			if end < len(displayBody) {
				s.printf("│   […] octets affichés %d-%d sur %d · suite : crl.show %d body %d\n", offset, end, len(displayBody), c.ID, end)
			}
			if offset > 0 {
				s.printf("│   [page brute depuis l'octet %d]\n", offset)
			}
		}
	} else {
		rendered, _ := display.RenderBody(displayBody, 0)
		bodyWidth := max(10, width-4)
		for _, line := range strings.Split(rendered, "\n") {
			for _, wline := range display.WrapText(line, bodyWidth) {
				s.printf("│   %s\n", wline)
			}
		}
	}
	s.printf("%s\n", bottom)
}

func (s *Shell) cmdTable() {
	if s.history != nil {
		selected := s.history.SelectedMany()
		if len(selected) > 0 {
			p := s.pal
			if len(selected) == 1 {
				c := selected[0]
				s.printf("%s\n", p.BoldGreen(fmt.Sprintf("[CRL] Tableau de la requête sélectionnée #%d", c.ID)))
				s.field("MÉTHODE", p.Method(c.Request.Method))
				s.field("URL", p.Cyan(display.MaskURL(c.Request.URL)))
				s.field("EN-TÊTES", p.Bold(fmt.Sprintf("%d", len(c.Request.Headers))))
				s.field("CORPS", p.Bold(fmt.Sprintf("%d octets", len(c.Request.Body))))
				if len(c.Request.Headers) > 0 {
					names := make([]string, 0, len(c.Request.Headers))
					for _, h := range c.Request.Headers {
						names = append(names, sanitizeTerminal(h.Name))
					}
					s.printf("\n  %s\n", p.BoldCyan(fmt.Sprintf("En-têtes (%d)", len(c.Request.Headers))))
					namesStr := strings.Join(names, ", ")
					for _, line := range display.WrapText(namesStr, s.termWidth()-4) {
						s.printf("  %s\n", p.Dim(line))
					}
					s.printf("  %s\n", p.Dim("crl.show affiche le détail complet des valeurs."))
				}
				return
			}
			s.printf("%s\n", p.BoldGreen(fmt.Sprintf("[CRL] Tableau des requêtes sélectionnées (%d)", len(selected))))
			s.rule(s.termWidth())
			for _, c := range selected {
				urlStr := display.MaskURL(c.Request.URL)
				urlLines := display.WrapText(urlStr, max(20, s.termWidth()-10))
				s.printf("  #%d %s %s\n", c.ID, p.Method(c.Request.Method), p.Cyan(urlLines[0]))
				for _, cont := range urlLines[1:] {
					s.printf("          %s\n", p.Cyan(cont))
				}
				s.printf("     %d en-tête(s), %d octet(s)\n", len(c.Request.Headers), len(c.Request.Body))
			}
			s.rule(s.termWidth())
			s.printf("%s\n", p.Dim("crl.show pour le détail · crl.launch/mint.replay pour exécuter"))
			return
		}
	}
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
		namesStr := strings.Join(names, ", ")
		for _, line := range display.WrapText(namesStr, s.termWidth()-4) {
			s.printf("  %s\n", p.Dim(line))
		}
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
	if rest != "" && !strings.EqualFold(strings.TrimSpace(rest), "active") {
		s.cmdMintReplay(rest)
		return
	}
	if strings.TrimSpace(rest) == "" && s.history != nil && len(s.history.SelectedMany()) > 0 {
		s.cmdMintReplay("")
		return
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
	s.rule(s.termWidth())
	s.field("Méthode", p.Method(b.Method()))
	s.field("URL", p.Cyan(display.MaskURL(sanitizeTerminal(b.URL()))))
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
	s.printf("\n[Mint] Capture récente de même méthode et URL : #%d (%s %s). La corrélation n'est pas garantie en cas de trafic concurrent ; comparez les en-têtes avec crl.show, détail : mint.request.show %d.\n", c.ID, sanitizeTerminal(c.Request.Method), display.MaskURL(sanitizeTerminal(c.Request.URL)), c.ID)
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
	s.rule(s.termWidth())
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
	s.rule(s.termWidth())
	s.field("Statut", p.StatusCode(status.Code, sanitizeTerminal(status.Label())))
	s.field("Méthode", p.Method(b.Method()))
	s.field("URL demandée", p.Cyan(display.MaskURL(sanitizeTerminal(b.URL()))))
	s.field("Durée", p.Cyan(durationLabel(meta.Duration)))
	s.field("Taille", p.Bold(display.FormatBytes(total)))
	if contentType := headers.ContentType(); contentType != "" {
		s.field("Type", p.Cyan(contentType))
	}
	if status.Proto != "" {
		s.field("HTTP", p.Dim(sanitizeTerminal(status.Proto)))
	}
	if meta.EffectiveURL != "" {
		s.field("URL finale", p.Cyan(display.MaskURL(sanitizeTerminal(meta.EffectiveURL))))
	}

	s.printf("\n  %s\n", p.BoldBlue(fmt.Sprintf("En-têtes de réponse (%d)", len(headers))))
	if len(headers) == 0 {
		s.printf("    %s\n", p.Dim("(aucun)"))
	} else {
		for _, h := range headers {
			s.printf("%s\n", s.headerLine(h.Name, h.Value, "    "))
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
	s.rule(s.termWidth())
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
	width := s.termWidth()

	// Largeurs de colonnes adaptatives : ID, MÉTHODE, STATUT et DURÉE ont des
	// largeurs fixes minimales ; l'espace restant est partagé entre HÔTE et CHEMIN.
	idWidth := 4
	methodWidth := 7
	statusWidth := 8
	durWidth := 9
	sepWidth := 10 // 5 séparateurs de 2 caractères

	minTotal := idWidth + methodWidth + statusWidth + durWidth + sepWidth
	if width < minTotal {
		width = minTotal
	}
	remaining := width - minTotal
	hostWidth := remaining / 2
	if hostWidth > 26 {
		hostWidth = 26
	}
	pathWidth := remaining - hostWidth
	if pathWidth < 10 {
		pathWidth = 10
		hostWidth = remaining - pathWidth
		if hostWidth < 5 {
			hostWidth = 5
		}
	}

	header := fmt.Sprintf("%s  %s  %s  %s  %s  %s",
		padCell("ID", idWidth),
		padCell("MÉTHODE", methodWidth),
		padCell("HÔTE", hostWidth),
		padCell("CHEMIN", pathWidth),
		padCell("STATUT", statusWidth),
		padCell("DURÉE", durWidth))
	s.printf("%s\n", p.Bold(header))
	s.rule(width)
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
	s.rule(s.termWidth())
	if c.FlowID != "" {
		s.field("Flux", p.Dim(c.FlowID))
	}
	s.field("Date", c.StartedAt.Local().Format("2006-01-02 15:04:05.000"))
	s.field("URL", p.Cyan(display.MaskURL(sanitizeTerminal(c.Request.URL))))
	s.field("Hôte", c.Request.Host)
	s.field("Chemin", p.Cyan(display.MaskURL(sanitizeTerminal(c.PathAndQuery()))))
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
				s.printf("%s\n", s.headerLine(h.Name, h.Value, "      "))
			}
		}
		s.printf("    %s\n", p.BoldCyan(fmt.Sprintf("Corps (%d octets%s)", len(c.Request.Body), truncatedSuffix(c.Request.BodyTruncated, c.Request.BodySize))))
		rendered, _ := display.RenderBody(display.MaskBody(c.Request.Body), 8192)
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
				s.printf("%s\n", s.headerLine(h.Name, h.Value, "      "))
			}
		}
		s.printf("    %s\n", p.BoldCyan(fmt.Sprintf("Corps (%d octets%s)", len(c.Response.Body), truncatedSuffix(c.Response.BodyTruncated, c.Response.BodySize))))
		rendered, _ := display.RenderBody(display.MaskBody(c.Response.Body), 8192)
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
	words := shellWords(rest)
	if len(words) == 1 && strings.EqualFold(words[0], "clear") {
		s.history.ClearSelection()
		s.printf("[Mint] Sélection effacée.\n")
		return
	}
	mode := "replace"
	if len(words) > 1 && (strings.EqualFold(words[0], "add") || strings.EqualFold(words[0], "remove")) {
		mode = strings.ToLower(words[0])
		rest = strings.TrimSpace(rest[len(words[0]):])
	}
	if strings.EqualFold(strings.TrimSpace(rest), "all") {
		ids := make([]int, 0)
		for _, c := range s.history.List() {
			ids = append(ids, c.ID)
		}
		if len(ids) == 0 {
			s.errorf("aucune requête Mint à sélectionner")
			return
		}
		var captures []mint.Capture
		var err error
		if mode == "add" {
			captures, err = s.history.AddSelection(ids)
		} else if mode == "remove" {
			captures = s.history.RemoveSelection(ids)
		} else {
			captures, err = s.history.SelectMany(ids)
		}
		if err != nil {
			s.errorf("%v", err)
			return
		}
		s.printSelected(captures)
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
	var captures []mint.Capture
	var err error
	switch mode {
	case "add":
		captures, err = s.history.AddSelection(ids)
	case "remove":
		captures = s.history.RemoveSelection(ids)
	default:
		captures, err = s.history.SelectMany(ids)
	}
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
		urlStr := display.MaskURL(c.PathAndQuery())
		urlLines := display.WrapText(urlStr, max(20, s.termWidth()-20))
		s.printf("  #%d %s %s%s\n", c.ID, p.Method(c.Request.Method), c.Request.Host, p.Cyan(urlLines[0]))
		for _, cont := range urlLines[1:] {
			s.printf("          %s\n", p.Cyan(cont))
		}
	}
}

func (s *Shell) requestBuilderFor(c mint.Capture) (*request.Builder, error) {
	if b := s.requestEdits[c.ID]; b != nil {
		return b, nil
	}
	b := request.New()
	headers := make([]request.Header, 0, len(c.Request.Headers))
	for _, h := range c.Request.Headers {
		headers = append(headers, request.Header{Name: h.Name, Value: h.Value})
	}
	if err := b.Import(request.CapturedRequest{URL: c.Request.URL, Method: c.Request.Method, Headers: headers, Body: c.Request.Body, BodyPresent: len(c.Request.Body) > 0}); err != nil {
		return nil, err
	}
	s.requestEdits[c.ID] = b
	return b, nil
}

func (s *Shell) captureWithEdits(c mint.Capture) (mint.Capture, error) {
	b, err := s.requestBuilderFor(c)
	if err != nil {
		return c, err
	}
	c.Request.Method = b.Method()
	c.Request.URL = b.URL()
	c.Request.Headers = nil
	for _, h := range b.Headers() {
		c.Request.Headers = append(c.Request.Headers, mint.Header{Name: h.Name, Value: h.Value})
	}
	c.Request.Body = b.Body()
	if s.requestBodyReplaced[c.ID] {
		c.Request.BodyTruncated = false
	}
	if !c.Request.BodyTruncated {
		c.Request.BodySize = int64(len(c.Request.Body))
	}
	u, err := url.Parse(c.Request.URL)
	if err == nil {
		c.Request.Scheme = u.Scheme
		c.Request.Host = u.Host
		c.Request.Path = u.EscapedPath()
		if c.Request.Path == "" {
			c.Request.Path = "/"
		}
		c.Request.RawQuery = u.RawQuery
	}
	return c, nil
}

func (s *Shell) resolveTargets(spec string) ([]mint.Capture, error) {
	spec = strings.TrimSpace(spec)
	if strings.EqualFold(spec, "all") {
		return s.history.List(), nil
	}
	if strings.EqualFold(spec, "selected") {
		out := s.history.SelectedMany()
		if len(out) == 0 {
			return nil, fmt.Errorf("aucune requête sélectionnée")
		}
		return out, nil
	}
	var ids []int
	for _, part := range strings.Fields(strings.ReplaceAll(spec, ",", " ")) {
		id, err := parseID(part)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("cible requise : ID, liste d'ID, selected ou all")
	}
	out := make([]mint.Capture, 0, len(ids))
	seen := map[int]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		c, ok := s.history.Get(id)
		if !ok {
			return nil, fmt.Errorf("requête #%d introuvable", id)
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Shell) applyCaptureChanges(captures []mint.Capture, change func(*request.Builder) error) {
	ok, failed := 0, 0
	for _, c := range captures {
		b, err := s.requestBuilderFor(c)
		if err == nil {
			err = change(b)
		}
		if err != nil {
			failed++
			s.printf("[ERREUR] Requête #%d : %s\n", c.ID, sanitizeTerminal(err.Error()))
		} else {
			ok++
		}
	}
	s.printf("[CRL] Modifications : %d requête(s) réussie(s), %d échec(s).\n", ok, failed)
}

func (s *Shell) cmdTargetHeader(words []string) {
	if len(words) < 2 {
		s.errorf("usage : crl.head set <cible> <nom> <valeur> | delete <cible> <nom> | show <cible>")
		return
	}
	action := strings.ToLower(words[0])
	targets := words[1]
	if action == "show" {
		if len(words) != 2 {
			s.errorf("usage : crl.head show <cible>")
			return
		}
		captures, err := s.resolveTargets(targets)
		if err != nil {
			s.errorf("%v", err)
			return
		}
		for _, c := range captures {
			b, e := s.requestBuilderFor(c)
			if e != nil {
				s.errorf("requête #%d : %v", c.ID, e)
				continue
			}
			s.printf("[CRL] En-têtes de #%d (%d) :\n", c.ID, len(b.Headers()))
			for _, h := range b.Headers() {
				s.printf("%s\n", s.headerLine(h.Name, h.Value, "  "))
			}
		}
		return
	}
	if len(words) < 3 {
		s.errorf("usage : crl.head set <cible> <nom> <valeur> | delete <cible> <nom>")
		return
	}
	name := words[2]
	captures, err := s.resolveTargets(targets)
	if err != nil {
		s.errorf("%v", err)
		return
	}
	if action == "set" {
		if len(words) < 4 {
			s.errorf("valeur d'en-tête manquante")
			return
		}
		value := strings.Join(words[3:], " ")
		s.applyCaptureChanges(captures, func(b *request.Builder) error { return b.SetHeader(name, value) })
		return
	}
	if action == "delete" {
		s.applyCaptureChanges(captures, func(b *request.Builder) error {
			if b.RemoveHeader(name) == 0 {
				return fmt.Errorf("en-tête %q absent", name)
			}
			return nil
		})
		return
	}
	s.errorf("commande d'en-tête inconnue : %s", action)
}

// shellWords handles quoted command arguments without invoking a shell.
func shellWords(input string) []string {
	var out []string
	var b strings.Builder
	quote := rune(0)
	escaped := false
	active := false
	for _, r := range input {
		if escaped {
			b.WriteRune(r)
			escaped = false
			active = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			active = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			active = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			active = true
			continue
		}
		if r == ' ' || r == '\t' {
			if active {
				out = append(out, b.String())
				b.Reset()
				active = false
			}
			continue
		}
		b.WriteRune(r)
		active = true
	}
	if escaped {
		b.WriteRune('\\')
	}
	if active {
		out = append(out, b.String())
	}
	return out
}

type replayResult struct {
	id       int
	method   string
	url      string
	status   response.Status
	duration string
	headers  response.Headers
	body     []byte
	total    int64
	bodyPath string
	err      error
	network  bool
}

// cmdMintReplay démarre chaque capture sélectionnée en parallèle, conserve son
// identifiant et ne rend les résultats qu'après la fin du lot. Le rendu est
// volontairement commun à toutes les réponses, comme pour crl.launch.
func (s *Shell) cmdMintReplay(rest string) {
	var captures []mint.Capture
	var err error
	if strings.TrimSpace(rest) != "" {
		captures, err = s.resolveTargets(rest)
		if err != nil {
			s.errorf("%v", err)
			return
		}
	} else {
		captures = s.history.SelectedMany()
	}
	if len(captures) == 0 {
		s.errorf("aucune capture sélectionnée : utilisez mint.select <id> [id...]")
		return
	}
	s.clearReplayArtifacts()
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	results := make([]replayResult, len(captures))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, capture := range captures {
		prepared, prepErr := s.captureWithEdits(capture)
		if prepErr == nil {
			if ok, why := prepared.Importable(); !ok {
				prepErr = fmt.Errorf("%s", why)
			}
		}
		wg.Add(1)
		go func(i int, c mint.Capture, prepErr error) {
			defer wg.Done()
			r := replayResult{id: c.ID, method: c.Request.Method, url: c.Request.URL}
			if prepErr != nil {
				r.err = prepErr
				results[i] = r
				return
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				r.err = ctx.Err()
				r.network = true
				results[i] = r
				return
			}
			defer func() { <-sem }()
			results[i] = s.replayCapture(ctx, c)
		}(i, prepared, prepErr)
	}
	wg.Wait()

	completed, successHTTP, httpErrors, networkErrors, executionErrors := 0, 0, 0, 0, 0
	for _, r := range results {
		if r.err != nil {
			if r.network {
				networkErrors++
			} else {
				executionErrors++
			}
			continue
		}
		completed++
		if r.status.Code >= 400 {
			httpErrors++
		} else if r.status.Code >= 200 && r.status.Code < 400 {
			successHTTP++
		}
	}
	for _, r := range results {
		if r.bodyPath != "" {
			s.replayBodies[r.id] = r.bodyPath
			s.replayDirs = append(s.replayDirs, filepath.Dir(r.bodyPath))
		}
	}
	s.lastBatchSent = len(results)
	s.lastBatchSuccess = completed
	s.lastBatchFail = networkErrors + executionErrors
	s.lastBatchSet = true
	s.printReplaySummary(results, completed, successHTTP, httpErrors, networkErrors, executionErrors)
}

func (s *Shell) printReplaySummary(results []replayResult, completed, successHTTP, httpErrors, networkErrors, executionErrors int) {
	p := s.pal
	title := "Résultats du rejeu parallèle"
	if len(results) == 1 {
		title = "Résultat du rejeu"
	}
	s.printf("%s %s — %d requête(s)\n", p.BoldMagenta("[Mint]"), p.Bold(title), len(results))
	s.printf("\n%s\n", p.BoldBlue("Résumé"))
	s.printf("  Total : %d\n", len(results))
	s.printf("  Terminées : %d\n", completed)
	s.printf("  Succès HTTP : %d\n", successHTTP)
	s.printf("  Erreurs HTTP : %d\n", httpErrors)
	s.printf("  Erreurs réseau : %d\n", networkErrors)
	if executionErrors > 0 {
		s.printf("  Erreurs requête : %d\n", executionErrors)
	}

	s.printf("\n%s\n", p.BoldBlue("Résultats"))
	header := fmt.Sprintf("%-5s %s %s %s %s", "ID", padCell("MÉTHODE", 7), padCell("STATUT", 7), padCell("DURÉE", 10), "URL / ERREUR")
	s.printf("  %s\n", p.Dim(header))
	for _, r := range results {
		status := strconv.Itoa(r.status.Code)
		detail := display.MaskURL(sanitizeTerminal(r.url))
		if r.err != nil {
			status = "ERREUR"
			detail = sanitizeTerminal(r.err.Error())
		}
		prefix := fmt.Sprintf("  #%-4d %-7s %-7s %-10s ", r.id, r.method, status, r.duration)
		detailLines := display.WrapText(detail, max(20, s.termWidth()-display.VisibleWidth(prefix)))
		s.printf("%s%s\n", prefix, detailLines[0])
		for _, cont := range detailLines[1:] {
			s.printf("%s%s\n", strings.Repeat(" ", display.VisibleWidth(prefix)), cont)
		}
	}

	for _, r := range results {
		s.printf("\n")
		s.printReplayResponse(r)
	}
}

func (s *Shell) printReplayResponse(r replayResult) {
	p := s.pal
	width := s.boxWidth()
	title := fmt.Sprintf(" RÉPONSE #%d ", r.id)
	top := "╭─" + title
	top += strings.Repeat("─", max(0, width-display.VisibleWidth(top)-1)) + "╮"
	bottom := "╰" + strings.Repeat("─", max(0, width-2)) + "╯"
	s.printf("%s\n", p.BoldCyan(top))

	if r.err != nil {
		kind := "Erreur d'exécution"
		if r.network {
			kind = "Erreur réseau"
		}
		s.printf("│ %s\n", p.BoldRed(kind))
		s.replayField("Méthode", p.Method(r.method), width)
		s.replayField("URL", p.Cyan(display.MaskURL(sanitizeTerminal(r.url))), width)
		if r.duration != "" {
			s.replayField("Durée", p.Cyan(r.duration), width)
		}
		s.replayField("Détail", p.Red(sanitizeTerminal(r.err.Error())), width)
		s.printf("%s\n", p.BoldCyan(bottom))
		return
	}

	s.replayField("Statut", p.StatusCode(r.status.Code, r.status.Label()), width)
	s.replayField("Méthode", p.Method(r.method), width)
	s.replayField("URL", p.Cyan(display.MaskURL(sanitizeTerminal(r.url))), width)
	s.replayField("Durée", p.Cyan(r.duration), width)
	s.replayField("Taille", p.Bold(display.FormatBytes(r.total)), width)

	s.printf("│\n│ %s\n", p.BoldBlue(fmt.Sprintf("En-têtes de réponse (%d)", len(r.headers))))
	if len(r.headers) == 0 {
		s.printf("│   %s\n", p.Dim("(aucun)"))
	} else {
		for _, h := range r.headers {
			s.printf("%s\n", s.headerLine(h.Name, h.Value, "│   "))
		}
	}

	view := response.RenderBody(r.body, r.total, launchBodyLimit, s.outputMode, r.headers.ContentType())
	s.printf("│\n│ %s\n", p.BoldBlue(fmt.Sprintf("Corps — %s (%s)", bodyKindLabel(view.Kind), display.FormatBytes(r.total))))
	bodyWidth := max(10, width-4)
	for _, line := range strings.Split(view.Text, "\n") {
		for _, wrapped := range display.WrapText(sanitizeTerminal(line), bodyWidth) {
			s.printf("│   %s\n", wrapped)
		}
	}
	if view.Truncated {
		s.printf("│   %s\n", p.BoldYellow(fmt.Sprintf("[…] Affichage limité à %s sur %s reçus ; le corps complet reste disponible.", display.FormatBytes(launchBodyLimit), display.FormatBytes(r.total))))
	}
	if r.bodyPath != "" && r.total > launchBodyLimit {
		if r.total <= 16<<20 {
			s.printf("│   %s\n", p.Dim(fmt.Sprintf("Corps complet : %s · suite : mint.replay.body %d %d", r.bodyPath, r.id, launchBodyLimit)))
		} else {
			s.printf("│   %s\n", p.Dim("Corps complet privé : "+r.bodyPath))
		}
	}
	if r.status.Code >= 500 {
		s.printf("│ %s\n", p.BoldRed("Erreur HTTP côté serveur : la réponse a bien été reçue, ce n'est pas une erreur réseau."))
	} else if r.status.Code >= 400 {
		s.printf("│ %s\n", p.BoldYellow("Erreur HTTP côté client : la réponse a bien été reçue."))
	}
	s.printf("%s\n", p.BoldCyan(bottom))
}

func (s *Shell) replayField(label, value string, width int) {
	p := s.pal
	label = padCell(label, 14)
	prefix := "│ " + p.BoldCyan(label) + p.Dim(" : ")
	available := width - display.VisibleWidth(prefix)
	if available < 10 {
		available = 10
	}
	lines := display.WrapText(value, available)
	for i, line := range lines {
		if i == 0 {
			s.printf("%s%s\n", prefix, line)
		} else {
			s.printf("│ %s%s\n", strings.Repeat(" ", 17), line)
		}
	}
}

// replayCapture exécute une capture et conserve tout ce qui est nécessaire au
// rendu structuré et à mint.replay.body. Aucun résultat n'est simulé : les
// valeurs absentes restent absentes.
func (s *Shell) replayCapture(ctx context.Context, c mint.Capture) replayResult {
	r := replayResult{id: c.ID, method: c.Request.Method, url: c.Request.URL}
	fail := func(err error) replayResult {
		r.err = err
		r.network = true
		return r
	}
	if ok, why := c.Importable(); !ok {
		r.err = fmt.Errorf("%s", why)
		r.network = false
		return r
	}
	b := request.New()
	headers := make([]request.Header, 0, len(c.Request.Headers))
	for _, h := range c.Request.Headers {
		headers = append(headers, request.Header{Name: h.Name, Value: h.Value})
	}
	if err := b.Import(request.CapturedRequest{URL: c.Request.URL, Method: c.Request.Method, Headers: headers, Body: c.Request.Body, BodyPresent: len(c.Request.Body) > 0}); err != nil {
		r.err = err
		return r
	}
	// Replayed captures must not be recorded a second time by this tool's Mint
	// proxy. Captured requests have no portable proxy setting, so replay direct.
	if err := b.SetProxy("none"); err != nil {
		r.err = err
		return r
	}
	res := b.ResolveProxy("", false)
	args, err := b.CurlArgs(res)
	if err != nil {
		r.err = err
		return r
	}
	dir, err := os.MkdirTemp("", "crl-replay-*")
	if err != nil {
		return fail(err)
	}
	keepDir := false
	defer func() {
		if !keepDir {
			_ = os.RemoveAll(dir)
		}
	}()
	headersPath, bodyPath := filepath.Join(dir, "headers"), filepath.Join(dir, "body")
	cmd := exec.CommandContext(ctx, "curl", append(args, "--dump-header", headersPath, "--output", bodyPath, "-w", "%{http_code} %{time_total}")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if b.BodySet() {
		cmd.Stdin = b.BodyReader()
	}
	out, err := cmd.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		// Une connexion refusée (code curl 7) est une erreur réseau précise.
		// Elle doit rester visible même si le contexte vient d'expirer, plutôt
		// que d'être écrasée par un "deadline exceeded" générique.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 7 {
			if message != "" {
				return fail(fmt.Errorf("curl: %s", truncate(sanitizeTerminal(message), 600)))
			}
			return fail(err)
		}
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		if message != "" {
			return fail(fmt.Errorf("curl: %s", truncate(sanitizeTerminal(message), 600)))
		}
		return fail(err)
	}
	parts := strings.Fields(string(out))
	if len(parts) < 2 {
		return fail(fmt.Errorf("réponse curl illisible"))
	}
	d, _ := strconv.ParseFloat(parts[1], 64)
	r.duration = (time.Duration(d * float64(time.Second))).String()
	headerData, _ := os.ReadFile(headersPath)
	httpStatus, parsedHeaders := response.ParseHeaderDump(headerData)
	code, _ := strconv.Atoi(parts[0])
	if httpStatus.Code == 0 {
		httpStatus.Code = code
	}
	if httpStatus.Code == 0 {
		return fail(fmt.Errorf("aucun statut HTTP exploitable"))
	}
	r.status = httpStatus
	r.headers = parsedHeaders
	if info, statErr := os.Stat(bodyPath); statErr == nil {
		r.total = info.Size()
	}
	r.body = readFilePrefix(bodyPath, launchBodyLimit)
	if r.total <= 16<<20 {
		if complete, readErr := os.ReadFile(bodyPath); readErr == nil {
			r.body = display.MaskBody(complete)
		}
	} else {
		r.body = []byte("[corps volumineux : aperçu masqué ; utilisez mint.replay.body pour consulter une page]")
	}
	r.bodyPath = bodyPath
	keepDir = true
	return r
}

func (s *Shell) clearReplayArtifacts() {
	seen := make(map[string]bool)
	for _, dir := range s.replayDirs {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			_ = os.RemoveAll(dir)
		}
	}
	s.replayDirs = nil
	s.replayBodies = make(map[int]string)
}

func (s *Shell) cmdMintReplayBody(rest string) {
	words := strings.Fields(rest)
	if len(words) < 1 || len(words) > 2 {
		s.errorf("usage : mint.replay.body <id> [offset]")
		return
	}
	id, err := parseID(words[0])
	if err != nil {
		s.errorf("%v", err)
		return
	}
	offset := 0
	if len(words) == 2 {
		offset, err = strconv.Atoi(words[1])
		if err != nil || offset < 0 {
			s.errorf("offset invalide")
			return
		}
	}
	path := s.replayBodies[id]
	if path == "" {
		s.errorf("corps de réponse #%d indisponible ; exécutez mint.replay", id)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		s.errorf("corps de réponse #%d expiré", id)
		return
	}
	if info.Size() > 16<<20 {
		s.errorf("réponse supérieure à 16 Mio ; aperçu intégré indisponible")
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		s.errorf("lecture du corps : %v", err)
		return
	}
	body = display.MaskBody(body)
	if offset >= len(body) {
		s.errorf("offset hors limites (%d octets affichés)", len(body))
		return
	}
	end := offset + launchBodyLimit
	if end > len(body) {
		end = len(body)
	}
	for offset < len(body) && offset > 0 && !utf8.RuneStart(body[offset]) {
		offset++
	}
	for end > offset && end < len(body) && !utf8.RuneStart(body[end]) {
		end--
	}
	view, _ := display.RenderBody(body[offset:end], 0)
	s.printf("[Mint] Corps de réponse #%d — octets %d-%d/%d\n", id, offset, end, len(body))
	s.printIndented(view, "  ")
	if end < len(body) {
		s.printf("Suite : mint.replay.body %d %d\n", id, end)
	}
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
	s.requestEdits = make(map[int]*request.Builder)
	s.requestBodyReplaced = make(map[int]bool)
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
	s.activeMintID = c.ID
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

func (s *Shell) cmdMintUnselect(rest string) {
	rest = strings.TrimSpace(rest)
	if strings.EqualFold(rest, "all") || rest == "" {
		s.history.ClearSelection()
		s.printf("[Mint] Sélection effacée.\n")
		return
	}
	words := strings.Fields(rest)
	if len(words) != 1 {
		s.errorf("usage : mint.unselect <id>|all")
		return
	}
	id, err := parseID(words[0])
	if err != nil {
		s.errorf("%v", err)
		return
	}
	s.history.RemoveSelection([]int{id})
	remaining := s.history.SelectedMany()
	if len(remaining) > 0 {
		s.printSelected(remaining)
	} else {
		s.printf("[Mint] Requête #%d retirée de la sélection. Sélection vide.\n", id)
	}
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
	methodBadge := "[" + method + "]"
	if selected > 0 {
		methodBadge = "[Actif: " + method + "]"
	}

	p := s.pal
	if !p.Enabled {
		prompt := fmt.Sprintf("crl %s [Mint: %s]", methodBadge, mintStateStr)
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
	parts = append(parts, p.BoldCyan(methodBadge))
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
	p := s.pal
	width := s.termWidth()
	labelPadded := padCell(label, 17)
	labelVisible := display.VisibleWidth(labelPadded)
	valueStart := 2 + labelVisible + 3 // "  " + label + " : "
	available := width - valueStart
	if available < 10 {
		available = 10
	}
	lines := display.WrapText(value, available)
	indent := strings.Repeat(" ", valueStart)
	for i, line := range lines {
		if i == 0 {
			fmt.Fprintf(s.out, "  %s : %s\n", p.BoldCyan(labelPadded), line)
		} else {
			fmt.Fprintf(s.out, "%s%s\n", indent, line)
		}
	}
}

func (s *Shell) rule(width int) {
	if width < 1 {
		width = 1
	}
	s.printf("%s\n", s.pal.Dim(strings.Repeat("─", width)))
}

func (s *Shell) headerValue(name, value string) string {
	return sanitizeTerminal(display.MaskHeaderValue(name, value))
}

func (s *Shell) headerLine(name, value string, outerPrefix string) string {
	p := s.pal
	nameClean := sanitizeTerminal(name)
	nameVisible := display.VisibleWidth(nameClean)
	prefix := p.BoldCyan(nameClean) + p.Dim(": ")
	clean := s.headerValue(name, value)

	width := s.termWidth()
	totalPrefix := display.VisibleWidth(outerPrefix) + nameVisible + 2
	available := width - totalPrefix
	if available < 10 {
		available = 10
	}

	lines := display.WrapText(clean, available)
	valueIndent := outerPrefix + strings.Repeat(" ", nameVisible+2)

	var b strings.Builder
	b.WriteString(outerPrefix)
	b.WriteString(prefix)
	b.WriteString(lines[0])
	for i := 1; i < len(lines); i++ {
		b.WriteString("\n")
		b.WriteString(valueIndent)
		b.WriteString(lines[i])
	}
	return b.String()
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

func (s *Shell) helpWidth() int {
	if f, ok := s.out.w.(*os.File); ok {
		return terminalWidth(f.Fd())
	}
	return 80
}

// termWidth retourne la largeur du terminal en colonnes. Il consulte d'abord
// la variable COLUMNS, puis l'ioctl du terminal, puis retourne 80 par défaut.
func (s *Shell) termWidth() int {
	if w := os.Getenv("COLUMNS"); w != "" {
		if n, err := strconv.Atoi(w); err == nil && n > 10 {
			return n
		}
	}
	if f, ok := s.out.w.(*os.File); ok {
		return terminalWidth(f.Fd())
	}
	return 80
}

// boxWidth retourne la largeur maximale à utiliser pour un cadre, bornée
// entre 40 et 200 colonnes.
func (s *Shell) boxWidth() int {
	w := s.termWidth()
	if w < 40 {
		return 40
	}
	if w > 200 {
		return 200
	}
	return w
}

func (s *Shell) printHelp(query string) {
	if query == "" {
		s.printGeneralHelp()
		return
	}
	if !renderHelp(s.out, s.pal, query, unicodeTerminal(), s.helpWidth()) {
		s.errorf("aucune aide trouvée pour %q ; utilisez help pour la liste des commandes", query)
	}
}

func (s *Shell) printGeneralHelp() {
	p := s.pal
	fmt.Fprintf(s.out, "\n%s\n", p.BoldCyan("╭─ CRL-CLI "))
	fmt.Fprintf(s.out, "%s\n", p.Bold("│  CRL-CLI — client HTTP en ligne de commande avec proxy de capture Mint"))
	fmt.Fprintf(s.out, "%s\n", p.Cyan("╰─────────────────────────────────────────────────────────────────────────────"))
	fmt.Fprintf(s.out, "\n%s\n", p.BoldBlue("PRÉSENTATION"))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("CRL  : construire et envoyer des requêtes HTTP (crl.host, crl.method, crl.head, crl.json, crl.body, crl.launch)."))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("Mint : capturer le trafic HTTP via un proxy local, sélectionner et rejouer des requêtes (mint.proxy, mint.requests, mint.select, mint.replay)."))
	fmt.Fprintf(s.out, "\n%s\n", p.BoldBlue("AIDE DÉTAILLÉE"))
	fmt.Fprintf(s.out, "  %s  %s\n", p.Cyan("crl.help"), p.Dim("toutes les commandes CRL (construction, envoi, inspection)."))
	fmt.Fprintf(s.out, "  %s  %s\n", p.Cyan("mint.help"), p.Dim("toutes les commandes Mint (capture, sélection, rejeu, proxy)."))
	fmt.Fprintf(s.out, "\n%s\n", p.BoldBlue("WORKFLOW TYPIQUE"))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("1. crl.host <url>        — définir l'URL cible"))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("2. crl.method <GET|POST|…> — définir la méthode"))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("3. crl.json <JSON>       — ou crl.body <texte> pour configurer le corps"))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("4. crl.launch            — envoyer la requête"))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("   mint.proxy on          — démarrer le proxy de capture"))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("   mint.requests          — lister les captures"))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("   mint.select 1 2 3      — sélectionner des captures"))
	fmt.Fprintf(s.out, "  %s\n", p.Dim("   crl.launch             — rejouer la sélection (ou crl.launch 5 pour une seule)"))
	fmt.Fprintf(s.out, "\n%s\n", p.Dim("Utilisez crl.help ou mint.help pour la liste complète des commandes de chaque catégorie."))
	fmt.Fprintf(s.out, "\n")
	renderHelp(s.out, s.pal, "", unicodeTerminal(), s.helpWidth())
}

func (s *Shell) printMintHelp() {
	renderHelp(s.out, s.pal, "mint", unicodeTerminal(), s.helpWidth())
	s.printf("\n[Mint] Le rejeu est explicite : sélectionnez avec mint.select, puis lancez mint.replay.\n")
	s.printf("[Mint] mint.use importe une capture dans le constructeur actif sans modifier la sélection ni envoyer la requête.\n")
}

func (s *Shell) printCrlHelp() {
	renderHelp(s.out, s.pal, "crl", unicodeTerminal(), s.helpWidth())
	s.printf("\n[CRL] crl.launch : sans argument, envoie le constructeur actif (ou rejoue la sélection Mint si une existe).\n")
	s.printf("[CRL] crl.launch <id> : envoie uniquement la capture indiquée, sans modifier la sélection.\n")
	s.printf("[CRL] crl.launch active : envoie le constructeur actif.\n")
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
	case strings.HasPrefix(msg, "[CRL] Échec :"), strings.HasPrefix(msg, "[CRL] ERREUR"):
		return s.pal.BoldRed("[CRL]") + s.pal.Red(msg[len("[CRL]"):])
	case strings.HasPrefix(msg, "[CRL] Attention"), strings.HasPrefix(msg, "[CRL] Avertissement"):
		return s.pal.BoldYellow("[CRL]") + s.pal.Yellow(msg[len("[CRL]"):])
	case strings.HasPrefix(msg, "[CRL] Aucune"), strings.HasPrefix(msg, "[CRL] Aucun"), strings.HasPrefix(msg, "[CRL] En-tête ") && strings.Contains(msg, " absent"):
		return s.pal.BoldYellow("[CRL]") + s.pal.Yellow(msg[len("[CRL]"):])
	case strings.HasPrefix(msg, "[CRL] OK"):
		return s.pal.BoldGreen("[CRL]") + s.pal.Green(msg[len("[CRL]"):])
	case strings.HasPrefix(msg, "[CRL] "):
		body := msg[len("[CRL]"):]
		lower := strings.ToLower(body)
		if containsAny(lower, "configuré", "configurée", "ajouté", "ajoutée", "supprimé", "supprimée", "modifié", "modifiée", "appliqué", "appliquée", "réinitialisé", "réinitialisée", "envoyée", "envoyé", "effacé", "effacée", "activées", "activés") {
			return s.pal.BoldGreen("[CRL]") + s.pal.Green(body)
		}
		return s.pal.BoldCyan("[CRL]") + s.pal.White(body)
	case strings.HasPrefix(msg, "[Mint] Échec"), strings.HasPrefix(msg, "[Mint] Erreur"):
		return s.pal.Red(msg)
	case strings.HasPrefix(msg, "[Mint] Attention"), strings.HasPrefix(msg, "[Mint] Aucune"), strings.HasPrefix(msg, "[Mint] Aucun"), strings.HasPrefix(msg, "[Mint] Indisponible"):
		return s.pal.Yellow(msg)
	case strings.HasPrefix(msg, "[Mint] Démarré"), strings.HasPrefix(msg, "[Mint] Arrêté"), strings.HasPrefix(msg, "[Mint] Capture enregistrée"):
		return s.pal.BoldGreen("[Mint]") + s.pal.Green(msg[len("[Mint]"):])
	case strings.HasPrefix(msg, "[Mint] "):
		return s.pal.BoldMagenta("[Mint]") + s.pal.White(msg[len("[Mint]"):])
	default:
		return msg
	}
}

func containsAny(text string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
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
	wrapWidth := s.termWidth() - len(prefix)
	if wrapWidth < 10 {
		wrapWidth = 10
	}
	for _, line := range strings.Split(text, "\n") {
		line = sanitizeTerminal(line)
		if s.pal.Enabled && (strings.Contains(line, "tronqué") || strings.HasPrefix(line, "[corps binaire")) {
			line = s.pal.Yellow(line)
		}
		// On écrit directement : un corps peut commencer par « [CRL] » ou
		// « [Mint] » sans que cela doive déclencher la coloration des préfixes.
		for _, wline := range display.WrapText(line, wrapWidth) {
			fmt.Fprintf(s.out, "%s%s\n", prefix, wline)
		}
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
