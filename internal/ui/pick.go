package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/devvmhq/devvm/internal/msg"
)

// Choice is one of the items Pick shows: a name, what it is, and whether
// it's chosen. A Fixed one is shown greyed out as it is: the cursor skips
// it, and it can't be toggled.
type Choice struct {
	Name, Desc string
	On         bool
	Fixed      bool
}

// Section is a heading and the choices under it, as Pick shows them. A
// section whose Heading is the zero message has no heading.
type Section struct {
	Heading msg.M
	Choices []Choice
}

// rawTerminal puts the terminal devvm reads from into raw mode, returning
// what undoes it.
func rawTerminal() (restore func(), err error) {
	old, err := term.MakeRaw(fd(os.Stdin))
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(fd(os.Stdin), old) }, nil
}

// Pick shows the sections' choices under help, each section under its
// heading, and lets the user toggle them: up and down (or k and j) move
// over every choice but the fixed ones, space toggles, enter confirms. It
// returns the sections as chosen. Without a terminal to read from and
// print to, it returns them as given. Ctrl+C ends the run as interrupted.
// Where the picker can show (CanPick) but the terminal won't go into raw
// mode or goes away while it's open (pick_failed), or is too short for
// the list (pick_too_small), it returns why, with the choices as given
// or as they were then: a caller that mustn't go on with them refuses.
func (u *UI) Pick(help msg.M, sections []Section) ([]Section, error) {
	out := make([]Section, len(sections))
	for i, s := range sections {
		out[i] = Section{Heading: s.Heading, Choices: slices.Clone(s.Choices)}
	}
	return out, u.pick(help, out, false)
}

// PickOne shows the choices under help and lets the user choose one: up
// and down (or k and j) move the choice, enter confirms. It starts from
// the first that's on and returns the index of the one chosen. Without a
// terminal to read from and print to, that's the one it started from.
// Ctrl+C ends the run as interrupted.
func (u *UI) PickOne(help msg.M, choices []Choice) int {
	out := slices.Clone(choices)
	start := max(slices.IndexFunc(out, func(c Choice) bool { return c.On }), 0)
	for i := range out {
		out[i].On = i == start
	}
	_ = u.pick(help, []Section{{Choices: out}}, true)
	return max(slices.IndexFunc(out, func(c Choice) bool { return c.On }), 0)
}

// pick runs pickLoop on the terminal, the sections' choices changed in
// place; where it isn't shown (CanPick) it leaves them as they are, and
// where the terminal won't go into raw mode it says why.
func (u *UI) pick(help msg.M, sections []Section, one bool) error {
	n := 0
	for _, s := range sections {
		n += len(s.Choices)
	}
	if n == 0 || !u.CanPick() {
		return nil
	}
	p := newPicking(sections, one)
	p.width, p.height = u.termSize()
	text := u.outStyle.message(help)
	p.reserve = 1 // the line the cursor is left on
	for l := range strings.SplitSeq(text, "\n") {
		p.reserve++
		if p.width > 0 {
			p.reserve += (runes(l) - 1) / p.width // a line that wraps
		}
	}
	// A window too short to move in, with its lines above and below,
	// can't redraw over itself: refused, rather than leave stale lines.
	if room := p.height - p.reserve; p.height > 0 && room < pickMinRoom && len(p.lines()) > room {
		return msg.PickTooSmall(strconv.Itoa(p.height))
	}
	p.more = func(n int) string { return u.outStyle.line(msg.PickMore(n)) }
	defer holdStatus(u)()
	restore, err := u.rawInput()
	if err != nil {
		return msg.PickFailed(err.Error()).Because(err)
	}
	// In raw mode a line ends with \r\n; the cursor is hidden meanwhile.
	u.write(u.stdout, strings.ReplaceAll(text, "\n", "\r\n")+"\r\n\033[?25l")
	ok, err := u.pickLoop(u.stdin, p)
	u.write(u.stdout, "\033[?25h")
	restore()
	switch {
	case err != nil:
		u.write(u.stdout, "\r\n")
		return msg.PickFailed(err.Error()).Because(err)
	case !ok:
		u.write(u.stderr, "\n")
		u.interrupted.Store(true)
		exit(InterruptedCode)
	}
	return nil
}

