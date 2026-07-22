package jira

import "strings"

// adfNode is the subset of an Atlassian Document Format node we care about.
// ADF is a recursive JSON tree (doc -> content[] -> node -> content[] -> ...);
// this struct only names the fields the walker reads, so unknown attrs (marks,
// attrs, table layout, etc.) decode into the zero value and are ignored rather
// than erroring.
type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text,omitempty"`
	Content []adfNode `json:"content,omitempty"`
}

// adfDoc is the top-level Atlassian Document Format envelope Jira expects
// wherever it accepts rich text for writing (e.g. a comment body): a doc-typed
// node plus the version number, neither of which flattenADF's read-side
// adfNode carries.
type adfDoc struct {
	Type    string    `json:"type"`
	Content []adfNode `json:"content"`
	Version int       `json:"version"`
}

// textToADF renders plain text into a minimal ADF document: each line becomes
// its own paragraph, so a multi-line comment body (e.g. a PR link followed by
// a status line) doesn't collapse onto one line. It is the encode-side
// counterpart to flattenADF, needed because Jira's comment endpoint only
// accepts ADF bodies, never plain text.
func textToADF(text string) adfDoc {
	lines := strings.Split(text, "\n")
	content := make([]adfNode, 0, len(lines))
	for _, line := range lines {
		para := adfNode{Type: "paragraph"}
		if line != "" {
			para.Content = []adfNode{{Type: "text", Text: line}}
		}
		content = append(content, para)
	}
	return adfDoc{Type: "doc", Version: 1, Content: content}
}

// skippedADFNodes are block types whose content is not prose (tables, panels,
// media, mentions, dividers): the walker skips them and their children
// entirely rather than dumping raw cell/attr text into the brief.
var skippedADFNodes = map[string]bool{
	"table":           true,
	"panel":           true,
	"mediaSingle":     true,
	"media":           true,
	"mediaGroup":      true,
	"mention":         true,
	"emoji":           true,
	"rule":            true,
	"extension":       true,
	"bodiedExtension": true,
	"inlineExtension": true,
}

// flattenADF walks an ADF document tree and returns its plain-text rendering:
// each paragraph/heading becomes one line and each codeBlock becomes a
// fenced line, joined with newlines. Unknown node types are traversed for
// nested paragraphs (lists, blockquotes, expands) so content isn't silently
// dropped; known non-prose types (tables, panels, mentions, media) are
// skipped entirely rather than erroring.
func flattenADF(doc adfNode) string {
	var lines []string
	var walkBlock func(n adfNode)
	walkBlock = func(n adfNode) {
		if skippedADFNodes[n.Type] {
			return
		}
		switch n.Type {
		case "paragraph", "heading":
			if text := walkInline(n); text != "" {
				lines = append(lines, text)
			}
		case "codeBlock":
			// A codeBlock's content is text nodes directly, not paragraph-wrapped,
			// so walkInline (which just concatenates text/hardBreak children)
			// extracts it correctly; the default branch below would instead
			// recurse into those text nodes as if they were nested blocks and
			// silently produce nothing.
			if text := walkInline(n); text != "" {
				lines = append(lines, "```\n"+text+"\n```")
			}
		default:
			// doc, blockquote, bulletList, orderedList, listItem, expand, and any
			// unrecognized container: no prose of its own, recurse for nested blocks.
			for _, c := range n.Content {
				walkBlock(c)
			}
		}
	}
	walkBlock(doc)
	return strings.Join(lines, "\n")
}

// walkInline collects the text of a single block node (paragraph/heading/
// codeBlock), concatenating text nodes and turning hardBreaks into newlines.
// Inline nodes with no plain-text meaning (mention, emoji, unknown) are
// skipped gracefully.
func walkInline(n adfNode) string {
	var out strings.Builder
	for _, c := range n.Content {
		switch c.Type {
		case "text":
			out.WriteString(c.Text)
		case "hardBreak":
			out.WriteString("\n")
		default:
			// mention, emoji, inlineExtension, or anything unrecognized: no plain
			// text to extract, skip without error.
		}
	}
	return out.String()
}
