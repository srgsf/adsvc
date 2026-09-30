// Package mpv holds adskip.lua, the mpv client of adsvc, for the web page to hand out.
package mpv

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed adskip.lua
var script string

// defaultToken is how the script's defaults name the token; Script fills it in.
const defaultToken = `ads_token = ""` //nolint:gosec // G101: the empty default, not a credential

// Script is adskip.lua with token as the default ads_token.
func Script(token string) []byte {
	return []byte(strings.Replace(script, defaultToken, "ads_token = "+luaString(token), 1))
}

// luaString quotes s as a Lua string literal.
func luaString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range []byte(s) {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\%03d`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
