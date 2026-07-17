// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package progress renders build progress to the terminal.
//
// It shows a docker-build-like view: each phase of the build is a step header
// ("=> fetching source (git)"), and for long-running subprocesses (git fetch,
// make) the most recent lines of their output are shown as a dimmed rolling
// tail beneath the current step. Completed steps remain on screen, dimmed with
// a ✓ (or ✗ on failure), so the whole build history stays visible.
//
// Two implementations back the [Reporter] interface:
//   - tty:   a live inline TUI (bubbletea) used when the output is a terminal.
//   - plain: line-by-line passthrough used otherwise (CI logs, pipes, test
//     buffers), so output stays readable when there is no TTY to redraw on.
//
// The Reporter is an [io.Writer]: subprocess stdout/stderr written to it feed
// the current step's tail (tty) or are passed through verbatim (plain). This
// lets it slot directly into the build orchestrator's existing
// MakeRunner.Run(dir, env, args, stdout, stderr) seam without restructuring.
//
// The spinner and the scrolling tail are the ready-to-use components from
// charmbracelet/bubbles: [spinner] for the in-progress indicator and
// [viewport] for the rolling tail. The viewport owns the fiddly parts the TUI
// used to hand-roll — width-aware truncation (ansi.StringWidth/ansi.Cut,
// CJK-correct), a fixed-height frame (so the inline renderer never triggers a
// full clear+redraw as lines stream in), and bottom-pinning (GotoBottom) so the
// latest output is always visible.
//
// In TTY mode the reporter also tees the full, raw subprocess output — plus the
// step/detail/done markers, in the same plain format as the non-TTY reporter —
// to a log file, so a failed build can point the user at the complete
// transcript. The live TUI only retains the last 50 captured lines, and a
// kernel make error is often buried hundreds of lines above that. The log is
// kept only on failure (removed on success) and its path is printed in the
// post-TUI failure dump.
package progress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironcore-dev/kbake/internal/xbufio"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
)

type contextKeyType uint8

const (
	contextKey = contextKeyType(0)
)

type discardReporter struct {
}

func (discardReporter) Write(p []byte) (n int, err error) {
	return len(p), nil
}

func (discardReporter) Step(name string) {
}

func (discardReporter) Detail(s string) {
}

func (discardReporter) Done(err error) {
}

func (discardReporter) Close() error {
	return nil
}

func ReporterFromContext(ctx context.Context) Reporter {
	if rep, ok := ctx.Value(contextKey).(Reporter); ok {
		return rep
	}
	return discardReporter{}
}

func NewContext(ctx context.Context, rep Reporter) context.Context {
	return context.WithValue(ctx, contextKey, rep)
}

// tailLines is the maximum number of recent output lines the tty view keeps
// beneath the current step header (the live, rolling tail).
const tailLines = 15

// stepLineCap is the maximum number of lines captured per step for the failure
// dump. It is well above tailLines so a failed step's real error output (e.g.
// a compiler error buried above the 15-line live tail) is retained even though
// the live view only shows the last 15.
const stepLineCap = 200

// failDumpLines is the maximum number of captured lines printed in the
// post-TUI failure dump.
const failDumpLines = 50

// Styling for the progress view. Colors use lipgloss's predefined 4-bit
// constants (lipgloss.Green / Red / Cyan); the styles are package-level so
// they're allocated once. Rendering goes through these instead of raw ANSI
// escapes so the intent ("dim this", "green ✓") is legible at the call site.
var (
	// faintStyle dims text — the rolling tail, the finished-step line, and the
	// detail/elapsed labels. It is also the viewport's per-line style hook so
	// the viewport's width-truncation (ansi.Cut) keeps the SGR codes balanced
	// even when a line is cut mid-content.
	faintStyle = lipgloss.NewStyle().Faint(true)
	// spinnerStyle colors the running step's spinner cyan.
	spinnerStyle = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
	// successMark colors the ✓ on a completed step green (left bright, not
	// faint, so the status pops against the dimmed line).
	successMark = lipgloss.NewStyle().Foreground(lipgloss.Green)
	// failMark colors the ✗ on a failed step red.
	failMark = lipgloss.NewStyle().Foreground(lipgloss.Red)
)

