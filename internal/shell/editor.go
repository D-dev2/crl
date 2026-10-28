package shell

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// editMultiline opens VISUAL/EDITOR with an existing UTF-8 body and returns
// the exact bytes saved. The editor is executed directly, without a shell.
func editMultiline(pattern string, initial []byte) ([]byte, error) {
	name, err := os.CreateTemp("", pattern)
	if err != nil {
		return nil, err
	}
	path := name.Name()
	defer os.Remove(path)
	if _, err = name.Write(initial); err != nil {
		name.Close()
		return nil, err
	}
	if err = name.Close(); err != nil {
		return nil, err
	}
	choice := strings.TrimSpace(os.Getenv("VISUAL"))
	if choice == "" {
		choice = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if choice == "" {
		choice = "vi"
	}
	parts := strings.Fields(choice)
	if len(parts) == 0 {
		return nil, fmt.Errorf("éditeur vide")
	}
	if !filepath.IsAbs(parts[0]) {
		if _, err := exec.LookPath(parts[0]); err != nil {
			return nil, err
		}
	}
	args := append(parts[1:], path)
	cmd := exec.Command(parts[0], args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// editorResult est le résultat du traitement d'un lot d'octets saisis.
type editorResult struct {
	Lines       []string
	EOF         bool
	Interrupted bool
}

// editor est un éditeur de ligne minimal en mode brut. Il permet de redessiner
// le prompt et la saisie courante lorsqu'une notification Mint arrive, sans
// corrompre le texte déjà tapé.
type editor struct {
	out    io.Writer
	prompt string
	buf    []rune
	pos    int

	// originSaved mémorise le début du prompt avec une séquence ANSI de
	// sauvegarde de curseur. Les redessins et les notifications repartent de
	// cette origine, ce qui nettoie aussi les lignes physiques produites par
	// un prompt ou un collage plus long que la largeur du terminal.
	originSaved  bool
	promptActive bool

	history []string
	histIdx int
	saved   string

	escapeBuf []byte
	utf8Buf   []byte
}

func newEditor(out io.Writer, prompt string) *editor {
	return &editor{out: out, prompt: prompt}
}

func (e *editor) SetPrompt(prompt string) { e.prompt = prompt }

func (e *editor) Reset() {
	e.buf = nil
	e.pos = 0
	e.histIdx = len(e.history)
	e.saved = ""
}

func (e *editor) Text() string { return string(e.buf) }

// Active indique qu'un prompt est actuellement affiché et que la saisie est
// en cours. Les notifications peuvent alors utiliser Notify plutôt que
// PrintLines.
func (e *editor) Active() bool { return e.promptActive }

// DrawPrompt affiche un nouveau prompt à la position courante et mémorise son
// origine. À utiliser après une commande ou au démarrage.
func (e *editor) DrawPrompt() {
	e.drawFresh()
}

// drawFresh efface la ligne courante, mémorise l'origine du prompt puis
// redessine prompt et saisie. L'origine est ensuite utilisée par redraw et
// Notify pour effacer tout le bloc, y compris les lignes de continuation.
func (e *editor) drawFresh() {
	fmt.Fprint(e.out, "\r\033[K\033[s")
	e.drawContent()
	e.originSaved = true
	e.promptActive = true
}

// redraw repart de l'origine mémorisée, efface le prompt et toutes ses lignes
// de continuation, puis redessine la saisie. Cela évite les fragments de
// prompt laissés par un effacement limité à la dernière ligne physique.
func (e *editor) redraw() {
	if e.originSaved {
		fmt.Fprint(e.out, "\033[u\033[J")
	} else {
		fmt.Fprint(e.out, "\r\033[K\033[s")
		e.originSaved = true
	}
	e.drawContent()
	e.promptActive = true
}

func (e *editor) drawContent() {
	fmt.Fprint(e.out, e.prompt, string(e.buf))
	e.reposition()
}

func (e *editor) reposition() {
	if e.pos < len(e.buf) {
		fmt.Fprintf(e.out, "\033[%dD", len(e.buf)-e.pos)
	}
}

// PrintLines affiche des lignes sans toucher au prompt. Utilisé après
// l'exécution d'une commande, quand aucun prompt n'est affiché.
func (e *editor) PrintLines(lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprint(e.out, "\r\033[K")
	e.writeLines(lines)
}

func (e *editor) writeLines(lines []string) {
	for _, line := range lines {
		fmt.Fprint(e.out, sanitizeTerminalKeepANSI(line), "\r\n")
	}
}

// Notify affiche une ou plusieurs lignes au-dessus du prompt, puis restaure
// la saisie en cours. L'appel est centralisé par le shell, dans la goroutine
// qui lit les commandes : aucune goroutine Mint n'écrit directement ici.
func (e *editor) Notify(lines []string) {
	if len(lines) == 0 {
		return
	}
	if !e.promptActive {
		e.PrintLines(lines)
		return
	}
	if e.originSaved {
		fmt.Fprint(e.out, "\033[u\033[J")
	} else {
		fmt.Fprint(e.out, "\r\033[K")
	}
	e.writeLines(lines)
	// Après les notifications, l'origine du prompt est déplacée sous celles-ci.
	e.drawFresh()
}

func (e *editor) Feed(data []byte) editorResult {
	var result editorResult
	for _, b := range data {
		if len(e.escapeBuf) > 0 || b == 0x1b {
			e.escapeBuf = append(e.escapeBuf, b)
			consumed, action := e.parseEscape()
			if consumed {
				e.escapeBuf = nil
				if action != nil {
					action(&result)
				}
			}
			continue
		}

		e.utf8Buf = append(e.utf8Buf, b)
		if !utf8.FullRune(e.utf8Buf) {
			continue
		}
		r, size := utf8.DecodeRune(e.utf8Buf)
		if r == utf8.RuneError && size == 1 {
			// Octet invalide : on l'ignore plutôt que de corrompre la ligne.
			e.utf8Buf = nil
			continue
		}
		e.utf8Buf = e.utf8Buf[size:]
		e.handleRune(r, &result)
	}
	return result
}

func (e *editor) handleRune(r rune, result *editorResult) {
	switch r {
	case '\r', '\n':
		line := string(e.buf)
		fmt.Fprint(e.out, "\r\n")
		e.promptActive = false
		if strings.TrimSpace(line) != "" {
			e.history = append(e.history, line)
			if len(e.history) > 200 {
				e.history = e.history[len(e.history)-200:]
			}
		}
		e.buf = nil
		e.pos = 0
		e.histIdx = len(e.history)
		e.saved = ""
		result.Lines = append(result.Lines, line)
	case 0x03: // Ctrl-C
		fmt.Fprint(e.out, "^C\r\n")
		e.buf = nil
		e.pos = 0
		e.histIdx = len(e.history)
		e.promptActive = false
		result.Interrupted = true
	case 0x04: // Ctrl-D
		if len(e.buf) == 0 {
			fmt.Fprint(e.out, "\r\n")
			e.promptActive = false
			result.EOF = true
			return
		}
		if e.pos < len(e.buf) {
			e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
			e.redraw()
		}
	case 0x01: // Ctrl-A
		e.pos = 0
		e.redraw()
	case 0x05: // Ctrl-E
		e.pos = len(e.buf)
		e.redraw()
	case 0x02: // Ctrl-B
		if e.pos > 0 {
			e.pos--
			e.redraw()
		}
	case 0x06: // Ctrl-F
		if e.pos < len(e.buf) {
			e.pos++
			e.redraw()
		}
	case 0x0b: // Ctrl-K
		e.buf = e.buf[:e.pos]
		e.redraw()
	case 0x15: // Ctrl-U
		e.buf = append([]rune(nil), e.buf[e.pos:]...)
		e.pos = 0
		e.redraw()
	case 0x17: // Ctrl-W
		start := e.pos
		for start > 0 && e.buf[start-1] == ' ' {
			start--
		}
		for start > 0 && e.buf[start-1] != ' ' {
			start--
		}
		e.buf = append(e.buf[:start], e.buf[e.pos:]...)
		e.pos = start
		e.redraw()
	case 0x0c: // Ctrl-L
		fmt.Fprint(e.out, "\033[2J\033[H")
		e.drawFresh()
	case 0x7f, 0x08: // Retour arrière
		if e.pos > 0 {
			e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
			e.pos--
			e.redraw()
		}
	case 0x09: // Tabulation : insère deux espaces
		e.insert([]rune("  "))
	default:
		if r < 0x20 {
			return
		}
		e.insert([]rune{r})
	}
}

func (e *editor) insert(runes []rune) {
	atEnd := e.pos == len(e.buf)
	out := make([]rune, 0, len(e.buf)+len(runes))
	out = append(out, e.buf[:e.pos]...)
	out = append(out, runes...)
	out = append(out, e.buf[e.pos:]...)
	e.buf = out
	e.pos += len(runes)
	if atEnd {
		// À la fin de la ligne, écrire seulement les nouveaux caractères évite
		// de redessiner tout le tampon à chaque octet d'un collage. C'est
		// indispensable lorsque la ligne dépasse la largeur du terminal : un
		// redessin complet ne nettoierait que la dernière ligne visuelle.
		fmt.Fprint(e.out, string(runes))
		return
	}
	e.redraw()
}

// parseEscape analyse une séquence ANSI. Le booléen indique si la séquence est
// complète ; l'action optionnelle est exécutée par Feed.
func (e *editor) parseEscape() (bool, func(*editorResult)) {
	seq := string(e.escapeBuf)
	if seq == "\x1b" {
		return false, nil
	}
	if len(seq) == 2 && seq[1] != '[' && seq[1] != 'O' {
		return true, nil // Alt+touche : ignoré
	}
	if len(seq) < 3 && !strings.HasPrefix(seq, "\x1b[") && !strings.HasPrefix(seq, "\x1bO") {
		return false, nil
	}
	if !strings.HasPrefix(seq, "\x1b[") && !strings.HasPrefix(seq, "\x1bO") {
		return true, nil
	}
	if len(seq) == 2 {
		return false, nil
	}

	body := seq[2:]
	switch body {
	case "A":
		return true, func(*editorResult) { e.historyPrev() }
	case "B":
		return true, func(*editorResult) { e.historyNext() }
	case "C":
		return true, func(*editorResult) {
			if e.pos < len(e.buf) {
				e.pos++
				e.redraw()
			}
		}
	case "D":
		return true, func(*editorResult) {
			if e.pos > 0 {
				e.pos--
				e.redraw()
			}
		}
	case "H", "1~", "7~":
		return true, func(*editorResult) { e.pos = 0; e.redraw() }
	case "F", "4~", "8~":
		return true, func(*editorResult) { e.pos = len(e.buf); e.redraw() }
	case "3~":
		return true, func(*editorResult) {
			if e.pos < len(e.buf) {
				e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
				e.redraw()
			}
		}
	}
	if len(body) >= 1 {
		last := body[len(body)-1]
		if (last >= 'A' && last <= 'Z') || last == '~' {
			return true, nil
		}
	}
	return false, nil
}

func (e *editor) historyPrev() {
	if len(e.history) == 0 {
		return
	}
	if e.histIdx == len(e.history) {
		e.saved = string(e.buf)
	}
	if e.histIdx > 0 {
		e.histIdx--
	}
	e.setBuf(e.history[e.histIdx])
}

func (e *editor) historyNext() {
	if len(e.history) == 0 || e.histIdx >= len(e.history) {
		return
	}
	e.histIdx++
	if e.histIdx == len(e.history) {
		e.setBuf(e.saved)
		return
	}
	e.setBuf(e.history[e.histIdx])
}

func (e *editor) setBuf(text string) {
	e.buf = []rune(text)
	e.pos = len(e.buf)
	e.redraw()
}

func sanitizeTerminal(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// sanitizeTerminalKeepANSI nettoie du texte destiné au terminal tout en
// conservant uniquement les séquences SGR de couleur (finissant par « m »).
// Cela permet d'afficher des notifications colorées au-dessus du prompt sans
// laisser passer de séquences de déplacement de curseur ou d'autres contrôles.
func sanitizeTerminalKeepANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == ';') {
					j++
				}
				if j < len(s) {
					if s[j] == 'm' {
						b.WriteString(s[i : j+1])
					}
					i = j + 1
					continue
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\n' || r == '\r':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// caractère de contrôle ignoré
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}
