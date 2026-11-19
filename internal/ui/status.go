package ui

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/devvmhq/devvm/internal/msg"
)

// In a terminal, a status line shows what devvm is waiting for: the step,
// for how long, and as much of the run log's latest line as fits. It's
// redrawn every redrawEvery, at the terminal's width then.
const redrawEvery = 200 * time.Millisecond

// A step that took at least this long leaves a line behind ("✓ step (5s)").
const doneAfter = 3 * time.Second

// A statusLine is one slow step's line. The UI keeps them as a stack,
// innermost last, and shows only that one: a step inside another (a
// logged command inside a Status) shows its own line until it ends, then
// the outer one's comes back.
type statusLine struct {
	u       *UI
	label   []part // its values bold
	start   time.Time
	stopped chan struct{}
	ended   chan struct{}
	once    sync.Once
	frame   int // under the UI's outMu
}

// statusState is what the terminal shows of the status lines, under the
// UI's outMu, which every write to the terminal holds: a message clears
// the line, prints, then draws it again under itself (writeTo), and a
// question takes it off until it's answered (holdStatus).
type statusState struct {
	lines []*statusLine // the stack, innermost last
	shown bool          // a line is on the terminal
	drawn int           // the runes it has
	// held counts the questions and pickers on the terminal: no line
	// meanwhile, as one would draw over what's typed.
	held int
	// partial: the last write to the terminal didn't end its line, so a
	// line drawn now would clear it; none until one ends.
	partial bool
}

// Status shows label on a status line, around slow work that isn't one
// logged command, until stop is called. Only in a terminal, not while
// verbose, and not once the run is interrupted, as nothing would clear
// a line started after the run's last stop (Main).
func (u *UI) Status(label msg.M) (stop func()) {
	if verbose(u) || !u.stderrTTY || u.Interrupted() {
		return func() {}
	}
	s := u.startStatus(u.errStyle.valueParts(label))
	return s.stop
}

// startStatus shows label on a status line over any showing, until its
// stop.
func (u *UI) startStatus(label []part) *statusLine {
	s := &statusLine{u: u, label: label, start: time.Now(), stopped: make(chan struct{}), ended: make(chan struct{})}
	u.outMu.Lock()
	u.status.lines = append(u.status.lines, s)
	drawStatus(u)
	u.outMu.Unlock()
	go s.run()
	return s
}

// stopStatus stops every status line showing, the innermost first.
func (u *UI) stopStatus() {
	u.outMu.Lock()
	lines := slices.Clone(u.status.lines)
	u.outMu.Unlock()
	for _, s := range slices.Backward(lines) {
		s.stop()
	}
}

// run redraws s every redrawEvery while it's the innermost line.
func (s *statusLine) run() {
	defer close(s.ended)
	t := time.NewTicker(redrawEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case <-t.C:
			s.u.outMu.Lock()
			if l := s.u.status.lines; l[len(l)-1] == s {
				drawStatus(s.u)
			}
			s.u.outMu.Unlock()
		}
	}
}

// stop takes s off the stack; when it showed, it's cleared and the line
// under it, if any, drawn again.
func (s *statusLine) stop() {
	s.once.Do(func() {
		close(s.stopped)
		<-s.ended
		u := s.u
		u.outMu.Lock()
		defer u.outMu.Unlock()
		i := slices.Index(u.status.lines, s)
		if i < 0 {
			return
		}
		top := i == len(u.status.lines)-1
		u.status.lines = slices.Delete(u.status.lines, i, i+1)
		if top {
			clearStatus(u)
			drawStatus(u)
		}
	})
}

// drawStatus draws the innermost status line, over the one shown, unless
// none is, a question holds them, or a write left its line unended. It
// holds outMu.
func drawStatus(u *UI) {
	st := &u.status
	if len(st.lines) == 0 || st.held > 0 || st.partial {
		return
	}
	s := st.lines[len(st.lines)-1]
	frames := glyphs(u.getenv)[1:]
	cols := u.width()
	secs := int(time.Since(s.start).Seconds())
	_, log := statusText(plainText(s.label), secs, lastLine(u.logPath()), cols)
	parts := append([]part{{"    " + frames[s.frame%len(frames)] + " ", ""}}, s.label...)
	parts = append(parts, part{" " + duration(secs), ""}, part{log, sgrDim})
	line, drawn := u.errStyle.fit(parts, cols-1)
	s.frame++
	prev := 0
	if st.shown {
		prev = st.drawn
	}
	// No line wrapping while it draws: a resize between reading the width
	// and drawing would wrap.
	_, _ = io.WriteString(u.stderr, clearLine(prev, cols)+"\033[?7l"+line+"\033[?7h")
	st.shown, st.drawn = true, drawn
}