// Reporter renders build progress and absorbs subprocess output. It is an
// [io.Writer]: bytes written to it become the current step's tail.
type Reporter interface {
	io.Writer
	// Step begins a new phase, finalizing the previous one (if any). The
	// name is shown verbatim as the step header.
	Step(name string)
	// Detail updates the current step's sub-phase label (e.g. "checking out
	// abc123…"), giving feedback during otherwise-silent operations.
	Detail(s string)
	// Done finalizes the current step with a success/failure status derived
	// from err (nil = success).
	Done(err error)
}

func MultiReporter(reps ...Reporter) Reporter {
	allReporters := make([]Reporter, 0, len(reps))
	for _, rep := range reps {
		if mr, ok := rep.(*multiReporter); ok {
			allReporters = append(allReporters, mr.reporters...)
		} else {
			allReporters = append(allReporters, rep)
		}
	}
	return &multiReporter{allReporters}
}

type multiReporter struct {
	reporters []Reporter
}

func (m *multiReporter) Write(p []byte) (n int, err error) {
	for _, rep := range m.reporters {
		n, err = rep.Write(p)
		if err != nil {
			return n, err
		}
		if n != len(p) {
			return n, io.ErrShortWrite
		}
	}
	return len(p), nil
}

func (m *multiReporter) Step(name string) {
	for _, rep := range m.reporters {
		rep.Step(name)
	}
}

func (m *multiReporter) Detail(s string) {
	for _, rep := range m.reporters {
		rep.Detail(s)
	}
}

func (m *multiReporter) Done(err error) {
	for _, rep := range m.reporters {
		rep.Done(err)
	}
}

// New returns a Reporter that writes to w. If w is a terminal a live inline TUI
// is used; otherwise output is plain (passthrough) so it stays readable in CI
// logs, pipes, and test buffers. cancel, when non-nil, is wired into the TUI
// model so Ctrl+C / "q" / esc aborts the build (cancelling the orchestrator's
// context, which kills any running make); it is ignored by the plain reporter.
//
// In TTY mode, logPath enables a full-transcript log file teed alongside the
// live TUI: every raw byte written to the reporter (plus step/detail/done
// markers) lands there, so a failed build can point the user at the complete
// output — the live TUI only retains the last 50 captured lines, and a kernel
// make error is often buried far above that. The file is created (with its
// parent dir) and kept only on failure (its path is printed in the post-TUI
// dump); a successful build removes it. An empty logPath disables the log. The
// plain reporter ignores it: non-TTY output already captures everything
// verbatim, so a separate file would be redundant.
func New(w io.Writer, cancel context.CancelFunc) (Reporter, func() error) {
	if f, ok := w.(*os.File); ok && term.IsTerminal(f.Fd()) {
		r := newTTYReporter(w, cancel)
		return r, r.Close
	}
	return newPlainReporter(w), func() error { return nil }
}

type FileReporter struct {
	file *os.File
	Reporter
}

func NewFileReporter(f *os.File) *FileReporter {
	return &FileReporter{
		file:     f,
		Reporter: newPlainReporter(f),
	}
}

func (f *FileReporter) Close() error {
	return errors.Join(
		f.file.Sync(),
		f.file.Close(),
	)
}

// --- messages for the tty model ---

type stepMsg struct{ name string }
type detailMsg struct{ s string }
type lineMsg struct{ line string }

// doneMsg finalises the current step. A nil err means success; a non-nil err
// is shown alongside the step so the user can see *why* a build phase failed
// (the orchestrator also returns it, but surfacing it in the TUI itself keeps
// the failure visible while the program shuts down).
type doneMsg struct{ err error }

