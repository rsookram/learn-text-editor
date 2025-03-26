package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const version string = "0.0.1"

const tabStop int = 8

const requiredQuitTimes int = 3

var quitTimes = requiredQuitTimes

var lastMatchLine = -1
var searchForward = true

type editorRow struct {
	idx    int
	raw    string
	render string
	// highlight contains values which correspond to each character in render
	// with information which indicates how the character should be highlighted.
	highlight      []editorHighlight
	hasOpenComment bool
}

type editorConfig struct {
	cx, cy int
	// rx is an index into the render field of a row
	rx int

	rowOffset int
	colOffset int

	screenRows, screenCols int

	row []editorRow

	dirty bool

	filename string

	statusMessage string
	statusTime    time.Time

	// syntax indicates what syntax highlighting should be applied to the loaded
	// file. nil means that there was no file type detected.
	syntax *editorSyntax
}

var e editorConfig

const (
	backspace rune = 127 // Technically DEL in ASCII

	arrowUp    rune = '↑'
	arrowDown  rune = '↓'
	arrowLeft  rune = '←'
	arrowRight rune = '→'

	pageUp   rune = '↟'
	pageDown rune = '↡'
	home     rune = '↞'
	end      rune = '↠'

	delete rune = '⌫'
)

func main() {
	defer func() {
		if err := recover(); err != nil {
			die(fmt.Sprintf("panic: %v\n\n%s", err, string(debug.Stack())))
		}
	}()

	err := enableRawInput()
	if err != nil {
		die(err.Error())
	}

	e, err = initEditor()
	if err != nil {
		die(err.Error())
	}

	if len(os.Args) >= 2 {
		editorOpen(os.Args[1])
	}

	editorSetStatusMessage("HELP: Ctrl-S = save | Ctrl-Q = quit | Ctrl-F = find")

	for {
		editorRefreshScreen()
		editorProcessKeypress()
	}
}

func enableRawInput() error {
	err := exec.Command("stty", "-F", "/dev/tty", "raw").Run()
	if err != nil {
		return err
	}

	// Do not display entered characters on the screen.
	err = exec.Command("stty", "-F", "/dev/tty", "-echo").Run()
	if err != nil {
		return err
	}

	// Time out read call after 100 ms of no input.
	err = exec.Command("stty", "-F", "/dev/tty", "min", "0", "time", "1").Run()
	if err != nil {
		return err
	}

	return nil
}

func initEditor() (editorConfig, error) {
	config := editorConfig{
		cx:        0,
		cy:        0,
		rx:        0,
		rowOffset: 0,
		colOffset: 0,
	}

	// Move to end of screen
	fmt.Print("\x1b[999C\x1b[999B")

	// Query terminal for status information
	fmt.Print("\x1b[6n\r\n")

	// bb will contain the values in the format (for 80x24):
	// \x1b[24;80R
	bb, err := io.ReadAll(os.Stdin)
	if err != nil {
		die(err.Error())
	}

	output := string(bb)

	var rows int
	var cols int
	n, err := fmt.Sscanf(output, "\x1b[%d;%dR", &rows, &cols)
	if n != 2 {
		return editorConfig{}, fmt.Errorf("failed to parse terminal dimensions, given %q", output)
	}
	if err != nil {
		return editorConfig{}, fmt.Errorf("failed to parse terminal dimensions: %w", err)
	}

	// Reserve one row for the status bar and one for the status message
	config.screenRows = rows - 2
	config.screenCols = cols

	return config, nil
}

func editorSave() {
	if e.filename == "" {
		e.filename = editorPrompt("Save as: %s", func(string, rune) {})
		if e.filename == "" {
			editorSetStatusMessage("Save aborted")
			return
		}

		editorSelectSyntaxHighlight()
	}

	toSave := editorRowsToString()

	// TODO: Write to temp file then rename to e.filename
	if err := os.WriteFile(e.filename, toSave, 0o644); err != nil {
		editorSetStatusMessage("Can't save! I/O error: %s", err.Error())
	} else {
		e.dirty = false
		editorSetStatusMessage("%d bytes written to disk", len(toSave))
	}
}

