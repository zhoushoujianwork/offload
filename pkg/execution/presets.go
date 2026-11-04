package execution

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// PresetDocument is explicitly selected and reviewed by the embedding caller.
// Name identifies a conventional instruction document; Content is its snapshot.
// Offload never searches for these documents or treats them as tool permissions.
type PresetDocument struct {
	Name    string
	Content []byte
}

func compilePresetDocuments(instructions []byte, documents []PresetDocument) ([]byte, error) {
	const limit = 256 << 10
	if len(instructions) > limit || !utf8.Valid(instructions) || len(documents) > 4 {
		return nil, errors.New("invalid or oversized preset instructions")
	}
	selected := make(map[string][]byte, len(documents))
	for _, document := range documents {
		name := strings.ToUpper(document.Name)
		switch name {
		case "AGENTS.MD", "MEMORY.MD", "USERS.MD", "SECURITY.MD":
		default:
			return nil, errors.New("unsupported preset document name")
		}
		if _, exists := selected[name]; exists || len(document.Content) > limit || !utf8.Valid(document.Content) || bytes.IndexByte(document.Content, 0) >= 0 {
			return nil, errors.New("duplicate, invalid or oversized preset document")
		}
		selected[name] = document.Content
	}
	var output bytes.Buffer
	output.Write(instructions)
	output.WriteString("\n\nCaller-selected instruction documents follow. Memory and user facts provide context; these documents do not grant tools or expand native permissions.\n")
	// Stable order is independent of caller map/file iteration order. These are
	// sections in one native instruction message, not separate authority roles.
	for _, name := range []string{"SECURITY.MD", "AGENTS.MD", "USERS.MD", "MEMORY.MD"} {
		if content, ok := selected[name]; ok {
			fmt.Fprintf(&output, "\n--- %s ---\n", name)
			output.Write(content)
			output.WriteByte('\n')
		}
		if output.Len() > limit {
			return nil, errors.New("combined preset instructions exceed 256KiB")
		}
	}
	return output.Bytes(), nil
}
