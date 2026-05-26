package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"mission/internal"
)

func main() {
	dir, _ := os.Getwd()
	forceSetup := false
	var specSlug string

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
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
