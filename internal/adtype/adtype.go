// Package adtype is the type of a reference: what a detection of it is. Players decide what
// to do with each type (the mpv client skips both); adsvc only finds and reports them.
package adtype

// The types. An ad is a commercial or promo break; an intro is a recurring opening
// sequence (title music) that a series repeats in each episode. Credits are not a type:
// they rarely repeat the same audio.
const (
	Ad    = "ad"
	Intro = "intro"
)

// Valid reports whether t is a type ("" is not: see Of).
func Valid(t string) bool { return t == Ad || t == Intro }

// Of is t, or Ad when t is empty (records and rows from before types existed).
func Of(t string) string {
	if t == "" {
		return Ad
	}
	return t
}
