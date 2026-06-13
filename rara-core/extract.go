// extract.go — Phase 4 (email lane): the pure text-cleaning for the `extrair` capability.
//
// The email lane's to-text step is `extrair`, not `transcrever`: the source is already text,
// so instead of ASR we strip the noise an email carries — HTML markup, the sender's signature,
// and quoted-reply history — leaving the actual message body the gates and distill should judge.
//
// Everything here is PURE (no I/O): cleanEmailText takes the raw body and returns the cleaned
// text, so it is fully unit-tested. The I/O edge (reading the email, writing the transcripts
// row) lives in runners.go (extractRunner); this file is the cleaning logic only — the email
// counterpart of gates.go's pure cascade.
package main

import (
	"html"
	"regexp"
	"strings"
)

var (
	// reScriptStyle drops <script>/<style> blocks whole (their content is never message text).
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	// reBlockTag turns block-level boundaries into newlines so stripped HTML stays readable.
	reBlockTag = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/h[1-6]|/li)\s*/?>`)
	// reAnyTag removes every remaining tag.
	reAnyTag = regexp.MustCompile(`(?s)<[^>]+>`)
	// reHTMLish detects whether a body is HTML (so plain-text bodies are left untouched).
	reHTMLish = regexp.MustCompile(`(?i)<(html|body|div|p|br|table|span|a|img|head)\b`)
	// reBlankRun collapses 3+ consecutive newlines to a single blank line.
	reBlankRun = regexp.MustCompile(`\n{3,}`)
	// reAttribution matches a reply attribution line ("On <date>, <X> wrote:" / pt "escreveu:")
	// after which the quoted original thread follows — everything from there is dropped.
	reAttribution = regexp.MustCompile(`(?i)^(on\b.*\bwrote:|.*\bescreveu:|-{2,}\s*original message\s*-{2,}|from:\s.+)$`)
)

// cleanEmailText returns the human-written body of an email: HTML stripped (if any), the
// signature (everything after the standard "-- " delimiter) removed, and quoted-reply history
// (lines beginning with ">" and everything after a reply attribution) dropped. Deterministic
// and cheap — the gates/distill then judge the message itself, not its decoration.
func cleanEmailText(raw string) string {
	s := raw
	if reHTMLish.MatchString(s) {
		s = stripHTML(s)
	}
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		line := strings.TrimRight(ln, " \t\r")
		trimmed := strings.TrimSpace(line)
		// Signature delimiter ("-- " on its own line): everything below is the signature.
		if trimmed == "--" {
			break
		}
		// A reply attribution starts the quoted original thread: stop here.
		if reAttribution.MatchString(trimmed) {
			break
		}
		// Quoted lines ("> ...") are prior messages, not this one.
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		out = append(out, line)
	}
	cleaned := reBlankRun.ReplaceAllString(strings.Join(out, "\n"), "\n\n")
	return strings.TrimSpace(cleaned)
}

// stripHTML reduces an HTML body to plain text: drop script/style, turn block boundaries into
// newlines, remove the remaining tags, and unescape entities.
func stripHTML(s string) string {
	s = reScriptStyle.ReplaceAllString(s, "")
	s = reBlockTag.ReplaceAllString(s, "\n")
	s = reAnyTag.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}
