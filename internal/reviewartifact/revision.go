package reviewartifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const reviewResolutionRevisionPrefix = "review-resolution-v2:sha256:"

// ReviewResolutionRevision fingerprints the complete owner-rendered item and
// its scoped area. The revision field itself is cleared to avoid recursion.
// Only Go computes this token; other runtimes treat it as an opaque value.
func ReviewResolutionRevision(area string, item QueueItem) string {
	item.ReviewResolutionRevision = ""
	preimage := struct {
		Area string    `json:"area"`
		Item QueueItem `json:"item"`
	}{
		Area: area,
		Item: item,
	}
	body, err := json.Marshal(preimage)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return reviewResolutionRevisionPrefix + hex.EncodeToString(sum[:])
}

// stampReviewResolutionRevisions runs after annotation, grouping, and recount
// so every token binds the state the operator actually sees. Group samples are
// copies, so propagate the matching item's token into those copies as well.
func stampReviewResolutionRevisions(report *QueueReport) {
	if report == nil {
		return
	}
	revisions := make(map[string]string, len(report.Items))
	for i := range report.Items {
		item := &report.Items[i]
		item.ReviewResolutionRevision = ReviewResolutionRevision(item.Area, *item)
		revisions[item.Area+"\x00"+item.ID] = item.ReviewResolutionRevision
	}
	for i := range report.Groups {
		sample := report.Groups[i].Sample
		if sample == nil {
			continue
		}
		if revision := revisions[sample.Area+"\x00"+sample.ID]; revision != "" {
			sample.ReviewResolutionRevision = revision
			continue
		}
		sample.ReviewResolutionRevision = ReviewResolutionRevision(sample.Area, *sample)
	}
}