// status of a step.
const (
	stRunning = iota
	stDone
	stFail
)

// stepState is one phase of the build. The tty view keeps the whole history so
// completed steps stay visible (dimmed) while later steps run.
type stepState struct {
	name   string
	status int
	detail string
	// errText holds the failure reason when status == stFail, so the ✗ line
	// shows the cause (e.g. "make defconfig: exit status 2").
	errText string
	started time.Time
	// elapsed is frozen when the step is finalized (Done); while running it is
	// computed as time.Since(started) on each render.
	elapsed time.Duration
}

// model is the bubbletea model for the tty progress view.
type model struct {
	spinner spinner.Model
	steps   []stepState
	// viewport renders the running step's rolling tail. It owns width-aware
	// truncation, the fixed-height frame, and bottom-pinning — the parts the
	// View used to compute by hand.
	viewport viewport.Model
	// lines is the running step's tail content, capped at tailLines. It is the
	// source of truth fed to the viewport via SetContentLines on each new
	// line; the viewport holds a copy for rendering.
	lines []string
	// width/height are the terminal dimensions, learned via WindowSizeMsg.
	// height bounds the tail so the inline frame never exceeds the visible
	// window (otherwise the terminal scrolls and the inline renderer can't
	// erase lines that scrolled into scrollback — they pile up). Defaults are
	// used until the first WindowSizeMsg arrives.
	width, height int
	// cancel, when non-nil, is invoked on Ctrl+C / "q" / esc so the build is
	// aborted (it cancels the orchestrator's context, which kills any running
	// make via exec.CommandContext) before the program quits and restores the
	// terminal.
	cancel context.CancelFunc
}

func newModel(cancel context.CancelFunc) model {
	m := model{
		spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		cancel:  cancel,
		width:   80,
		height:  24,
	}
	// The tail is a passive rolling view of the most recent output lines:
	//   - SoftWrap off (default): long lines are truncated to the width
	//     (CJK-correct via ansi.StringWidth/ansi.Cut) rather than wrapped, so
	//     each row shows one distinct line — a tail of N rows shows N lines,
	//     not one wrapped line eating the whole window.
	//   - LeftGutterFunc renders the 2-space indent; the viewport accounts for
	//     it in its width math (content is cut to width-2).
	//   - StyleLineFunc dims every line; the viewport applies the style after
	//     truncation so the SGR stays balanced.
	// The Height is recomputed from the remaining window on each step/resize
	// (see tailHeight); the viewport's constant Height makes the frame area
	// stable, so the inline renderer never does a full clear+redraw as lines
	// stream in.
	//
	// The width is deliberately one column less than the terminal (see
	// tailWidth): the viewport pads every rendered line — content and the blank
	// rows that fill the fixed frame height — to its width via lipgloss, so a
	// viewport width equal to the terminal width makes every line end in the
	// last column and the cursor land in the phantom (pending-autowrap) column.
	m.viewport = viewport.New(viewport.WithWidth(m.tailWidth()))
	m.viewport.LeftGutterFunc = func(viewport.GutterContext) string { return "  " }
	m.viewport.StyleLineFunc = func(int) lipgloss.Style { return faintStyle }
	m.viewport.SetHeight(m.tailHeight())
	return m
}

