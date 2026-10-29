package shell

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// textArea is the multiline editor used by the interactive shell. Input stays
// in the shell's existing raw-mode event loop, so there is only one owner of
// the terminal and Mint notifications can be queued safely while it is open.
type textArea struct {
	out         io.Writer
	title       string
	target      string
	buf         []rune
	cursor      int
	top         int
	rows        int
	width       int
	escape      []byte
	escapeAt    time.Time
	utf8Bytes   []byte
	lastCR      bool
	message     string
	originSaved bool
}

type textAreaAction uint8

const (
	textAreaNone textAreaAction = iota
	textAreaSave
	textAreaCancel
)

func newTextArea(out io.Writer, title, target string, initial []byte) *textArea {
	buffer := []rune(string(initial))
	a := &textArea{out: out, title: title, target: target, buf: buffer, cursor: len(buffer), rows: 10, width: 78}
	return a
}

func (a *textArea) SetSize(width, height int) {
	if width >= 32 {
		if width > 100 {
			width = 100
		}
		a.width = width - 2
	}
	if height > 12 {
		a.rows = height - 10
	} else {
		a.rows = 3
	}
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

func (a *textArea) Draw(p palette, unicode bool) {
	if a.originSaved {
		fmt.Fprint(a.out, "\033[u\033[J")
	} else {
		fmt.Fprint(a.out, "\r\033[s\033[J")
		a.originSaved = true
	}
	left, right, horiz, top, mid, bottom := "|", "|", "-", "+", "+", "+"
	if unicode {
		left, right, horiz, top, mid, bottom = "│", "│", "─", "╭", "├", "╰"
	}
	line := func(l, r, value string) {
		value = truncateCells(value, a.width-2)
		pad := a.width - 1 - visibleANSIRunes(value)
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

	lines := strings.Split(string(a.buf), "\n")
	row, col := a.cursorPosition()
	if row < a.top {
		a.top = row
	}
	if row >= a.top+a.rows {
		a.top = row - a.rows + 1
	}
	if a.top < 0 {
		a.top = 0
	}
	for i := 0; i < a.rows; i++ {
		idx := a.top + i
		content := "~"
		if idx < len(lines) {
			content = a.horizontalLine(lines[idx], idx == row, col)
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
	fmt.Fprintf(a.out, "%s%s%s\033[u", bottom, bar, bottom)
	// Move to the visible cursor position (two framing lines + title/target).
	visibleRow := row - a.top
	if visibleRow < 0 {
		visibleRow = 0
	}
	if visibleRow >= a.rows {
		visibleRow = a.rows - 1
	}
	visibleCol := col
	maxCol := a.width - 4
	if col >= maxCol {
		visibleCol = col - maxCol + 1
	}
	if visibleCol > maxCol {
		visibleCol = maxCol
	}
	fmt.Fprintf(a.out, "\033[4B\033[%dB\033[%dC", visibleRow, visibleCol+2)
}

func (a *textArea) horizontalLine(line string, cursorLine bool, col int) string {
	r := []rune(line)
	start := 0
	width := a.width - 4
	if cursorLine && col >= width {
		start = col - width + 1
	}
	end := start + width
	if end > len(r) {
		end = len(r)
	}
	if start > len(r) {
		start = len(r)
	}
	return string(r[start:end])
}

func (a *textArea) cursorPosition() (row, col int) {
	for i, r := range a.buf {
		if i >= a.cursor {
			break
		}
		if r == '\n' {
			row++
			col = 0
		} else {
			col++
		}
	}
	return row, col
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
	a.message = ""
}
func (a *textArea) backspace() {
	if a.cursor > 0 {
		a.buf = append(a.buf[:a.cursor-1], a.buf[a.cursor:]...)
		a.cursor--
		a.message = ""
	}
}
func (a *textArea) delete() {
	if a.cursor < len(a.buf) {
		a.buf = append(a.buf[:a.cursor], a.buf[a.cursor+1:]...)
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
	lines := strings.Split(string(a.buf), "\n")
	if target >= len(lines) {
		target = len(lines) - 1
	}
	idx := 0
	for i := 0; i < target; i++ {
		idx += len([]rune(lines[i])) + 1
	}
	lineLen := len([]rune(lines[target]))
	if col > lineLen {
		col = lineLen
	}
	a.cursor = idx + col
}

func truncateCells(s string, limit int) string {
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return s
}

func visibleANSIRunes(s string) int {
	visible, inEscape := 0, false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		visible++
	}
	return visible
}
