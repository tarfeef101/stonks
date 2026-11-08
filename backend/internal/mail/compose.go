package mail

import (
	"fmt"
	"html"
	"strings"
)

// Subjects are pinned exactly (plan 8.1) — the e2e asserts on them
// verbatim in Mailpit.
const (
	WelcomeSubject      = "Welcome to Personal Finance Tracker"
	VerificationSubject = "Your Personal Finance Tracker account is set up"
)

// ComposeWelcome builds the exactly-once welcome message, sent the first
// time a users row is created for an authenticated principal. name is
// untrusted (a Keycloak profile claim): it is HTML-escaped in the HTML
// body and rendered as plain text in the Text body.
func ComposeWelcome(to, name string) (Message, error) {
	if to == "" {
		return Message{}, fmt.Errorf("mail: welcome: empty recipient")
	}
	g := greet(name)
	text := "Hi " + g + ",\n\n" +
		"Your Personal Finance Tracker account was created.\n" +
		"Sign in, set your preferences, and start tracking your finances.\n\n" +
		"- The Personal Finance Tracker team"
	inner := "<p>Hi " + html.EscapeString(g) + ",</p>\n" +
		"<p>Your Personal Finance Tracker account was created.<br/>\n" +
		"Sign in, set your preferences, and start tracking your finances.</p>"
	return Message{To: to, Subject: WelcomeSubject, Text: text, HTML: htmlPage(inner)}, nil
}

// ComposeVerification builds the message sent the first time the
// onboarded flag flips to true: the account is set up and ready.
func ComposeVerification(to, name string) (Message, error) {
	if to == "" {
		return Message{}, fmt.Errorf("mail: verification: empty recipient")
	}
	g := greet(name)
	text := "Hi " + g + ",\n\n" +
		"Your Personal Finance Tracker account is set up and ready.\n" +
		"You can start importing your finances whenever you like.\n\n" +
		"- The Personal Finance Tracker team"
	inner := "<p>Hi " + html.EscapeString(g) + ",</p>\n" +
		"<p>Your Personal Finance Tracker account is set up and ready.<br/>\n" +
		"You can start importing your finances whenever you like.</p>"
	return Message{To: to, Subject: VerificationSubject, Text: text, HTML: htmlPage(inner)}, nil
}

// greet renders the display name, falling back to "there" when empty.
func greet(name string) string {
	if name = strings.TrimSpace(name); name == "" {
		return "there"
	}
	return name
}

// htmlPage wraps message content in a single <div> with inline CSS only:
// no <link>, no <script>, no hyperlinks (the product has no user-facing
// URL in email), no external assets.
const htmlPageTemplate = `<div style="font-family: Arial, Helvetica, sans-serif; font-size: 14px; line-height: 1.6; color: #111827;">
%s
<p style="color: #6b7280;">- The Personal Finance Tracker team</p>
</div>`

func htmlPage(inner string) string {
	return fmt.Sprintf(htmlPageTemplate, inner)
}