func editorRowsToString() []byte {
	var out bytes.Buffer
	for _, r := range e.row {
		out.WriteString(r.raw)
		out.WriteRune('\n')
	}

	return out.Bytes()
}

func editorFind() {
	savedCx := e.cx
	savedCy := e.cy
	savedColOffset := e.colOffset
	savedRowOffset := e.rowOffset

	query := editorPrompt("Search: %s (Use ESC/Arrows/Enter)", editorFindCallback)

	if query == "" { // cancelled search
		e.cx = savedCx
		e.cy = savedCy
		e.colOffset = savedColOffset
		e.rowOffset = savedRowOffset
	}
}

func editorFindCallback(query string, key rune) {
	clearSearchHighlight(e.row)

	if key == '\r' || key == '\x1b' {
		lastMatchLine = -1
		searchForward = true
		return
	} else if key == arrowRight || key == arrowDown {
		searchForward = true
	} else if key == arrowLeft || key == arrowUp {
		searchForward = false
	} else {
		lastMatchLine = -1
		searchForward = true
	}

	if lastMatchLine == -1 {
		searchForward = true
	}

	currentSearchLine := lastMatchLine

	for range len(e.row) {
		if searchForward {
			currentSearchLine++
		} else {
			currentSearchLine--
		}

		if currentSearchLine == -1 {
			currentSearchLine = len(e.row) - 1
		} else if currentSearchLine == len(e.row) {
			currentSearchLine = 0
		}

		row := e.row[currentSearchLine]
		idx := strings.Index(row.render, query)
		if idx >= 0 {
			lastMatchLine = currentSearchLine
			e.cy = currentSearchLine
			e.cx = editorRowRxToCx(row, idx)
			// Hack. Scroll to the bottom of the file so that the next refresh will
			// scroll the match into view.
			e.rowOffset = len(e.row)

			highlightSearchResult(row, query, idx)
			break
		}
	}
}

func editorOpen(path string) {
	e.filename = path

	editorSelectSyntaxHighlight()

	bb, err := os.ReadFile(path)
	if err != nil {
		die("ReadFile")
	}

	text := string(bb)

	for line := range strings.Lines(text) {
		editorInsertRow(len(e.row), strings.TrimSuffix(line, "\n"))
	}

	e.dirty = false
}

func editorInsertNewline() {
	if e.cx == 0 {
		editorInsertRow(e.cy, "")
	} else {
		row := &e.row[e.cy]
		editorInsertRow(e.cy+1, row.raw[e.cx:])

		row = &e.row[e.cy]
		row.raw = row.raw[:e.cx]

		editorUpdateRow(row)
	}

	e.cy++
	e.cx = 0
}

func editorInsertRow(at int, line string) {
	e.row = slices.Insert(e.row, at, editorRow{idx: at, raw: line, render: ""})

	for i := range e.row[at+1:] {
		e.row[at+1+i].idx++
	}

	editorUpdateRow(&e.row[at])

	e.dirty = true
}

func editorDelRow(at int) {
	if at < 0 || at >= len(e.row) {
		return
	}

	e.row = slices.Delete(e.row, at, at+1)
	for i := range e.row[at:] {
		e.row[at+i].idx--
	}

	e.dirty = true
}

func editorInsertChar(c rune) {
	if e.cy == len(e.row) {
		editorInsertRow(len(e.row), "")
	}
	editorRowInsertChar(&e.row[e.cy], e.cx, c)

	e.cx++
}

func editorRowInsertChar(row *editorRow, at int, c rune) {
	if at < 0 || at > len(row.raw) {
		at = len(row.raw)
	}

	var newRaw strings.Builder
	newRaw.Grow(len(row.raw) + utf8.RuneLen(c))

	newRaw.WriteString(row.raw[:at])
	newRaw.WriteRune(c)
	newRaw.WriteString(row.raw[at:])

	row.raw = newRaw.String()
	editorUpdateRow(row)

	e.dirty = true
}

func editorRowAppendString(row *editorRow, s string) {
	row.raw = row.raw + s
	editorUpdateRow(row)

	e.dirty = true
}

