package main

// styles.go — Punk Science 2026 brand tokens for the derpy TUI.
//
// All lipgloss styles are defined here as package-level vars so that every
// view function references a single source of truth. Colour values are
// sourced exclusively from the PS 2026 brand spec; no value is guessed or
// approximated.
//
// PS palette quick-reference:
//   #C8DF00  PS Lime        — primary accent, active state, CTA
//   #7A8800  PS Olive       — secondary accent, feedback, hover
//   #454E00  PS Dark Olive  — tertiary, disabled / inactive accent
//   #3A3A3A  PS Dark Grey   — subtle text, dim labels, borders
//   #F0EDE4  PS Cream       — primary text on dark backgrounds

import "github.com/charmbracelet/lipgloss"

var (
	// psTitle is the app header: "punk.science · derpy".
	// Bold PS Lime so it reads as the primary landmark of the UI.
	// psTitle — Bold PS Lime: primary landmark, highest visual weight.
	psTitle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#C8DF00")). // PS Lime
		MarginBottom(1)

	// psTrack — PS Cream: highest-contrast text on dark bg.
	psTrack = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#F0EDE4")). // PS Cream
		MarginBottom(1)

	// psStatus — PS Dark Grey: dim informational text.
	psStatus = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#3A3A3A")). // PS Dark Grey
		MarginBottom(1)

	// psAlbum — PS Olive: the album under the track line. Readable but a
	// step behind the Cream hero; Dark Grey was too faint to notice.
	psAlbum = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#7A8800")) // PS Olive

	// psProgress — PS Lime: active progress bar fill.
	psProgress = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#C8DF00")) // PS Lime

	// psControls — PS Dark Grey: bottom hint, low visual priority.
	psControls = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#3A3A3A")). // PS Dark Grey
		MarginTop(2)

	// psPrompt — PS Lime: draws the eye to active input overlays.
	psPrompt = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#C8DF00")). // PS Lime
		MarginTop(1)

	// psNostr — PS Olive: transient feedback, one step back from accent.
	psNostr = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#7A8800")). // PS Olive
		MarginTop(1)
)
