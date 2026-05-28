package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"mission/internal"
)

var version = "dev"

func printHelp() {
	fmt.Println(`Quest — spec-driven development mission orchestrator

Usage:
  quest                     Launch dashboard (auto-discovers specs)
  quest <slug>              Jump directly to a spec's dashboard
  quest new                 Start a new spec creation flow
  quest <path>              Use a specific project directory

Flags:
  -h, --help                Show this help message
  -v, --version             Show version`)
}

func main() {
	dir, _ := os.Getwd()
	forceSetup := false
	var specSlug string

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-h", "--help":
			printHelp()
			os.Exit(0)
		case "-v", "--version":
			fmt.Printf("quest %s\n", version)
			os.Exit(0)
		case "new":
			forceSetup = true
		default:
			arg := os.Args[i]
			if !strings.Contains(arg, string(filepath.Separator)) && !strings.Contains(arg, "/") {
				specSlug = arg
			} else {
				abs, err := filepath.Abs(arg)
				if err == nil {
					dir = abs
				}
			}
		}
	}

	p := tea.NewProgram(
		internal.NewModel(dir, forceSetup, specSlug),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