func editorDelChar() {
	if e.cy == len(e.row) || (e.cx == 0 && e.cy == 0) {
		return
	}

	row := &e.row[e.cy]
	if e.cx > 0 {
		editorRowDelChar(row, e.cx-1)
		e.cx--
	} else {
		// Deleting at the beginning of the line. Join the current line with the
		// previous one.
		e.cx = len(e.row[e.cy-1].raw)

		editorRowAppendString(&e.row[e.cy-1], row.raw)
		editorDelRow(e.cy)

		e.cy--
	}
}

func editorRowDelChar(row *editorRow, at int) {
	if at < 0 || at >= len(row.raw) {
		return
	}

	// TODO: What about deleting a multi-byte character?
	var newRaw strings.Builder
	newRaw.Grow(len(row.raw) - 1)

	newRaw.WriteString(row.raw[:at])
	newRaw.WriteString(row.raw[at+1:])

	row.raw = newRaw.String()

	editorUpdateRow(row)

	e.dirty = true
}

func editorUpdateRow(row *editorRow) {
	var render strings.Builder
	render.Grow(len(row.raw))

	// Replace tabs with spaces for rendering
	var idx int
	for _, ch := range row.raw {
		if ch == '\t' {
			render.WriteRune(' ')
			idx++

			// Append spaces until the next tab stop
			for ; idx%tabStop != 0; idx++ {
				render.WriteRune(' ')
			}
		} else {
			render.WriteRune(ch)
			idx++
		}
	}

	row.render = render.String()

	editorUpdateSyntax(row)
}

func editorProcessKeypress() {
	c := editorReadKey()

	switch c {
	case '\r': // enter
		editorInsertNewline()
		break
	case ctrl('q'):
		if e.dirty && quitTimes > 0 {
			editorSetStatusMessage(
				"WARNING!!! File has unsaved changes. Press Ctrl-Q %d more times to quit.",
				quitTimes,
			)
			quitTimes--
			return
		}

		// Clear out any partial output
		fmt.Print("\x1b[2J")
		fmt.Print("\x1b[H")
		os.Exit(0)
	case ctrl('s'):
		editorSave()
	case ctrl('f'):
		editorFind()
	case arrowUp, arrowDown, arrowLeft, arrowRight:
		editorMoveCursor(c)
	case pageUp, pageDown:
		if c == pageUp {
			e.cy = e.rowOffset
		} else if c == pageDown {
			e.cy = e.rowOffset + e.screenRows - 1
			e.cy = min(e.cy, len(e.row))
		}

		for range e.screenRows {
			if c == pageUp {
				editorMoveCursor(arrowUp)
			} else {
				editorMoveCursor(arrowDown)
			}
		}
	case home, ctrl('a'):
		e.cx = 0
	case end, ctrl('e'):
		if e.cy < len(e.row) {
			e.cx = len(e.row[e.cy].raw)
		}
	case backspace, ctrl('h'), delete:
		if c == delete {
			editorMoveCursor(arrowRight)
		}
		editorDelChar()
		break
	case '\x1b', ctrl('l'): // escape
		break
	default:
		editorInsertChar(c)
	}

	quitTimes = requiredQuitTimes
}

func editorReadKey() rune {
	c := []byte{0}
	for {
		_, err := os.Stdin.Read(c)
		if err == io.EOF {
			// This likely happened due to read timing out.
			continue
		}
		if err != nil {
			die(err.Error())
		}

		break
	}

	ch := rune(c[0])

	if ch != '\x1b' {
		return ch
	}

	// Read escape sequence
	seq := []byte{0, 0, 0}
	_, err := os.Stdin.Read(seq[0:1])
	if err != nil {
		return '\x1b'
	}
	_, err = os.Stdin.Read(seq[1:2])
	if err != nil {
		return '\x1b'
	}

	if seq[0] == '[' {
		if seq[1] >= '0' && seq[1] <= '9' {
			_, err = os.Stdin.Read(seq[2:3])
			if err != nil {
				return '\x1b'
			}

			if seq[2] == '~' {
				switch seq[1] {
				case '1', '7':
					return home
				case '4', '8':
					return end
				case '3':
					return delete
				case '5':
					return pageUp
				case '6':
					return pageDown
				}
			}
		} else {
			switch seq[1] {
			case 'A':
				return arrowUp
			case 'B':
				return arrowDown
			case 'C':
				return arrowRight
			case 'D':
				return arrowLeft
			case 'H':
				return home
			case 'F':
				return end
			}
		}
	} else if seq[0] == 'O' {
		switch seq[1] {
		case 'H':
			return home
		case 'F':
			return end
		}
	}

	return '\x1b'
}

