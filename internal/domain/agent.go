package domain

import (
	"context"
	"encoding/json"
)

// ModelContext holds the opaque Responses API input item stream from the last
// successfully generated model turn. The transcript remains stored separately.
type ModelContext struct {
	ThroughMessageOrder int64
	ItemsJSON           json.RawMessage
	Initialized         bool
}

type AgentRequest struct {
	Instructions string
	Context      ModelContext
	Messages     []Message
}

type AgentResult struct {
	Text      string
	ItemsJSON json.RawMessage
}

type AgentMemory interface {
	Search(context.Context, string) (string, error)
}
