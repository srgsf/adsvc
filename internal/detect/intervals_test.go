package detect

import (
	"encoding/json"
	"testing"
	"time"
)

func TestIntervals(t *testing.T) {
	const s = time.Second
	var iv Intervals
	iv = iv.Add(10*s, 20*s).Add(30*s, 40*s).Add(19*s, 31*s)
	if len(iv) != 1 || iv[0] != [2]time.Duration{10 * s, 40 * s} {
		t.Fatalf("merge: %v", iv)
	}
	if f, to, ok := iv.FirstGap(0, 50*s); !ok || f != 0 || to != 10*s {
		t.Fatalf("gap1: %v %v %v", f, to, ok)
	}
	if f, to, ok := iv.FirstGap(15*s, 50*s); !ok || f != 40*s || to != 50*s {
		t.Fatalf("gap2: %v %v %v", f, to, ok)
	}
	if _, _, ok := iv.FirstGap(12*s, 38*s); ok {
		t.Fatal("no gap expected")
	}
}

func TestJSONInMilliseconds(t *testing.T) {
	iv := Intervals{{1500 * time.Millisecond, 20 * time.Second}}
	d := Detection{Label: "x", Type: "ad", Start: 1234 * time.Millisecond, End: 5 * time.Second, Score: 40, Confirmed: true}
	b, err := json.Marshal(struct {
		Iv Intervals `json:"iv"`
		D  Detection `json:"d"`
	}{iv, d})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"iv":[[1500,20000]],"d":{"adId":"00000000-0000-0000-0000-000000000000","label":"x","type":"ad","startMs":1234,"endMs":5000,"score":40,"confirmed":true}}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	var back struct {
		Iv Intervals `json:"iv"`
		D  Detection `json:"d"`
	}
	if err := json.Unmarshal(b, &back); err != nil || back.D != d || len(back.Iv) != 1 || back.Iv[0] != iv[0] {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}