// clearStatus clears the status line shown, if one is. It holds outMu.
func clearStatus(u *UI) {
	if u.status.shown {
		_, _ = io.WriteString(u.stderr, clearLine(u.status.drawn, u.width()))
		u.status.shown = false
	}
}

// holdStatus takes the status line off the terminal until release, for a
// question, the picker or a command on the terminal: a line drawn
// meanwhile would go over what's typed.
func holdStatus(u *UI) (release func()) {
	u.outMu.Lock()
	u.status.held++
	clearStatus(u)
	u.outMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			u.outMu.Lock()
			u.status.held--
			drawStatus(u)
			u.outMu.Unlock()
		})
	}
}

// writeTo writes s to w, holding outMu. On the terminal the status line
// is on, it clears the line first and draws it again after, so a
// message never lands on it; while a question holds it, the status line
// stays off.
func writeTo(u *UI, w io.Writer, s string) (int, error) {
	onTerm := w == u.stderr || u.stdoutTTY && w == u.stdout
	if onTerm {
		clearStatus(u)
	}
	n, err := io.WriteString(w, s)
	if onTerm && s != "" && u.status.held == 0 {
		u.status.partial = !strings.HasSuffix(s, "\n")
	}
	if onTerm {
		drawStatus(u)
	}
	return n, err
}

// width is the terminal's width, 80 when unknown (some terminals say 0).
func (u *UI) width() int {
	if w := u.termWidth(); w > 0 {
		return w
	}
	return 80
}

// glyphs returns the done mark, then the spinner's frames, in the
// terminal's character set.
func glyphs(getenv func(string) string) []string {
	charset := getenv("LC_ALL")
	if charset == "" {
		charset = getenv("LC_CTYPE")
	}
	if charset == "" {
		charset = getenv("LANG")
	}
	if strings.Contains(strings.ToLower(charset), "utf") {
		return strings.Fields("✓ ⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏")
	}
	return []string{"ok", "|", "/", "-", `\`}
}

// doneLine is what a step that took a while leaves behind, its mark green
// and its label's values bold in a terminal.
func (u *UI) doneLine(label []part, secs int) string {
	l, _ := u.errStyle.fit(label, len(plainText(label)))
	return "    " + u.errStyle.paint(sgrDone, glyphs(u.getenv)[0]) + " " + l + " (" + duration(secs) + ")\n"
}

// duration is 5s, or 2m05s from a minute.
func duration(secs int) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

// statusText is the label and time, then apart, so it can be dimmed, as
// much of the log's latest line (its last carriage-return segment, without
// control characters) as fits in cols, after two spaces.
func statusText(label string, secs int, logLine string, cols int) (text, log string) {
	text = label + " " + duration(secs)
	if i := strings.LastIndexByte(logLine, '\r'); i >= 0 {
		logLine = logLine[i+1:]
	}
	logLine = msg.OneLine(logLine)
	room := cols - utf8.RuneCountInString(text) - 9
	if room >= 10 && logLine != "" {
		log = "  " + truncate(logLine, room)
	}
	return text, log
}

func truncate(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// clearLine clears the status line. A terminal made narrower rewraps the
// line already drawn (most do) onto more rows, so it clears those above too.
func clearLine(drawn, cols int) string {
	if cols <= 0 {
		cols = 80
	}
	up := 0
	if drawn > cols {
		up = (drawn - 1) / cols
	}
	return "\r\033[K" + strings.Repeat("\033[A\033[K", up)
}

// tail returns up to the last size bytes of the file at path.
func tail(path string, size int64) string {
	f, err := os.Open(path) //nolint:gosec // the run's own log
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > size {
		if _, err := f.Seek(-size, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

// lastLine is the file's last line, "" when it has none.
func lastLine(path string) string {
	if path == "" {
		return ""
	}
	s := strings.TrimSuffix(tail(path, 4096), "\n")
	return s[strings.LastIndexByte(s, '\n')+1:]
}

// tailLines is the file's last n lines.
func tailLines(path string, n int) string {
	lines := strings.SplitAfter(strings.TrimSuffix(tail(path, 64<<10), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	s := strings.Join(lines, "")
	if s != "" {
		s += "\n"
	}
	return s
}
