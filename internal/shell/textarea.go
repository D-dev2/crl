package shell

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// textArea is the multiline editor used by the interactive shell. Input stays
// in the shell's existing raw-mode event loop, so there is only one owner of
// the terminal and Mint notifications can be queued safely while it is open.
type textArea struct {
	out       io.Writer
	title     string
	target    string
	buf       []rune
	cursor    int
	top       int
	rows      int
	width     int
	escape    []byte
	escapeAt  time.Time
	utf8Bytes []byte
	lastCR    bool
	message   string

	// drawnRows est le nombre de lignes occupées par le dernier panneau.
	// cursorOffset est la distance, en lignes, entre le haut du panneau et le
	// curseur visible. Ces deux valeurs permettent de replacer le curseur par
	// mouvements relatifs, sans dépendre de la mémoire de curseur ANSI qui
	// devient fausse dès que le panneau provoque un défilement du terminal.
	drawn        bool
	drawnRows    int
	cursorOffset int
	termHeight   int
	newlines     int
}

type textAreaAction uint8

const (
	textAreaNone textAreaAction = iota
	textAreaSave
	textAreaCancel
)

func newTextArea(out io.Writer, title, target string, initial []byte) *textArea {
	buffer := []rune(string(initial))
	newlines := 0
	for _, r := range buffer {
		if r == '\n' {
			newlines++
		}
	}
	a := &textArea{out: out, title: title, target: target, buf: buffer, cursor: len(buffer), rows: textAreaMinRows, width: 78, termHeight: 24, newlines: newlines}
	return a
}

const (
	// Le panneau comporte huit lignes fixes : bord supérieur, titre, cible,
	// séparateur, séparateur, deux lignes d'aide et bord inférieur.
	textAreaFrameExtra = 8
	textAreaMinRows    = 3
	textAreaMaxRows    = 18
)

func (a *textArea) SetSize(width, height int) {
	if width > 0 {
		if width > 100 {
			width = 100
		}
		// Les très petites largeurs restent exploitables ; le cadre peut alors
		// dépasser légèrement le terminal, mais aucun index négatif n'est créé.
		if width < 20 {
			width = 20
		}
		a.width = width - 2
	}
	if height > 0 {
		a.termHeight = height
	}

	maxRows := a.termHeight - textAreaFrameExtra
	if maxRows < 1 {
		maxRows = 1
	}
	if maxRows > textAreaMaxRows {
		maxRows = textAreaMaxRows
	}

	// Un petit JSON n'a pas besoin de remplir tout le terminal. On garde au
	// moins trois lignes d'édition, puis on s'arrête à la hauteur disponible.
	desired := a.newlines + 2
	if desired < textAreaMinRows {
		desired = textAreaMinRows
	}
	if desired > maxRows {
		desired = maxRows
	}
	if desired < 1 {
		desired = 1
	}
	a.rows = desired
	a.clampTop()
}

func (a *textArea) Text() string        { return string(a.buf) }
func (a *textArea) EscapePending() bool { return string(a.escape) == "\x1b" }
func (a *textArea) CancelPendingEscape() bool {
	if !a.EscapePending() {
		return false
	}
	a.escape = nil
	a.escapeAt = time.Time{}
	return true
}

// Clear efface le panneau depuis la position réelle du curseur. Les
// déplacements sont relatifs au curseur visible, pas à une position ANSI
// mémorisée : si le panneau a fait défiler le terminal, le haut du panneau
// reste joignable en remontant le nombre de lignes effectivement dessinées.
func (a *textArea) Clear() {
	if !a.drawn {
		return
	}
	fmt.Fprint(a.out, "\r")
	if a.cursorOffset > 0 {
		fmt.Fprintf(a.out, "\033[%dA", a.cursorOffset)
	}
	fmt.Fprint(a.out, "\033[J")
	a.drawn = false
	a.drawnRows = 0
	a.cursorOffset = 0
}

