package domain

// Project is one trusted, canonical entry in the registered workspace.
type Project struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Workspace is the complete trusted project registry and CLI working directory.
type Workspace struct {
	WorkingDirectory string    `json:"working_directory"`
	Projects         []Project `json:"projects"`
}
