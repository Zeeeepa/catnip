package tui

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/vanpelt/catnip/internal/services"
	"github.com/vanpelt/catnip/internal/tui/components"
)

var debugLogger *log.Logger
var debugEnabled bool

func init() {
	debugEnabled = os.Getenv("DEBUG") == "true"
	if debugEnabled {
		logFile, err := os.OpenFile("/tmp/catctrl-debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			log.Fatalln("Failed to open debug log file:", err)
		}
		debugLogger = log.New(logFile, "", log.LstdFlags|log.Lmicroseconds)
		debugLogger.Println("=== TUI DEBUG LOG STARTED ===")
	}
}

func debugLog(format string, args ...interface{}) {
	if debugEnabled && debugLogger != nil {
		debugLogger.Printf(format, args...)
	}
}

// App represents the main TUI application
type App struct {
	containerService *services.ContainerService
	containerName    string
	program          *tea.Program
	sseClient        *SSEClient
}

// NewApp creates a new application instance
func NewApp(containerService *services.ContainerService, containerName, workDir string) *App {
	return &App{
		containerService: containerService,
		containerName:    containerName,
	}
}

// Run starts the TUI application
func (a *App) Run(ctx context.Context, workDir string) error {
	// Initialize search input
	searchInput := textinput.New()
	searchInput.Placeholder = "Enter search pattern (regex supported)..."
	searchInput.CharLimit = 100
	searchInput.Width = 50
	searchInput.Prompt = "🔍 "
	searchInput.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(components.ColorPrimary)).Bold(true)
	searchInput.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(components.ColorText))
	searchInput.Cursor.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(components.ColorAccent)).Bold(true)

	// Initialize viewports
	logsViewport := viewport.New(80, 20)
	shellViewport := viewport.New(80, 24)

	// Load logo
	logo := loadLogo()

	// Initialize SSE client
	sseClient := NewSSEClient("http://localhost:8080/v1/events", nil)

	// Create the model using the new structure
	m := NewModel(a.containerService, a.containerName, workDir)

	// Set up the detailed model state
	m.logo = logo
	m.logsViewport = logsViewport
	m.searchInput = searchInput
	m.shellViewport = shellViewport
	m.shellSpinner = spinner.New()
	m.sseClient = sseClient

	// Initialize spinner
	m.shellSpinner.Spinner = spinner.Dot
	m.shellSpinner.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	a.program = tea.NewProgram(m, tea.WithAltScreen())

	// Initialize the shell manager with the program
	InitShellManager(a.program)

	// Update SSE client with the program reference
	sseClient.program = a.program
	a.sseClient = sseClient

	_, err := a.program.Run()

	// Clean up SSE client if it was started
	if a.sseClient != nil {
		a.sseClient.Stop()
	}

	return err
}

// Init initializes the model and returns initial commands
func (m Model) Init() tea.Cmd {
	return m.initCommands()
}

// View renders the current view
func (m Model) View() string {
	if m.width == 0 {
		return ""
	}

	// Get content from current view
	content := m.GetCurrentView().Render(&m)

	// Header
	headerStyle := components.HeaderStyle.Width(m.width-2).Padding(0, 1)
	header := headerStyle.Render(fmt.Sprintf("🐱 Catnip - %s", m.containerName))

	// Footer
	footer := m.renderFooter()

	// Main content area
	mainHeight := m.height - 4 // Account for header and footer
	mainStyle := components.MainContentStyle.Width(m.width - 2).Height(mainHeight)
	mainContent := mainStyle.Render(content)

	result := lipgloss.JoinVertical(lipgloss.Left, header, mainContent, footer)
	return result
}

// renderFooter renders the appropriate footer for the current view
func (m Model) renderFooter() string {
	footerStyle := components.FooterStyle.Width(m.width - 2)

	switch m.currentView {
	case OverviewView:
		return footerStyle.Render("Press l for logs, s for shell, 0 to open UI, 1-9 to open ports, q to quit")
	case ShellView:
		scrollKey := "Alt"
		if runtime.GOOS == "darwin" {
			scrollKey = "Option"
		}
		return footerStyle.Render(fmt.Sprintf("Ctrl+O: overview | Ctrl+Q: quit | %s+↑↓/PgUp/PgDn: scroll", scrollKey))
	case LogsView:
		if m.searchMode {
			// Replace footer with search input
			searchPrompt := "Search: "
			searchContent := searchPrompt + m.searchInput.View() + " (Enter to apply, Esc to cancel)"
			return footerStyle.Render(searchContent)
		} else {
			if m.searchPattern != "" {
				return footerStyle.Render("/ search, c clear filter, ↑↓ scroll, o overview, q quit • Streaming filtered logs")
			} else {
				return footerStyle.Render("/ search, c clear filter, ↑↓ scroll, o overview, q quit • Auto-refresh: ON")
			}
		}
	}
	return footerStyle.Render("")
}

// Helper functions that are still needed

// loadLogo reads the ASCII logo from the public directory
func loadLogo() []string {
	// Try to find the logo file
	possiblePaths := []string{
		"public/logo.ascii",
		"../public/logo.ascii",
		"../../public/logo.ascii",
		"../../../public/logo.ascii",
	}

	for _, path := range possiblePaths {
		if content, err := os.ReadFile(path); err == nil {
			lines := strings.Split(string(content), "\n")
			// Remove any trailing empty lines
			for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
				lines = lines[:len(lines)-1]
			}
			return lines
		}
	}

	// If we can't find the logo, return empty
	return []string{}
}

// isAppReady checks if the app is ready by hitting the /health endpoint
func isAppReady(baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}
