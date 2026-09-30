package detect

import (
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

func TestDropShadowed(t *testing.T) {
	s := time.Second
	a, b, c := fingerprint.ID{1}, fingerprint.ID{2}, fingerprint.ID{3}
	long := Detection{AdID: a, Start: 100 * s, End: 130 * s, Score: 300}
	short := Detection{AdID: b, Start: 120 * s, End: 135 * s, Score: 100}   // 10 of 15 s inside
	touching := Detection{AdID: c, Start: 129 * s, End: 144 * s, Score: 50} // 1 s inside
	got := DropShadowed([]Detection{long, short, touching})
	if len(got) != 2 || got[0].AdID != a || got[1].AdID != c {
		t.Fatalf("got %+v", got)
	}
	if got := DropShadowed([]Detection{short, long}); len(got) != 1 || got[0].AdID != a {
		t.Fatalf("order: %+v", got)
	}
}