func (a *textArea) Draw(p palette, unicode bool) {
	a.Clear()
	fmt.Fprint(a.out, "\r")

	left, right, horiz, top, mid, bottom := "|", "|", "-", "+", "+", "+"
	if unicode {
		left, right, horiz, top, mid, bottom = "│", "│", "─", "╭", "├", "╰"
	}
	line := func(l, r, value string) {
		value = truncateDisplay(value, a.width-2)
		pad := a.width - 1 - displayWidth(value)
		if pad < 0 {
			pad = 0
		}
		fmt.Fprintf(a.out, "%s%s%s%s%s\r\n", l, " ", value, strings.Repeat(" ", pad), r)
	}
	bar := strings.Repeat(horiz, a.width)
	fmt.Fprintf(a.out, "%s%s%s\r\n", top, bar, top)
	line(left, right, p.BoldCyan(a.title))
	line(left, right, p.Dim("Cible : ")+p.Cyan(a.target))
	fmt.Fprintf(a.out, "%s%s%s\r\n", mid, bar, mid)

	row, col := a.cursorPosition()
	if row < a.top {
		a.top = row
	}
	if row >= a.top+a.rows {
		a.top = row - a.rows + 1
	}
	a.clampTop()
	lines := a.visibleLines(a.top, a.rows)
	for i := 0; i < a.rows; i++ {
		idx := a.top + i
		content := "~"
		if i < len(lines) {
			content = a.horizontalLine(lines[i], idx == row, col)
		}
		line(left, right, content)
	}
	fmt.Fprintf(a.out, "%s%s%s\r\n", mid, bar, mid)
	line(left, right, "Ctrl+S : valider   Esc : annuler")
	if a.message != "" {
		line(left, right, p.BoldYellow(a.message))
	} else {
		line(left, right, "Flèches : déplacer   Suppr/Retour arrière : modifier")
	}
	fmt.Fprintf(a.out, "%s%s%s", bottom, bar, bottom)

	visibleRow := row - a.top
	if visibleRow < 0 {
		visibleRow = 0
	}
	if visibleRow >= a.rows {
		visibleRow = a.rows - 1
	}
	_, visibleCol := a.horizontalStart(col)
	// Le curseur est actuellement après le bord inférieur. On remonte jusqu'à
	// la ligne de contenu visible, puis on avance jusqu'à la colonne calculée.
	frameHeight := a.rows + textAreaFrameExtra
	a.cursorOffset = 4 + visibleRow
	if up := frameHeight - 1 - a.cursorOffset; up > 0 {
		fmt.Fprintf(a.out, "\r\033[%dA", up)
	} else {
		fmt.Fprint(a.out, "\r")
	}
	if visibleCol+2 > 0 {
		fmt.Fprintf(a.out, "\033[%dC", visibleCol+2)
	}
	a.drawn = true
	a.drawnRows = frameHeight
}

func (a *textArea) clampTop() {
	if a.top < 0 {
		a.top = 0
	}
	row, _ := a.cursorPosition()
	if row < a.top {
		a.top = row
	}
	if a.rows > 0 && row >= a.top+a.rows {
		a.top = row - a.rows + 1
	}
	if a.top < 0 {
		a.top = 0
	}
}

func (a *textArea) horizontalStart(col int) (start, visibleCol int) {
	width := a.width - 4
	if width < 1 {
		width = 1
	}
	if col >= width {
		start = col - width + 1
	}
	visibleCol = col - start
	if visibleCol < 0 {
		visibleCol = 0
	}
	if visibleCol > width-1 {
		visibleCol = width - 1
	}
	return start, visibleCol
}

func (a *textArea) horizontalLine(line string, cursorLine bool, col int) string {
	width := a.width - 4
	if width < 1 {
		width = 1
	}
	start := 0
	if cursorLine {
		start, _ = a.horizontalStart(col)
	}
	end := start + width
	var b strings.Builder
	cell := 0
	for _, r := range line {
		if r == '\t' {
			next := nextTabStop(cell)
			for cell < next {
				if cell >= start && cell < end {
					b.WriteByte(' ')
				}
				cell++
			}
			if cell >= end {
				break
			}
			continue
		}
		w := textCellWidth(r)
		if w == 0 {
			// Les marques combinantes restent attachées au caractère précédent
			// lorsqu'il est visible.
			if cell > start && cell <= end {
				b.WriteRune(r)
			}
			continue
		}
		if cell >= start && cell+w <= end {
			b.WriteString(textCellRune(r))
		}
		cell += w
		if cell >= end {
			break
		}
	}
	return b.String()
}

func (a *textArea) cursorPosition() (row, col int) {
	for i := 0; i < a.cursor && i < len(a.buf); i++ {
		r := a.buf[i]
		switch r {
		case '\n':
			row++
			col = 0
		case '\r':
			// Un CRLF d'origine ne doit pas ajouter une colonne fantôme :
			// l'affichage masque ce CR et le contenu sauvegardé reste intact.
			if i+1 < len(a.buf) && a.buf[i+1] == '\n' {
				continue
			}
			col++
		case '\t':
			col = nextTabStop(col)
		default:
			col += textCellWidth(r)
		}
	}
	return row, col
}

