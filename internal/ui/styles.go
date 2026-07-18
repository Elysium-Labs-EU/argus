// Package ui defines terminal color palette and lipgloss styles for argus CLI output.
package ui

import "github.com/charmbracelet/lipgloss"

var (
	TextMuted   = lipgloss.NewStyle().Faint(true)                        // hints, next-step lines
	TextCommand = lipgloss.NewStyle().Bold(true).Foreground(ColorAccent) // argus supervise
	TextBold    = lipgloss.NewStyle().Bold(true)

	LabelSuccess = lipgloss.NewStyle().Bold(true).Foreground(ColorSuccess)
	LabelWarning = lipgloss.NewStyle().Bold(true).Foreground(ColorWarning)
	LabelError   = lipgloss.NewStyle().Bold(true).Foreground(ColorError)
	LabelInfo    = lipgloss.NewStyle().Bold(true).Foreground(ColorInfo)
)