// pickMinRoom is the fewest lines a list that scrolls needs below its
// help: a row, and the lines above and below that say how many more.
const pickMinRoom = 3

// picking is what pickLoop draws and moves over: every choice in order,
// and the ones the cursor can reach.
type picking struct {
	sections []Section
	rows     []*Choice
	movable  []int // indexes into rows
	one      bool
	// width and height are the terminal's, 0 when unknown. Each line is
	// cut to the width, so it takes one line on the screen, and when the
	// lines don't fit in the height less reserve (the help above them and
	// the cursor's line below), they scroll in a window, with a line above
	// and below that says how many more there are (more).
	width, height, reserve int
	more                   func(n int) string
	top                    int // the first line the window shows
}

// pickLine is one line of the picker: a section's heading, or the row of
// that index in rows (-1 for a heading).
type pickLine struct {
	heading msg.M
	row     int
}

func newPicking(sections []Section, one bool) *picking {
	p := &picking{sections: sections, one: one, more: func(n int) string { return strconv.Itoa(n) + " more" }}
	for i := range sections {
		cs := sections[i].Choices
		for j := range cs {
			c := &cs[j]
			if !c.Fixed {
				p.movable = append(p.movable, len(p.rows))
			}
			p.rows = append(p.rows, c)
		}
	}
	return p
}

// lines are the picker's lines, in order.
func (p *picking) lines() []pickLine {
	var out []pickLine
	row := 0
	for _, sec := range p.sections {
		if sec.Heading.ID() != "" {
			out = append(out, pickLine{heading: sec.Heading, row: -1})
		}
		for range sec.Choices {
			out = append(out, pickLine{row: row})
			row++
		}
	}
	return out
}

// nameWidth is the names' column: 10, or the longest name's, such as a
// host's, but at most half the width, so a description still shows.
func (p *picking) nameWidth() int {
	w := 10
	for _, c := range p.rows {
		w = max(w, runes(msg.OneLine(c.Name)))
	}
	if p.width > 0 {
		w = min(w, max(10, (p.width-8)/2))
	}
	return w
}

// span is how many of n lines the window shows, all of them when they fit
// (or the height is unknown), and whether they scroll.
func (p *picking) span(n int) (int, bool) {
	room := p.height - p.reserve
	if p.height <= 0 || n <= room {
		return n, false
	}
	return max(1, room-2), true
}

// draw writes the headings and rows, the cursor at the movable row cur
// (-1 for none), and returns how many lines it wrote: always as many for
// one picker, so a redraw moves back over them all and leaves none stale.
func (p *picking) draw(s style, cur int) (string, int) {
	all := p.lines()
	span, scrolls := p.span(len(all))
	if !scrolls {
		p.top = 0
	} else if cur >= 0 {
		at := slices.IndexFunc(all, func(l pickLine) bool { return l.row == p.movable[cur] })
		// The heading above the first row shows with it.
		if at == 1 && all[0].row < 0 {
			at = 0
		}
		p.top = min(max(p.top, at-span+1), at)
	}
	var b strings.Builder
	line := func(text string) { b.WriteString("\033[2K" + text + "\r\n") }
	if scrolls {
		line(p.moreLine(s, "↑", p.top))
	}
	for _, l := range all[p.top : p.top+span] {
		if l.row < 0 {
			line(s.paint(sgrBold, cut(msg.OneLine(s.line(l.heading)), p.width)))
			continue
		}
		line(p.row(s, l.row, cur))
	}
	if !scrolls {
		return b.String(), span
	}
	line(p.moreLine(s, "↓", len(all)-p.top-span))
	return b.String(), span + 2
}

// moreLine says how many lines are past the window on one side, arrow's;
// empty when none are.
func (p *picking) moreLine(s style, arrow string, n int) string {
	if n <= 0 {
		return ""
	}
	return s.paint(sgrDim, cut("  "+arrow+" "+p.more(n), p.width))
}

