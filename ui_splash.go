package main

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	splashDuration  = 1800 * time.Millisecond
	splashLogoWidth = 61
)

type splashTickMsg time.Time

func splashTickCmd() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg { return splashTickMsg(t) })
}

// Original ASCII rendering of the Proxmox wordmark and X emblem.
// Keep the artwork ASCII-only so it aligns without special terminal fonts.
var splashLogo = []string{
	`XX       XX   ####   ####    ###   #   #  #   #   ###   #   #`,
	` XXX   XXX    #   #  #   #  #   #   # #   ## ##  #   #   # # `,
	`   XXXXX      ####   ####   #   #    #    # # #  #   #    #  `,
	`    XXX       #      # #    #   #   # #   #   #  #   #   # # `,
	`   XXXXX      #      #  ##   ###   #   #  #   #   ###   #   #`,
	` XXX   XXX                                                  `,
	`XX       XX                                                 `,
}

func splashPaint(value, color string, bold bool) string {
	return paint(value, color, "", bold)
}

func (m *model) splashView() string {
	width, height := max(1, m.width), max(1, m.height)
	step := int(m.splashElapsed / (50 * time.Millisecond))
	var lines []string
	if height >= 22 && width >= splashLogoWidth {
		lines = append(lines, splashPaint("·       ·       ·       ·       ·       ·       ·       ·", "#29313B", false), "")
	}
	if width >= splashLogoWidth && height >= 16 {
		// Reveal from left to right, then move a narrow highlight across it.
		reveal := min(splashLogoWidth, 6+step*5)
		highlight := (step*3)%(splashLogoWidth+12) - 6
		gradient := []string{"#A5440B", "#CA570F", "#E87017", "#FF8A24", "#FFAC4D", "#FFD08A"}
		for rowIndex, row := range splashLogo {
			row += strings.Repeat(" ", max(0, splashLogoWidth-len(row)))
			var line strings.Builder
			for col, char := range row {
				if col >= reveal {
					line.WriteByte(' ')
					continue
				}
				color := gradient[min(len(gradient)-1, col*len(gradient)/splashLogoWidth)]
				if col < 11 {
					color = gradient[min(len(gradient)-1, rowIndex)]
				}
				if col >= highlight && col < highlight+4 {
					color = "#FFE5BF"
				}
				line.WriteString(splashPaint(string(char), color, true))
			}
			lines = append(lines, line.String())
		}
		lines = append(lines, "")
	} else {
		lines = append(lines, splashPaint("PROXMOX", orange, true))
	}
	lines = append(lines, splashPaint("PVE STAND DEPLOYER", text, true))
	if height >= 8 {
		lines = append(lines, splashPaint("Учебные стенды Proxmox VE", muted, false), "")
	}
	// Decorative motion, not a claim about API connectivity or deployment progress.
	track := min(32, max(3, width-4))
	head := step % (track + 5)
	var sweep strings.Builder
	for col := 0; col < track; col++ {
		if col <= head && col > head-5 {
			sweep.WriteString(splashPaint("=", orange, true))
		} else {
			sweep.WriteString(splashPaint("-", border, false))
		}
	}
	lines = append(lines, sweep.String())
	if height >= 8 {
		lines = append(lines, "", splashPaint("Любая клавиша — пропустить", muted, false))
	}
	if height >= 22 && width >= splashLogoWidth {
		lines = append(lines, "", splashPaint("·       ·       ·       ·       ·       ·       ·       ·", "#29313B", false))
	}
	content := lipgloss.JoinVertical(lipgloss.Center, lines...)
	content = lipgloss.NewStyle().MaxWidth(width).MaxHeight(height).Render(content)
	return paint(lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content), text, graphite, false)
}