// visibleLines extrait uniquement la fenêtre demandée. Un corps volumineux
// n'est donc jamais découpé en une tranche de chaînes pour chaque frappe.
func (a *textArea) visibleLines(from, count int) []string {
	if from < 0 {
		from = 0
	}
	if count < 1 {
		return nil
	}
	out := make([]string, 0, count)
	row, start := 0, 0
	for i := 0; i <= len(a.buf) && row < from+count; i++ {
		atEnd := i == len(a.buf)
		if !atEnd && a.buf[i] != '\n' {
			continue
		}
		if row >= from {
			end := i
			if !atEnd && end > start && a.buf[end-1] == '\r' {
				end--
			}
			out = append(out, string(a.buf[start:end]))
		}
		row++
		start = i + 1
	}
	return out
}

// lineBounds retourne les bornes d'une ligne logique. Le saut de ligne final
// n'est pas inclus ; hasNewline permet d'ignorer un CR uniquement lorsqu'il
// forme une paire CRLF avec ce saut.
func (a *textArea) lineBounds(target int) (start, end int, hasNewline bool) {
	if target < 0 {
		target = 0
	}
	row := 0
	for i := 0; i <= len(a.buf); i++ {
		if i == len(a.buf) {
			return start, i, false
		}
		if a.buf[i] != '\n' {
			continue
		}
		if row == target {
			return start, i, true
		}
		row++
		start = i + 1
	}
	return start, len(a.buf), false
}

func (a *textArea) Feed(data []byte) textAreaAction {
	action := textAreaNone
	for _, b := range data {
		if len(a.escape) > 0 || b == 0x1b {
			if len(a.escape) == 0 && b == 0x1b {
				a.escapeAt = time.Now()
			}
			a.escape = append(a.escape, b)
			if done := a.feedEscape(); done != textAreaNone {
				action = done
			}
			continue
		}
		a.utf8Bytes = append(a.utf8Bytes, b)
		if !utf8.FullRune(a.utf8Bytes) {
			continue
		}
		r, size := utf8.DecodeRune(a.utf8Bytes)
		if r == utf8.RuneError && size == 1 {
			a.utf8Bytes = a.utf8Bytes[1:]
			continue
		}
		a.utf8Bytes = a.utf8Bytes[size:]
		if r == '\n' && a.lastCR {
			a.lastCR = false
			continue
		}
		a.lastCR = r == '\r'
		switch r {
		case '\r', '\n':
			a.insert('\n')
		case 0x13:
			action = textAreaSave
		case 0x03, 0x1b:
			action = textAreaCancel
		case 0x08, 0x7f:
			a.backspace()
		case 0x04:
			a.delete()
		case 0x01:
			a.moveLineEdge(false)
		case 0x05:
			a.moveLineEdge(true)
		case 0x0c: // Ctrl+L redraw
		case 0x09:
			a.insert('\t')
		default:
			if r >= 0x20 {
				a.insert(r)
			}
		}
	}
	return action
}

func (a *textArea) feedEscape() textAreaAction {
	seq := string(a.escape)
	if seq == "\x1b" {
		return textAreaNone
	}
	if len(seq) == 2 && seq[1] != '[' && seq[1] != 'O' {
		a.escape = nil
		a.escapeAt = time.Time{}
		return textAreaNone
	}
	if len(seq) < 3 {
		return textAreaNone
	}
	if !strings.HasPrefix(seq, "\x1b[") && !strings.HasPrefix(seq, "\x1bO") {
		a.escape = nil
		return textAreaNone
	}
	body := seq[2:]
	done := true
	if len(body) == 1 {
		switch body[0] {
		case 'A':
			a.moveVertical(-1)
		case 'B':
			a.moveVertical(1)
		case 'C':
			a.moveHorizontal(1)
		case 'D':
			a.moveHorizontal(-1)
		case 'H':
			a.moveLineEdge(false)
		case 'F':
			a.moveLineEdge(true)
		default:
			done = false
		}
	} else if body == "3~" {
		a.delete()
	} else if body == "1~" || body == "7~" {
		a.moveLineEdge(false)
	} else if body == "4~" || body == "8~" {
		a.moveLineEdge(true)
	} else {
		last := body[len(body)-1]
		done = last == '~' || (last >= 'A' && last <= 'Z')
	}
	if done {
		a.escape = nil
		a.escapeAt = time.Time{}
	}
	return textAreaNone
}