func (m model) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Track the terminal size so the tail can be capped to the visible
		// window and the viewport can truncate lines to the width.
		m.width, m.height = msg.Width, msg.Height
		m.viewport.SetWidth(m.tailWidth())
		m.resizeTail()
		return m, nil
	case tea.KeyPressMsg:
		// In raw mode Ctrl+C arrives as a keypress rather than SIGINT, so the
		// model must handle it: cancel the build (kills make) and quit, which
		// lets Run return and restore the terminal. Keypresses are NOT forwarded
		// to the viewport — the tail is a passive view, not a scrollable pane.
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case stepMsg:
		// Finalize a previous still-running step (shouldn't happen — Done is
		// called explicitly — but be defensive).
		if n := len(m.steps); n > 0 && m.steps[n-1].status == stRunning {
			m.steps[n-1].status = stDone
			m.steps[n-1].elapsed = time.Since(m.steps[n-1].started)
		}
		m.steps = append(m.steps, stepState{name: msg.name, status: stRunning, started: time.Now()})
		m.lines = nil
		m.viewport.SetContentLines(nil)
		m.resizeTail()
		return m, m.spinner.Tick
	case detailMsg:
		if n := len(m.steps); n > 0 && m.steps[n-1].status == stRunning {
			m.steps[n-1].detail = msg.s
		}
		return m, nil
	case lineMsg:
		// Sanitize before storing: subprocess output (git progress in
		// particular) can contain embedded carriage returns and other control
		// chars. A \r moves the cursor to column 0 mid-line; the viewport
		// renders line-by-line and doesn't simulate cursor movement within a
		// line, so a \r-laden line would corrupt the frame. Collapse to what a
		// terminal would visually show (the segment after the last \r) and drop
		// other control chars. Width truncation is left to the viewport.
		line := sanitizeLine(msg.line)
		if line == "" {
			return m, nil
		}
		m.lines = append(m.lines, line)
		if len(m.lines) > tailLines {
			m.lines = m.lines[len(m.lines)-tailLines:]
		}
		m.viewport.SetContentLines(m.lines)
		m.viewport.GotoBottom()
		return m, nil
	case doneMsg:
		if n := len(m.steps); n > 0 && m.steps[n-1].status == stRunning {
			m.steps[n-1].elapsed = time.Since(m.steps[n-1].started)
			if msg.err == nil {
				m.steps[n-1].status = stDone
			} else {
				m.steps[n-1].status = stFail
				m.steps[n-1].errText = msg.err.Error()
			}
		}
		// Finalizing a step changes the count of headers above the running
		// step, so the tail's available height may change — recompute it.
		m.resizeTail()
		return m, nil
	}
	return m, nil
}

// resizeTail sets the viewport's height to the rows available for the running
// step's tail (window height minus the fixed step headers above it, the running
// header, and a 1-row margin), capped at tailLines, and re-pins to the bottom.
// Called whenever the window size or the step layout changes — not on every
// line (adding a line doesn't change the header count).
func (m *model) resizeTail() {
	m.viewport.SetHeight(m.tailHeight())
	m.viewport.GotoBottom()
}

// tailWidth returns the width to use for the running step's tail viewport: one
// column less than the terminal width. The viewport pads every rendered line
// (both content and the blank rows that fill the fixed frame height) to its
// width via lipgloss; if that width equals the terminal width, each line ends
// in the last column and the cursor lands in the phantom (pending-autowrap)
// column. The cursed inline renderer then desyncs its differential cursor
// model from the terminal on the following newline — the cursor's Y drifts by
// a row per full-width line, and over a rapid modules_install flood (hundreds
// of lines) the drift compounds: tail content is painted onto the
// spinner/header rows and fragments of old, un-cleared lines bleed through
// (the renderer itself documents this as "cursor desync" for the wide-cell
// variant). Leaving a one-column right margin keeps the cursor off the phantom
// column. This never shows up under test because the tests feed short lines
// ("x", "CC main.o") that never reach the last column — it only bites real,
// long make output padded out to the full width.
func (m model) tailWidth() int {
	return m.width - 1
}

// tailHeight returns the number of rows available for the running step's tail
// viewport: the window height minus the finalized step headers above the running
// step, minus the running header itself and a 1-row margin (keeping the frame
// below the window so the inline renderer never scrolls). Capped at tailLines
// so a tall terminal doesn't reserve an oversized tail.
func (m model) tailHeight() int {
	finishedHeaders := 0
	for i, s := range m.steps {
		isLast := i == len(m.steps)-1
		if !isLast || s.status != stRunning {
			finishedHeaders++
		}
	}
	avail := max(m.height-finishedHeaders-2, 0) // headers + running header + 1 margin
	return min(tailLines, avail)
}

