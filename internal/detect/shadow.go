package detect

// shadowShare is the part of a detection that must lie inside a stronger detection of
// another ad for it to be explained by it.
const shadowShare = 0.5

// DropShadowed removes detections explained by a stronger one. Ads can share audio (a
// music bed, a tail reused in a shorter spot), so a short ad can match inside a longer one
// and its alignment then extends past the real match by the rest of its length: a player
// that skips the long ad and then the short one loses content after the long one. A
// detection goes when another ad's detection with a higher score covers at least half of
// it. Back-to-back ads that merely touch keep both. The order is kept.
func DropShadowed(ds []Detection) []Detection {
	out := make([]Detection, 0, len(ds))
	for i, d := range ds {
		shadowed := false
		for j, e := range ds {
			if i == j || e.AdID == d.AdID || e.Score < d.Score || (e.Score == d.Score && j > i) {
				continue
			}
			ov := min(d.End, e.End) - max(d.Start, e.Start)
			if ov > 0 && float64(ov) >= shadowShare*float64(d.End-d.Start) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			out = append(out, d)
		}
	}
	return out
}
