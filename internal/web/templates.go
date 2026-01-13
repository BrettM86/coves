// Package web provides HTTP handlers and templates for the Coves web interface.
// This includes the landing page and static file serving for the coves.social website.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Templates holds the parsed HTML templates for the web interface.
type Templates struct {
	templates *template.Template
}

// NewTemplates creates a new Templates instance by parsing all embedded templates.
func NewTemplates() (*Templates, error) {
	tmpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}
	return &Templates{templates: tmpl}, nil
}

// Render renders a named template with the provided data to the response writer.
// Returns an error if the template doesn't exist or rendering fails.
func (t *Templates) Render(w http.ResponseWriter, name string, data interface{}) error {
	// Set content type before writing
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Check if template exists
	tmpl := t.templates.Lookup(name)
	if tmpl == nil {
		return fmt.Errorf("template %q not found", name)
	}

	// Execute template
	if err := tmpl.Execute(w, data); err != nil {
		return fmt.Errorf("failed to execute template %q: %w", name, err)
	}

	return nil
}

// ProjectStaticFileServer returns an http.Handler that serves static files from the project root.
// This is used for files that live outside the web package (e.g., /static/images/).
func ProjectStaticFileServer(staticDir string) http.Handler {
	absPath, err := filepath.Abs(staticDir)
	if err != nil {
		panic(fmt.Sprintf("failed to get absolute path for static directory: %v", err))
	}
	return http.StripPrefix("/static/", http.FileServer(http.Dir(absPath)))
}
