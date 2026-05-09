package domain

// Workspace is the filesystem workspace assigned to one issue identifier per
// SPEC §4.1.4.
type Workspace struct {
	// Path is the absolute workspace path.
	Path string
	// Key is the sanitized issue identifier (workspace directory name).
	Key string
	// CreatedNow is true only if the directory was created during this call.
	CreatedNow bool
}