func (a *textArea) insert(r rune) {
	a.buf = append(a.buf, 0)
	copy(a.buf[a.cursor+1:], a.buf[a.cursor:])
	a.buf[a.cursor] = r
	a.cursor++
	if r == '\n' {
		a.newlines++
	}
	a.message = ""
}
func (a *textArea) backspace() {
	if a.cursor > 0 {
		removed := a.buf[a.cursor-1]
		a.buf = append(a.buf[:a.cursor-1], a.buf[a.cursor:]...)
		a.cursor--
		if removed == '\n' {
			a.newlines--
		}
		a.message = ""
	}
}
func (a *textArea) delete() {
	if a.cursor < len(a.buf) {
		removed := a.buf[a.cursor]
		a.buf = append(a.buf[:a.cursor], a.buf[a.cursor+1:]...)
		if removed == '\n' {
			a.newlines--
		}
		a.message = ""
	}
}
func (a *textArea) moveHorizontal(delta int) {
	a.cursor += delta
	if a.cursor < 0 {
		a.cursor = 0
	}
	if a.cursor > len(a.buf) {
		a.cursor = len(a.buf)
	}
}
func (a *textArea) moveLineEdge(end bool) {
	start := a.cursor
	for start > 0 && a.buf[start-1] != '\n' {
		start--
	}
	if !end {
		a.cursor = start
		return
	}
	stop := start
	for stop < len(a.buf) && a.buf[stop] != '\n' {
		stop++
	}
	a.cursor = stop
}
func (a *textArea) moveVertical(delta int) {
	row, col := a.cursorPosition()
	target := row + delta
	if target < 0 {
		target = 0
	}
	if lines := a.newlines + 1; target >= lines {
		target = lines - 1
	}
	start, end, hasNewline := a.lineBounds(target)
	if hasNewline && end > start && a.buf[end-1] == '\r' {
		end--
	}
	// On conserve la colonne visuelle : les tabulations et les caractères
	// larges ne décalent donc pas le curseur d'une ligne à l'autre.
	idx := end
	cell := 0
	for i := start; i < end; i++ {
		r := a.buf[i]
		if r == '\t' {
			next := nextTabStop(cell)
			if next > col {
				idx = i
				break
			}
			cell = next
			idx = i + 1
			continue
		}
		w := textCellWidth(r)
		if cell+w > col {
			idx = i
			break
		}
		cell += w
		idx = i + 1
	}
	a.cursor = idx
}

const textAreaTabWidth = 8

func nextTabStop(col int) int {
	if col < 0 {
		col = 0
	}
	return col + textAreaTabWidth - col%textAreaTabWidth
}

// textCellWidth donne la largeur d'affichage d'une rune. Les caractères de
// contrôle sont rendus par un symbole d'une colonne ; les marques combinantes
// n'occupent aucune colonne et les caractères larges en occupent deux.
func textCellWidth(r rune) int {
	if r == '\r' || r < 0x20 || r == 0x7f {
		return 1
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	if isWideRune(r) {
		return 2
	}
	return 1
}

func textCellRune(r rune) string {
	switch {
	case r == '\r':
		return "␍"
	case r < 0x20 || r == 0x7f:
		return "·"
	default:
		return string(r)
	}
}

func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f,
		r == 0x2329, r == 0x232a,
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe10 && r <= 0xfe19,
		r >= 0xfe30 && r <= 0xfe6f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f000 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd:
		return true
	default:
		return false
	}
}

// displayWidth ignore les séquences ANSI et compte les colonnes du terminal.
func displayWidth(s string) int {
	width := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = ansiSequenceEnd(s, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r == '\t' {
			width = nextTabStop(width)
			continue
		}
		width += textCellWidth(r)
	}
	return width
}

func ansiSequenceEnd(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	if s[i+1] == '[' {
		j := i + 2
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
		if j < len(s) {
			j++
		}
		return j
	}
	if i+2 <= len(s) {
		return i + 2
	}
	return len(s)
}

// truncateDisplay tronque sans casser une séquence ANSI ni un caractère large.
func truncateDisplay(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if displayWidth(s) <= limit {
		return s
	}
	var b strings.Builder
	used := 0
	hadANSI := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			end := ansiSequenceEnd(s, i)
			b.WriteString(s[i:end])
			hadANSI = true
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w := textCellWidth(r)
		if r == '\t' {
			w = nextTabStop(used) - used
		}
		if used+w > limit-1 {
			break
		}
		b.WriteRune(r)
		used += w
		i += size
	}
	b.WriteString("…")
	if hadANSI {
		b.WriteString("\033[0m")
	}
	return b.String()
}
