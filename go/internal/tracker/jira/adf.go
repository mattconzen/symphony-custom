package jira

import "strings"

// ADFToText converts an Atlassian Document Format (ADF) node tree to plain text.
//
// The node is expected to be a map with at least a "type" key and optionally
// "content" (array of child nodes) and "text" (string for leaf text nodes).
//
// Rules:
//   - "text" nodes: append the "text" field.
//   - "hardBreak" nodes: append "\n".
//   - paragraph / heading / blockquote containers: append "\n" after their children.
//   - Unknown node types are skipped (no error).
func ADFToText(node any) string {
	if node == nil {
		return ""
	}
	m, ok := node.(map[string]any)
	if !ok {
		return ""
	}

	nodeType, _ := m["type"].(string)
	var sb strings.Builder

	switch nodeType {
	case "text":
		if text, ok := m["text"].(string); ok {
			sb.WriteString(text)
		}
	case "hardBreak":
		sb.WriteString("\n")
	default:
		// Walk children.
		if content, ok := m["content"].([]any); ok {
			for _, child := range content {
				sb.WriteString(ADFToText(child))
			}
		}
		// Append a trailing newline for block-level containers.
		switch nodeType {
		case "paragraph", "heading", "blockquote", "bulletList", "orderedList", "listItem",
			"codeBlock", "panel", "rule", "table", "tableRow", "tableCell", "tableHeader":
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// TextToADF wraps a plain-text string in minimal ADF (Atlassian Document Format).
//
// The resulting structure is:
//
//	{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":<s>}]}]}
//
// This is used when creating comments via the JIRA Cloud REST API.
func TextToADF(s string) any {
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": s,
					},
				},
			},
		},
	}
}