func (m model) View() tea.View {
	var b strings.Builder
	for i, s := range m.steps {
		if i < len(m.steps)-1 {
			b.WriteString(renderFinished(s))
			b.WriteByte('\n')
			continue
		}
		if s.status != stRunning {
			b.WriteString(renderFinished(s))
			b.WriteByte('\n')
			continue
		}
		// The current, running step: spinner + name + detail + live elapsed. The
		// spinner is cyan, the name is plain, and the detail/elapsed are dimmed
		// so the eye is drawn to the spinner + step name first.
		b.WriteString(spinnerStyle.Render(m.spinner.View()))
		b.WriteByte(' ')
		b.WriteString(s.name)
		if s.detail != "" {
			b.WriteString(" " + faintStyle.Render(s.detail))
		}
		b.WriteString(" " + faintStyle.Render(fmtElapsed(time.Since(s.started))))
		b.WriteByte('\n')
		// Render the tail viewport only once output has begun. Before any line
		// arrives the step shows just its header (no blank rows) — important for
		// silent steps like the source copy (~27s, no line output). Once the
		// first line arrives the viewport reserves its full fixed height (a
		// single resize, not a per-line storm); from then on its constant
		// Height keeps the frame area stable so the inline renderer never
		// triggers a full clear+redraw on each line. The Height>0 guard skips a
		// window too short to fit any tail rows (the step shows just its header).
		if len(m.lines) > 0 && m.viewport.Height() > 0 {
			b.WriteString(m.viewport.View())
			b.WriteByte('\n')
		}
	}
	return tea.NewView(b.String())
}

// sanitizeLine prepares a subprocess output line for the tail viewport: it
// collapses carriage-return overwrites (git progress writes "Receiving 1%\r...
// Receiving 100%\r" — a terminal shows only the last segment written, so we
// keep it) and drops other control characters that would move the cursor or
// emit beeps. The viewport renders line-by-line and doesn't simulate cursor
// movement within a line, so a \r would corrupt the frame. Width truncation is
// intentionally NOT done here — the viewport handles it (ansi.StringWidth/
// ansi.Cut, CJK-correct). Empty results are dropped by the caller.
func sanitizeLine(line string) string {
	// A carriage return moves the cursor to column 0 without erasing, so a
	// run like "Receiving 1%\rReceiving 100%\r" visually shows the last segment
	// written (git progress). Split on \r and keep the last non-empty segment:
	// a trailing \r (e.g. "done\r") must NOT blank the line, but an empty tail
	// ("\r\r") correctly yields "".
	var last string
	for seg := range strings.SplitSeq(line, "\r") {
		if seg != "" {
			last = seg
		}
	}
	line = last
	// Strip remaining control chars (keep tab/space and printable runes). This
	// also removes any stray \r at the very end and \0/backspace/etc.
	line = strings.Map(func(r rune) rune {
		if r == '\t' || r == ' ' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, line)
	return strings.TrimSpace(line)
}

func renderFinished(s stepState) string {
	markStyle := successMark
	mark := "✓"
	if s.status == stFail {
		markStyle = failMark
		mark = "✗"
	}
	// Build the parenthesised suffix: detail and/or error, then elapsed.
	var inner []string
	if s.detail != "" {
		inner = append(inner, s.detail)
	}
	if s.status == stFail && s.errText != "" {
		inner = append(inner, s.errText)
	}
	inner = append(inner, fmtElapsed(s.elapsed))
	// Colored status mark, then the dimmed "name (detail: …: elapsed)" suffix.
	return markStyle.Render(mark) + faintStyle.Render(fmt.Sprintf(" %s (%s)", s.name, strings.Join(inner, ": ")))
}

// fmtElapsed formats a duration as e.g. "12s" or "1m03s".
func fmtElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	return fmt.Sprintf("%dm%02ds", m, s)
}