// row is the line of rows[i], the cursor at the movable row cur: its
// pointer, its mark, its name in the names' column, cut from the left so
// its end, a host's domain, shows, and its description, the line cut to
// the width.
func (p *picking) row(s style, i, cur int) string {
	open, on, closed := "[", "x", "]"
	if p.one {
		open, on, closed = "(", "*", ")"
	}
	c := p.rows[i]
	mark, ptr := " ", " "
	if c.On {
		mark = on
	}
	if cur >= 0 && p.movable[cur] == i {
		ptr = ">"
	}
	w := p.nameWidth()
	name := msg.OneLine(c.Name)
	if r := []rune(name); len(r) > w {
		name = "…" + string(r[len(r)-w+1:])
	}
	text := cut(fmt.Sprintf("%s %s%s%s %s %s", ptr, open, mark, closed, pad(name, w), msg.OneLine(c.Desc)), p.width)
	if c.Fixed {
		return s.paint(sgrDim, text)
	}
	return text
}

// runes is how many runes s has: its columns, for the text pickers show.
func runes(s string) int { return len([]rune(s)) }

// pad fills s with spaces to w runes.
func pad(s string, w int) string { return s + strings.Repeat(" ", max(0, w-runes(s))) }

// cut cuts s to fit a line of width columns, one left for the cursor, and
// ends it in … when it's cut; 0 is any width.
func cut(s string, width int) string {
	r := []rune(s)
	if width <= 0 || len(r) < width {
		return s
	}
	return string(r[:max(0, width-2)]) + "…"
}

// pickLoop draws p's choices and reads keys until enter (or the end of
// the input), toggling choices in place, or with one, moving the one
// that's on with the cursor. It's false on Ctrl+C, and the read's error
// when the terminal went away, which confirms nothing.
func (u *UI) pickLoop(in *bufio.Reader, p *picking) (bool, error) {
	one := p.one
	n := len(p.movable)
	cur := -1
	if n > 0 {
		cur = 0
	}
	if one && n > 0 {
		cur = max(slices.IndexFunc(p.movable, func(r int) bool { return p.rows[r].On }), 0)
	}
	for {
		out, lines := p.draw(u.outStyle, cur)
		u.write(u.stdout, out)
		key, err := in.ReadByte()
		switch {
		case errors.Is(err, io.EOF):
			return true, nil // the input's end confirms, as Enter does
		case err != nil:
			// The terminal went away (EIO on a hang-up): nothing confirmed.
			return false, err
		}
		switch key {
		case 0x03: // Ctrl+C: raw mode turns it into input
			return false, nil
		case '\r', '\n':
			return true, nil
		case ' ':
			if !one && cur >= 0 {
				c := p.rows[p.movable[cur]]
				c.On = !c.On
			}
		case 'a':
			if !one {
				p.toggleAll()
			}
		case 'k':
			cur = step(cur, n, -1)
		case 'j':
			cur = step(cur, n, 1)
		case 0x1b: // an arrow: ESC [ A or ESC [ B
			if seq, err := in.Peek(2); err == nil && seq[0] == '[' {
				switch seq[1] {
				case 'A':
					cur = step(cur, n, -1)
				case 'B':
					cur = step(cur, n, 1)
				}
				_, _ = in.Discard(2)
			}
		}
		if one {
			for i, r := range p.movable {
				p.rows[r].On = i == cur
			}
		}
		u.write(u.stdout, fmt.Sprintf("\033[%dA", lines))
	}
}

// toggleAll chooses every row the cursor can reach, or when they're all
// chosen already, unchooses them all.
func (p *picking) toggleAll() {
	all := !slices.ContainsFunc(p.movable, func(r int) bool { return !p.rows[r].On })
	for _, r := range p.movable {
		p.rows[r].On = !all
	}
}

// step moves the cursor cur by d over n rows, wrapping; -1 (none) stays.
func step(cur, n, d int) int {
	if cur < 0 {
		return cur
	}
	return (cur + n + d) % n
}
