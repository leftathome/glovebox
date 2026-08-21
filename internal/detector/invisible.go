package detector

import (
	"fmt"
	"strings"

	"github.com/leftathome/glovebox/internal/engine"
)

// maxPreview bounds how much recovered payload is echoed into the signal
// detail. The quarantine record already carries the full content hash; the
// preview exists to tell a human reviewer what was hidden, not to
// reproduce it in full inside an audit line.
const maxPreview = 120

// InvisibleSmugglingDetector fires on Unicode Tags-block characters
// (U+E0000-U+E007F), the invisible-ASCII channel used to hide instructions
// in plain sight.
//
// It is deliberately narrower than encoding_anomaly. Zero-width joiners
// appear legitimately in emoji sequences and Indic/Persian text, and the
// explicit bidi controls appear in genuine right-to-left documents, so
// those stay suspicion signals at encoding_anomaly's weight. The Tags block
// has no legitimate use in ingested content, which is what justifies
// wiring this detector to a rule heavy enough to quarantine on its own.
type InvisibleSmugglingDetector struct{}

func (d InvisibleSmugglingDetector) Detect(content []byte) ([]engine.Signal, error) {
	tags, _ := engine.CountInvisible(content)
	if tags == 0 {
		return nil, nil
	}

	detail := fmt.Sprintf("%d Unicode Tags-block characters", tags)
	if payload := engine.DecodeTagChars(content); len(payload) > 0 {
		preview := string(payload)
		if len(preview) > maxPreview {
			preview = preview[:maxPreview] + "..."
		}
		// Collapse newlines so a smuggled payload cannot break the audit
		// line's shape; encoding/json still escapes the value itself.
		preview = strings.ReplaceAll(preview, "\n", " ")
		detail += fmt.Sprintf("; decoded hidden text: %q", preview)
	}

	return []engine.Signal{{
		Name:    "invisible_smuggling",
		Weight:  1.0,
		Matched: detail,
	}}, nil
}
