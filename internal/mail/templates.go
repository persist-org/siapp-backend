package mail

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	texttemplate "text/template"
	"time"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// The plain-text and HTML templates are parsed separately on purpose: html/template escapes
// values for HTML (a name like "<script>" is harmless there), text/template must not, or a
// plain-text mail would show "&lt;".
var (
	textTemplates = texttemplate.Must(texttemplate.ParseFS(templateFS, "templates/*.txt.tmpl"))
	htmlTemplates = htmltemplate.Must(htmltemplate.ParseFS(templateFS, "templates/*.html.tmpl"))
)

const resetEmailSubject = "Atur ulang kata sandi SiAPP"

type passwordResetData struct {
	Name     string
	Link     string
	ValidFor string
}

// NewPasswordResetMessage builds the password reset email, in Indonesian. link is the full
// URL with the secret token; validFor is how long it works.
func NewPasswordResetMessage(to, fullName, link string, validFor time.Duration) (Message, error) {
	data := passwordResetData{Name: fullName, Link: link, ValidFor: formatDuration(validFor)}

	var text, html bytes.Buffer
	if err := textTemplates.ExecuteTemplate(&text, "password_reset.txt.tmpl", data); err != nil {
		return Message{}, fmt.Errorf("render password reset text: %w", err)
	}
	if err := htmlTemplates.ExecuteTemplate(&html, "password_reset.html.tmpl", data); err != nil {
		return Message{}, fmt.Errorf("render password reset html: %w", err)
	}
	return Message{To: to, Subject: resetEmailSubject, Text: text.String(), HTML: html.String()}, nil
}

// formatDuration writes a duration the way the emails say it: "1 jam", "30 menit".
func formatDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d jam", int(d/time.Hour))
	}
	return fmt.Sprintf("%d menit", int(d.Round(time.Minute)/time.Minute))
}
