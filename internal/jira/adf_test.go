package jira

import (
	"encoding/json"
	"testing"
)

// nestedADFFixture is a Jira ADF description tree with: two paragraphs
// (one carrying bold and italic text marks), a bullet list of nested
// paragraphs, a table, an info panel, and a mention — the table/panel/
// mention content must not appear in the flattened output.
const nestedADFFixture = `{
	"type": "doc",
	"version": 1,
	"content": [
		{
			"type": "paragraph",
			"content": [
				{"type": "text", "text": "Hello "},
				{"type": "text", "text": "world", "marks": [{"type": "strong"}]},
				{"type": "text", "text": "!"}
			]
		},
		{
			"type": "paragraph",
			"content": [
				{"type": "text", "text": "Second paragraph with "},
				{"type": "text", "text": "italic", "marks": [{"type": "em"}]},
				{"type": "text", "text": " text."}
			]
		},
		{
			"type": "bulletList",
			"content": [
				{"type": "listItem", "content": [
					{"type": "paragraph", "content": [{"type": "text", "text": "item one"}]}
				]},
				{"type": "listItem", "content": [
					{"type": "paragraph", "content": [{"type": "text", "text": "item two"}]}
				]}
			]
		},
		{
			"type": "table",
			"content": [
				{"type": "tableRow", "content": [
					{"type": "tableCell", "content": [
						{"type": "paragraph", "content": [{"type": "text", "text": "should not appear"}]}
					]}
				]}
			]
		},
		{
			"type": "panel",
			"attrs": {"panelType": "info"},
			"content": [
				{"type": "paragraph", "content": [{"type": "text", "text": "panel text should be skipped"}]}
			]
		},
		{
			"type": "paragraph",
			"content": [
				{"type": "text", "text": "Mentions: "},
				{"type": "mention", "attrs": {"id": "123", "text": "@bob"}},
				{"type": "text", "text": " done."}
			]
		}
	]
}`

func TestFlattenADFNested(t *testing.T) {
	var doc adfNode
	if err := json.Unmarshal([]byte(nestedADFFixture), &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	got := flattenADF(doc)
	want := "Hello world!\n" +
		"Second paragraph with italic text.\n" +
		"item one\n" +
		"item two\n" +
		"Mentions:  done."

	if got != want {
		t.Errorf("flattenADF mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFlattenADFEmptyDoc(t *testing.T) {
	doc := adfNode{Type: "doc"}
	if got := flattenADF(doc); got != "" {
		t.Errorf("flattenADF(empty doc) = %q, want empty", got)
	}
}

func TestFlattenADFSkipsUnknownNodeType(t *testing.T) {
	raw := `{
		"type": "doc",
		"content": [
			{"type": "paragraph", "content": [{"type": "text", "text": "before"}]},
			{"type": "someFutureBlockType", "content": [
				{"type": "paragraph", "content": [{"type": "text", "text": "nested in unknown"}]}
			]},
			{"type": "paragraph", "content": [{"type": "text", "text": "after"}]}
		]
	}`
	var doc adfNode
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	got := flattenADF(doc)
	want := "before\nnested in unknown\nafter"
	if got != want {
		t.Errorf("flattenADF mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFlattenADFHardBreak(t *testing.T) {
	raw := `{
		"type": "doc",
		"content": [
			{"type": "paragraph", "content": [
				{"type": "text", "text": "line one"},
				{"type": "hardBreak"},
				{"type": "text", "text": "line two"}
			]}
		]
	}`
	var doc adfNode
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	got := flattenADF(doc)
	want := "line one\nline two"
	if got != want {
		t.Errorf("flattenADF mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}
