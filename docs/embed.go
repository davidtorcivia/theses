// Package docs embeds the public agent instructions and their references.
package docs

import "embed"

//go:embed SKILLS.md api.md connections.md
var AgentFiles embed.FS
