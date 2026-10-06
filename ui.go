package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
	"github.com/charmbracelet/lipgloss"
)

var (
	sOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	sInfo  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	sStep  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	sBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 2)
	sTitle = sBox.BorderForeground(lipgloss.Color("212"))
	sDone  = sBox.BorderForeground(lipgloss.Color("2"))
)

// UI is everything that talks to the user. Tests swap the prompts for canned answers.
type UI struct {
	Out     io.Writer
	Choose  func(header string, opts ...string) string
	Confirm func(q string) bool
	Input   func(header, def string) string
	Spin    func(title string, fn func() error) error
}

func (u UI) ok(f string, a ...any)   { fmt.Fprintln(u.Out, sOK.Render("  ✓ "+fmt.Sprintf(f, a...))) }
func (u UI) info(f string, a ...any) { fmt.Fprintln(u.Out, sInfo.Render("  · "+fmt.Sprintf(f, a...))) }
func (u UI) warn(f string, a ...any) { fmt.Fprintln(u.Out, sWarn.Render("  ! "+fmt.Sprintf(f, a...))) }
func (u UI) step(s string)           { fmt.Fprintln(u.Out, "\n"+sStep.Render(s)) }
func (u UI) box(st lipgloss.Style, lines ...string) {
	fmt.Fprintln(u.Out, st.Render(strings.Join(lines, "\n")))
}

// terminalUI uses huh prompts. CCSYNC_CHOICE answers every prompt without a tty
// (scripts, tests): its value is picked in Choose, "abort" makes Confirm false.
func terminalUI() UI {
	canned := os.Getenv("CCSYNC_CHOICE")
	u := UI{Out: os.Stdout}
	u.Choose = func(header string, opts ...string) string {
		if canned != "" {
			return canned
		}
		v := opts[0]
		o := make([]huh.Option[string], len(opts))
		for i, s := range opts {
			o[i] = huh.NewOption(s, s)
		}
		if err := huh.NewSelect[string]().Title(header).Options(o...).Value(&v).Run(); err != nil {
			return opts[0] // ctrl-c: first option is always the safe one
		}
		return v
	}
	u.Confirm = func(q string) bool {
		if canned != "" {
			return canned != "abort"
		}
		v := false
		_ = huh.NewConfirm().Title(q).Value(&v).Run()
		return v
	}
	u.Input = func(header, def string) string {
		if canned != "" {
			return def
		}
		v := def
		if err := huh.NewInput().Title(header).Value(&v).Run(); errors.Is(err, huh.ErrUserAborted) {
			return "" // ctrl-c: callers stop on empty
		}
		return strings.TrimSpace(v)
	}
	u.Spin = func(title string, fn func() error) error {
		if canned != "" {
			return fn()
		}
		var err error
		if e := spinner.New().Title(title).Action(func() { err = fn() }).Run(); e != nil {
			return e
		}
		return err
	}
	return u
}