// --- plain (non-TTY) reporter ---

type plainReporter struct {
	w    io.Writer
	step string
}

func newPlainReporter(w io.Writer) Reporter { return &plainReporter{w: w} }

func (p *plainReporter) Step(name string) {
	p.finish(nil)
	p.step = name
	_, _ = fmt.Fprintf(p.w, "=> %s\n", name)
}

func (p *plainReporter) Detail(s string) {
	if s == "" {
		return
	}
	_, _ = fmt.Fprintf(p.w, "  -> %s\n", s)
}

func (p *plainReporter) Done(err error) { p.finish(err) }

func (p *plainReporter) Write(b []byte) (int, error) { return p.w.Write(b) }

func (p *plainReporter) finish(err error) {
	if p.step == "" {
		return
	}
	if err == nil {
		_, _ = fmt.Fprintf(p.w, "✓ %s\n", p.step)
	} else {
		_, _ = fmt.Fprintf(p.w, "✗ %s: %v\n", p.step, err)
	}
	p.step = ""
}

// --- tty reporter ---

// sender is the subset of *tea.Program the reporter uses, extracted so the
// line-handling can be tested without a real program.
type sender interface {
	Send(tea.Msg)
}

type ttyReporter struct {
	send   sender
	p      *tea.Program
	done   chan struct{}
	buf    *xbufio.ScanBuffer
	mu     sync.Mutex
	closed bool
	cancel context.CancelFunc
	// w is the underlying writer (the TUI's output). After the TUI exits and
	// the terminal is restored, the failure dump is written here as plain text
	// so the cause survives the teardown (the ✗ line only carries "exit status 2").
	w io.Writer
	// Failure-dump capture. The live tail (model.lines, tailLines) is dropped
	// the instant a step finalizes, so a failed step's make/git output
	// vanishes. We keep a longer rolling buffer per step here and, on the first
	// failure, snapshot it for a plain-text dump printed in Close.
	stepName    string
	stepLines   []string
	failedName  string
	failedLines []string
	failed      bool
}

func newTTYReporter(w io.Writer, cancel context.CancelFunc) *ttyReporter {
	r := &ttyReporter{
		done:   make(chan struct{}),
		cancel: cancel,
		w:      w,
		buf:    xbufio.NewScanBuffer(),
	}
	r.send = r // send to self; Write -> r.Send -> p.Send
	// The program is created with the default options (input = os.Stdin, signal
	// handler on) — only the output is redirected to w. This is the canonical
	// bubbletea setup: the framework puts the terminal into raw mode and,
	// crucially, restores it when Run returns, reads the terminal's responses
	// to its own capability queries (so they don't linger in the input buffer
	// and corrupt the shell after kbake exits), and turns SIGINT/SIGTERM into
	// messages the model handles (Ctrl+C arrives as a keypress in raw mode and
	// is handled in Update). The build context's cancel func is wired into the
	// model so Ctrl+C aborts a running make before the program quits.
	r.p = tea.NewProgram(newModel(cancel),
		tea.WithOutput(w),
	)
	go func() {
		_, _ = r.p.Run()
		// If the program exits for any reason (Ctrl+C, SIGINT/SIGTERM, or the
		// orchestrator's Close) before the build finished, cancel the build
		// context so a running make/git is killed and the orchestrator can
		// unwind instead of hanging. On a normal exit the build is already done,
		// so this is a harmless no-op.
		if r.cancel != nil {
			r.cancel()
		}
		close(r.done)
	}()
	return r
}

// Send forwards to the program; satisfies sender and is the path Write uses.
// Safe to call before Run's event loop starts (Send blocks until it does) and
// after it exits (a no-op once the program's context is canceled).
func (r *ttyReporter) Send(msg tea.Msg) {
	r.p.Send(msg)
}

