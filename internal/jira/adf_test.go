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

func TestFlattenADFCodeBlock(t *testing.T) {
	raw := `{
		"type": "doc",
		"content": [
			{"type": "paragraph", "content": [{"type": "text", "text": "Repro:"}]},
			{"type": "codeBlock", "attrs": {"language": "go"}, "content": [
				{"type": "text", "text": "func main() {\n\tpanic(\"boom\")\n}"}
			]},
			{"type": "paragraph", "content": [{"type": "text", "text": "after"}]}
		]
	}`
	var doc adfNode
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	got := flattenADF(doc)
	want := "Repro:\n" +
		"```\n" +
		"func main() {\n\tpanic(\"boom\")\n}\n" +
		"```\n" +
		"after"
	if got != want {
		t.Errorf("flattenADF mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestTextToADFRoundTripsThroughFlattenADF encodes text with textToADF, then
// marshals and unmarshals it through the wire shape (json.Marshal ->
// adfNode) and flattens it back with flattenADF, asserting the round trip
// reproduces the original text. This is the encode-side counterpart to the
// flattenADF fixtures above, and exercises both directions of the same
// contract Comment (see jira.go) depends on.
func TestTextToADFRoundTripsThroughFlattenADF(t *testing.T) {
	for _, text := range []string{
		"Opened https://example.test/pull/1",
		"line one\nline two",
	} {
		doc := textToADF(text)
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded adfNode
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := flattenADF(decoded); got != text {
			t.Errorf("round trip mismatch:\ngot:  %q\nwant: %q", got, text)
		}
	}
}

func TestTextToADFDocEnvelope(t *testing.T) {
	doc := textToADF("hello")
	if doc.Type != "doc" || doc.Version != 1 {
		t.Errorf("doc envelope = %+v, want type=doc version=1", doc)
	}
}

func TestFlattenADFTaskList(t *testing.T) {
	raw := `{
		"type": "doc",
		"content": [
			{"type": "paragraph", "content": [{"type": "text", "text": "Checklist:"}]},
			{"type": "taskList", "attrs": {"localId": "abc"}, "content": [
				{"type": "taskItem", "attrs": {"localId": "1", "state": "TODO"}, "content": [
					{"type": "text", "text": "unchecked item"}
				]},
				{"type": "taskItem", "attrs": {"localId": "2", "state": "DONE"}, "content": [
					{"type": "text", "text": "checked item"}
				]}
			]},
			{"type": "paragraph", "content": [{"type": "text", "text": "after"}]}
		]
	}`
	var doc adfNode
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	got := flattenADF(doc)
	want := "Checklist:\n" +
		"- [ ] unchecked item\n" +
		"- [x] checked item\n" +
		"after"
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
