// Review-queue notification (plan 5.5): the one notification kind the
// worker writes when a categorise job leaves rows in the review queue,
// plus the title and ref renderers both writers share (round 33) so the
// worker's lazy creation and the server's self-heal produce
// byte-identical rows.
package categorize

import (
	"encoding/json"
	"fmt"
)

// KindReview is the notification kind of the "N transactions need
// review" prompt.
const KindReview = "review"

// ReviewNotificationTitle renders the prompt title (and body) for count
// pending review rows.
func ReviewNotificationTitle(count int) string {
	return fmt.Sprintf("%d transactions need review", count)
}

// ReviewNotificationRef renders the deterministic jsonb ref {count,
// job_id}: encoding/json sorts map keys, so the bytes are always
// {"count":N,"job_id":"..."}.
func ReviewNotificationRef(count int, jobID string) []byte {
	ref, _ := json.Marshal(map[string]any{"count": count, "job_id": jobID})
	return ref
}