func editorPrompt(prompt string, callback func(query string, key rune)) string {
	var buf strings.Builder

	for {
		editorSetStatusMessage(prompt, buf.String())
		editorRefreshScreen()

		c := editorReadKey()
		if c == backspace || c == 'h'&0b0001_1111 || c == delete {
			if buf.Len() > 0 {
				old := buf.String()
				buf.Reset()
				buf.WriteString(old[:len(old)-1])
			}
		} else if c == '\x1b' { // escape
			editorSetStatusMessage("")
			callback(buf.String(), c)
			return ""
		} else if c == '\r' {
			if buf.Len() > 0 {
				editorSetStatusMessage("")
				callback(buf.String(), c)
				return buf.String()
			}
		} else if c >= ' ' && c <= '~' { // if printable
			buf.WriteRune(c)
		}

		callback(buf.String(), c)
	}
}

func editorMoveCursor(key rune) {
	var row string
	if e.cy < len(e.row) {
		row = e.row[e.cy].raw
	}

	switch key {
	case arrowUp:
		if e.cy != 0 {
			e.cy--
		}
	case arrowLeft:
		if e.cx != 0 {
			e.cx--
		} else if e.cy > 0 {
			e.cy--
			e.cx = len(e.row[e.cy].raw)
		}
	case arrowDown:
		if e.cy < len(e.row) {
			e.cy++
		}
	case arrowRight:
		if e.cx < len(row) {
			e.cx++
		} else if e.cy < len(e.row) && e.cx == len(row) {
			e.cy++
			e.cx = 0
		}
	}

	// Ensure the cursor isn't past the end of the line after moving up / down to
	// a shorter line.
	if e.cy < len(e.row) {
		e.cx = min(e.cx, len(e.row[e.cy].raw))
	} else {
		e.cx = 0
	}
}

func editorRefreshScreen() {
	editorScroll()

	buf := bufio.NewWriter(os.Stdout)

	// Hide cursor
	fmt.Fprint(buf, "\x1b[?25l")
	// Move cursor to top left
	fmt.Fprint(buf, "\x1b[H")

	editorDrawRows(buf)
	editorDrawStatusBar(buf)
	editorDrawMessageBar(buf)

	// Move the cursor to the correct position
	fmt.Fprintf(buf, "\x1b[%d;%dH", (e.cy-e.rowOffset)+1, (e.rx-e.colOffset)+1)

	// Show cursor again
	fmt.Fprint(buf, "\x1b[?25h")

	buf.Flush()
}

func editorScroll() {
	e.rx = 0
	if e.cy < len(e.row) {
		e.rx = editorRowCxToRx(e.row[e.cy], e.cx)
	}

	if e.cy < e.rowOffset {
		e.rowOffset = e.cy
	}
	if e.cy >= e.rowOffset+e.screenRows {
		e.rowOffset = e.cy - e.screenRows + 1
	}
	if e.rx < e.colOffset {
		e.colOffset = e.rx
	}
	if e.rx >= e.colOffset+e.screenCols {
		e.colOffset = e.rx - e.screenCols + 1
	}
}

func editorRowCxToRx(row editorRow, cx int) int {
	rx := 0
	for i := range cx {
		if row.raw[i] == '\t' {
			rx += (tabStop - 1) - (rx % tabStop)
		}
		rx++
	}

	return rx
}

func editorRowRxToCx(row editorRow, rx int) int {
	curRx := 0
	for cx, ch := range row.raw {
		if ch == '\t' {
			curRx += (tabStop - 1) - (curRx % tabStop)
		}
		curRx++

		if curRx > rx {
			return cx
		}
	}

	return len(row.raw)
}

