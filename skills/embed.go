// Package skills embeds the skill that `offload install-agent` copies into agent homes.
package skills

import _ "embed"

//go:embed offload/SKILL.md
var Offload []byte
