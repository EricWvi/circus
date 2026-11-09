// Package textutil provides text conversions shared by device tools.
package textutil

import "strings"

const replacementRune = "\uFFFD"

// NodeUTF8 decodes b using the WHATWG UTF-8 decoder's maximal-subpart
// substitution rule, matching Node's Buffer.toString("utf8").
func NodeUTF8(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			out.WriteByte(c)
			i++
			continue
		}

		need := 0
		firstLow, firstHigh := byte(0x80), byte(0xbf)
		switch {
		case c >= 0xc2 && c <= 0xdf:
			need = 1
		case c == 0xe0:
			need, firstLow = 2, 0xa0
		case c >= 0xe1 && c <= 0xec:
			need = 2
		case c == 0xed:
			need, firstHigh = 2, 0x9f
		case c >= 0xee && c <= 0xef:
			need = 2
		case c == 0xf0:
			need, firstLow = 3, 0x90
		case c >= 0xf1 && c <= 0xf3:
			need = 3
		case c == 0xf4:
			need, firstHigh = 3, 0x8f
		default:
			out.WriteString(replacementRune)
			i++
			continue
		}

		valid := 0
		for valid < need && i+1+valid < len(b) {
			next := b[i+1+valid]
			low, high := byte(0x80), byte(0xbf)
			if valid == 0 {
				low, high = firstLow, firstHigh
			}
			if next < low || next > high {
				break
			}
			valid++
		}
		if valid == need {
			out.Write(b[i : i+1+need])
			i += 1 + need
			continue
		}

		// Consume the lead byte and valid continuation bytes, but leave an
		// offending non-continuation byte for the next decoder iteration.
		out.WriteString(replacementRune)
		i += 1 + valid
	}
	return out.String()
}
