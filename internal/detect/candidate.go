package detect

import (
	"math"
	"time"
)

// Candidate is a detection plus the evidence for it. A skip is only safe once the alignment
// has been confirmed, either by analysing the whole ad or by two consecutive blocks
// agreeing on the same alignment with a high score (the player may buffer only 10-60 s).
type Candidate struct {
	Detection
	// Agree counts the consecutive blocks that found this alignment.
	Agree int
	// LastTo is the end of the last block that found it.
	LastTo time.Duration
	// Sticky is set once the candidate has been confirmed (possibly in an earlier
	// session). Evidence only accumulates, so a confirmed ad never goes back to
	// unconfirmed for this alignment.
	Sticky bool
	// FirstSeen and ConfirmedAt are the media time decoded (BlockResult.Reached) when this
	// alignment was found and when it was confirmed, for look-ahead measures; kept by
	// whoever measures them (bench), 0 otherwise.
	FirstSeen, ConfirmedAt time.Duration
}

// DefaultMinScore is the match threshold every command uses unless told otherwise.
const DefaultMinScore = 20

// DefaultConfirmScore is the score that confirms a candidate from two blocks, for a match
// threshold of minScore.
func DefaultConfirmScore(minScore int) int { return 2 * minScore }

// NoBlock is the block time of detections that come from a stored map rather than from an
// analysed block: far from any block, without overflowing in differences.
const NoBlock = -time.Duration(math.MaxInt64 / 4)

// MergeCandidate folds one block's detection into cands: the same alignment again adds to
// the agreement count (when the block follows the previous one), and a better-scoring
// alignment of the same occurrence replaces it. It returns the updated list, the index d
// landed at (-1 when it was dropped) and whether that entry is new or was re-aligned.
func MergeCandidate(cands []Candidate, d Detection, blockFrom, blockTo time.Duration) ([]Candidate, int, bool) {
	for i := range cands {
		e := &cands[i]
		if e.AdID != d.AdID || d.Start >= e.End || e.Start >= d.End {
			continue
		}
		if (d.Start - e.Start).Abs() < 150*time.Millisecond { // same alignment
			if (blockFrom - e.LastTo).Abs() < 250*time.Millisecond {
				e.Agree++
			}
			e.LastTo = blockTo
			if d.Score > e.Score {
				e.Score = d.Score
			}
			return cands, i, false
		}
		if d.Score <= e.Score {
			return cands, -1, false
		}
		*e = Candidate{Detection: d, Agree: 1, LastTo: blockTo} // better alignment wins
		return cands, i, true
	}
	cands = append(cands, Candidate{Detection: d, Agree: 1, LastTo: blockTo})
	return cands, len(cands) - 1, true
}

// Confirm applies the skip rule given what has been analysed: the whole ad has been
// covered, or two consecutive blocks agreed on this alignment with at least confirmScore.
// Once confirmed, the candidate stays confirmed.
func (c *Candidate) Confirm(covered Intervals, confirmScore int) bool {
	if !c.Sticky {
		c.Sticky = covered.Contains(c.Start, c.End) || (c.Agree >= 2 && c.Score >= confirmScore)
		if c.Sticky {
			confirmations.Inc()
		}
	}
	return c.Sticky
}
