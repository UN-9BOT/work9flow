// Package main is the entry point for work9flowd, the local workflow
// orchestrator runtime. The TUI ships as a separate binary under
// cmd/work9flow and connects to this daemon over the work9flow protocol.
//
// See .beads/issues.jsonl (MVP 01..07) for the planned runtime surface.
package main

import (
	"fmt"
	"os"

	"github.com/charmbracelet/log"

	// Blank imports pin the planned Charm + Catppuccin stack in go.mod
	// until the runtime lands its real callers (MVP 02+). See README.md
	// for what each library is for.
	_ "github.com/catppuccin/go"
	_ "github.com/charmbracelet/bubbletea"
	_ "github.com/charmbracelet/glamour"
	_ "github.com/charmbracelet/huh"
	_ "github.com/charmbracelet/lipgloss"
)

func main() {
	log.SetReportTimestamp(false)
	log.SetReportCaller(false)
	log.Info("work9flowd", "version", "bootstrap", "pid", os.Getpid())
	fmt.Fprintln(os.Stderr, "work9flowd: bootstrap — runtime not implemented yet (MVP 02)")
}