func editorDrawRows(w io.Writer) {
	for y := range e.screenRows {
		fileRow := y + e.rowOffset
		if fileRow >= len(e.row) {
			if len(e.row) == 0 && y == e.screenRows/3 {
				welcomeLabel := fmt.Sprintf("lte -- version %s", version)
				welcomeLabel = welcomeLabel[:min(len(welcomeLabel), e.screenCols)]

				padding := (e.screenCols - len(welcomeLabel)) / 2
				if padding > 0 {
					fmt.Fprint(w, "~")
					fmt.Fprint(w, strings.Repeat(" ", padding-1))
				}

				fmt.Fprint(w, welcomeLabel)
			} else {
				fmt.Fprint(w, "~")
			}
		} else {
			rowToDraw := e.row[fileRow].render
			highlights := e.row[fileRow].highlight
			if e.colOffset <= len(rowToDraw) {
				rowToDraw = rowToDraw[e.colOffset:]
				highlights = highlights[e.colOffset:]
			} else {
				rowToDraw = ""
			}
			rowToDraw = rowToDraw[:min(len(rowToDraw), e.screenCols)]

			currentColour := -1
			for i, ch := range rowToDraw {
				// TODO: Need a check that handles multi-byte characters
				if ch < ' ' || ch > '~' { // is non-printable
					sym := "?"
					if ch <= 26 {
						sym = string('A' - 1 + ch)
					}

					fmt.Fprint(w, "\x1b[7m")
					fmt.Fprint(w, sym)
					fmt.Fprint(w, "\x1b[m")
					if currentColour != -1 {
						fmt.Fprintf(w, "\x1b[%dm", currentColour)
					}
				} else if highlights[i] == highlightNormal {
					if currentColour != -1 {
						fmt.Fprint(w, "\x1b[39m")
						currentColour = -1
					}
				} else {
					colour := editorSyntaxToColour(highlights[i])
					if colour != currentColour {
						fmt.Fprintf(w, "\x1b[%dm", colour)
						currentColour = colour
					}
				}
				fmt.Fprint(w, string(ch))
			}

			fmt.Fprint(w, "\x1b[39m")
		}

		fmt.Fprint(w, "\x1b[K")
		fmt.Fprint(w, "\r\n")
	}
}

func editorDrawStatusBar(w io.Writer) {
	fmt.Fprint(w, "\x1b[7m")

	name := e.filename
	if e.filename == "" {
		name = "[No Name]"
	}

	isModified := ""
	if e.dirty {
		isModified = "(modified)"
	}

	status := fmt.Sprintf("%.20s - %d lines %s", name, len(e.row), isModified)
	status = status[:min(len(status), e.screenCols)]

	fileType := "no ft"
	if e.syntax != nil {
		fileType = e.syntax.fileType
	}
	rightStatus := fmt.Sprintf("%s | %d/%d", fileType, e.cy+1, len(e.row))

	fmt.Fprint(w, status)
	fmt.Fprint(w, strings.Repeat(" ", e.screenCols-len(status)-len(rightStatus)))
	fmt.Fprint(w, rightStatus)

	fmt.Fprint(w, "\x1b[m")
	fmt.Fprint(w, "\r\n")
}

func editorDrawMessageBar(w io.Writer) {
	fmt.Fprint(w, "\x1b[K")

	if e.statusTime.Add(time.Second * 5).After(time.Now()) {
		message := e.statusMessage
		message = message[:min(len(message), e.screenCols)]

		fmt.Fprint(w, message)
	}
}

func editorSetStatusMessage(format string, a ...any) {
	e.statusMessage = fmt.Sprintf(format, a...)
	e.statusTime = time.Now()
}

// ctrl returns the rune that is provided as input when the corresponding key
// is pressed while the ctrl key is held.
//
// For example, `ctrl('a')` is the input seen when ctrl-A is pressed.
func ctrl(c rune) rune {
	return c & 0b0001_1111
}

func die(s any) {
	// Clear out any partial output
	fmt.Print("\x1b[2J")
	fmt.Print("\x1b[H")

	// Restore normal printing
	exec.Command("stty", "-F", "/dev/tty", "-raw").Run()

	fmt.Fprintln(os.Stderr, s)
	os.Exit(1)
}
