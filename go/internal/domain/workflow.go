package domain

// Workflow is the parsed WORKFLOW.md payload per SPEC §4.1.2.
type Workflow struct {
	// Config is the YAML front matter root object.
	Config map[string]any
	// PromptTemplate is the trimmed Markdown body after front matter.
	PromptTemplate string
}