func (r *ttyReporter) Step(name string) {
	r.mu.Lock()
	r.stepName = name
	r.stepLines = nil
	r.mu.Unlock()
	r.send.Send(stepMsg{name})
}

func (r *ttyReporter) Detail(s string) {
	r.send.Send(detailMsg{s})
}

func (r *ttyReporter) Done(err error) {
	if err != nil {
		r.mu.Lock()
		if !r.failed {
			r.failed = true
			r.failedName = r.stepName
			// Snapshot the failing step's captured output NOW, while the step is
			// still current: a later step (e.g. cleanup after a failure) must
			// not be able to clobber the dump with its own name and lines.
			// Flush surfaces a trailing partial line (no newline yet), which
			// chronologically belongs after the complete lines in stepLines.
			_ = r.buf.Flush()
			r.failedLines = append(slices.Clone(r.stepLines), r.buf.AllNext()...)
		}
		r.mu.Unlock()
	}
	r.send.Send(doneMsg{err: err})
}

func (r *ttyReporter) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	// Only a real program has a goroutine that closes r.done. Tests bypass
	// newTTYReporter (constructing ttyReporter directly with a fakeSender), so
	// r.p is nil there; guard the Quit/wait so Close doesn't deadlock.
	if r.p != nil {
		r.send.Send(tea.Quit())
		<-r.done
	}
	// The TUI is down and the terminal is restored, so it's safe to write plain
	// text. If a step failed, snapshot its captured output (flushing any partial
	// line still buffered in the scanner) and dump it so the cause outlives the
	// live tail, which is dropped the moment a step finalizes.
	r.mu.Lock()
	_ = r.buf.Close() // flush a trailing partial line into stepLines
	if extra := r.buf.AllNext(); len(extra) > 0 {
		r.stepLines = append(r.stepLines, extra...)
	}
	if r.failed && r.failedLines == nil {
		// Defensive: Done snapshots name+lines on the first failure, so this is
		// only reachable if a step failed without Done ever running — fall back
		// to the current step rather than dumping nothing.
		r.failedName = r.stepName
		r.failedLines = append(r.failedLines, r.stepLines...)
	}
	name := r.failedName
	lines := r.failedLines
	r.mu.Unlock()
	if len(lines) > 0 {
		dumpFailure(r.w, name, lines)
	}
	return nil
}

func (r *ttyReporter) Write(b []byte) (int, error) {
	r.mu.Lock()
	if _, err := r.buf.Write(b); err != nil {
		r.mu.Unlock()
		return 0, err
	}
	lines := r.buf.AllNext()
	// Capture the step's output for the failure dump (the live tail is capped at
	// tailLines and dropped on finalization; this keeps up to stepLineCap so the
	// real error survives even if it scrolled past the live view).
	r.stepLines = append(r.stepLines, lines...)
	if len(r.stepLines) > stepLineCap {
		r.stepLines = r.stepLines[len(r.stepLines)-stepLineCap:]
	}
	r.mu.Unlock()
	for _, l := range lines {
		r.send.Send(lineMsg{l})
	}
	return len(b), nil
}

// dumpFailure prints a red header followed by the dimmed last failDumpLines
// lines captured for the failed step. It is called from Close after the TUI
// has quit and the terminal is restored, so the output is plain text that
// persists on screen (unlike the live rolling tail, which is dropped when the
// step finalizes). This mirrors how `docker build` shows the failing step's
// output after its progress UI exits.
func dumpFailure(w io.Writer, step string, lines []string) {
	if len(lines) > failDumpLines {
		lines = lines[len(lines)-failDumpLines:]
	}
	_, _ = fmt.Fprintf(w, "\n%s\n", failMark.Render(fmt.Sprintf("✗ %s — last %d lines of output:", step, len(lines))))
	for _, l := range lines {
		_, _ = fmt.Fprintln(w, "  "+faintStyle.Render(l))
	}
}
