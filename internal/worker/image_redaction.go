package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	imageDataPrefix     = "data:image/"
	malformedImageFrame = "[malformed_image_frame omitted]"
)

var imageDataMIMEs = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// sanitizeCodexAppServerLine removes inline image bytes from app-server frames
// before they reach diagnostics, captures or observers. The parser consumes the
// same sanitized frame; none of its control decisions depend on image payloads.
func sanitizeCodexAppServerLine(line []byte) []byte {
	if !json.Valid(line) {
		return []byte(malformedImageFrame)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return []byte(malformedImageFrame)
	}
	changed, safe := sanitizeImageValue(&value)
	if !safe {
		return []byte(malformedImageFrame)
	}
	if !changed {
		return bytes.Clone(line)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(malformedImageFrame)
	}
	return encoded
}

func sanitizeImageValue(value *any) (changed, safe bool) {
	switch item := (*value).(type) {
	case []any:
		for i := range item {
			childChanged, childSafe := sanitizeImageValue(&item[i])
			if !childSafe {
				return false, false
			}
			changed = changed || childChanged
		}
		return changed, true
	case map[string]any:
		for key, child := range item {
			childChanged, childSafe := sanitizeImageValue(&child)
			if !childSafe {
				return false, false
			}
			if childChanged {
				item[key] = child
				changed = true
			}
		}
		return changed, true
	case string:
		if !strings.Contains(item, imageDataPrefix) && !strings.Contains(item, `\u`) && !strings.Contains(item, `\/`) {
			return false, true
		}
		if json.Valid([]byte(item)) {
			var nested any
			decoder := json.NewDecoder(strings.NewReader(item))
			decoder.UseNumber()
			if decoder.Decode(&nested) == nil {
				nestedChanged, nestedSafe := sanitizeImageValue(&nested)
				if nestedSafe && nestedChanged {
					encoded, err := json.Marshal(nested)
					if err == nil {
						*value = string(encoded)
						return true, true
					}
				}
			}
		}
		replacement, changed := sanitizeEmbeddedImageDataURLs(item)
		if changed {
			*value = replacement
		}
		return changed, true
	default:
		return false, true
	}
}

func sanitizeEmbeddedImageDataURLs(value string) (string, bool) {
	var safe strings.Builder
	rest := value
	changed := false
	for {
		start := strings.Index(rest, imageDataPrefix)
		if start < 0 {
			safe.WriteString(rest)
			return safe.String(), changed
		}
		safe.WriteString(rest[:start])
		end, replacement := sanitizeImageDataURLSpan(rest[start:])
		safe.WriteString(replacement)
		rest = rest[start+end:]
		changed = true
	}
}

func sanitizeImageDataURLSpan(value string) (int, string) {
	for _, mediaType := range imageDataMIMEs {
		prefix := "data:" + mediaType + ";base64,"
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		end := len(prefix)
		for end < len(value) && isBase64Byte(value[end]) {
			end++
		}
		payload := value[len(prefix):end]
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil || len(data) == 0 {
			return end, "[image-data-omitted]"
		}
		digest := sha256.Sum256(data)
		return end, fmt.Sprintf("%s[omitted;sha256=%s;bytes=%d]", prefix, hex.EncodeToString(digest[:]), len(data))
	}
	end := 0
	for end < len(value) && value[end] != ' ' && value[end] != '\t' && value[end] != '\r' && value[end] != '\n' {
		end++
	}
	return end, "[image-data-omitted]"
}

func isBase64Byte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '+' || b == '/' || b == '='
}

func imageOmissionFrame(reason string, originalBytes int) []byte {
	encoded, _ := json.Marshal(map[string]any{
		"method": "offload/image-data-omitted",
		"params": map[string]any{"reason": reason, "originalBytes": originalBytes},
	})
	return encoded
}
