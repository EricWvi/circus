package textutil

import (
	"encoding/binary"
	"unicode/utf16"
	"unicode/utf8"
)

// Encoding identifies the byte encoding used by a decoded file.
type Encoding uint8

const (
	// EncodingUTF8 identifies ordinary UTF-8 bytes.
	EncodingUTF8 Encoding = iota
	// EncodingUTF16LE identifies UTF-16LE bytes with a byte-order mark.
	EncodingUTF16LE
)

// Decoded is file text together with its original encoding and UTF-8 validity.
type Decoded struct {
	Text        string
	Encoding    Encoding
	InvalidUTF8 bool
}

// HasUTF16LEBOM reports whether data starts with the UTF-16LE byte-order mark.
func HasUTF16LEBOM(data []byte) bool {
	return len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe
}

// Decode converts UTF-8 or BOM-marked UTF-16LE bytes to UTF-8 text.
func Decode(data []byte) Decoded {
	if HasUTF16LEBOM(data) {
		// Inferred: UTF-16BE and other BOMs are treated as non-UTF-8 until captured.
		return Decoded{Text: decodeUTF16LE(data[2:]), Encoding: EncodingUTF16LE}
	}
	// Inferred: a valid literal U+FFFD does not indicate an encoding failure;
	// only replacement runes produced while decoding invalid bytes do.
	return Decoded{Text: NodeUTF8(data), Encoding: EncodingUTF8, InvalidUTF8: !utf8.Valid(data)}
}

// Encode converts UTF-8 text back to the decoded file's original encoding.
func (d Decoded) Encode(text string) []byte {
	if d.Encoding != EncodingUTF16LE {
		return []byte(text)
	}
	units := utf16.Encode([]rune(text))
	data := make([]byte, 2+2*len(units))
	data[0], data[1] = 0xff, 0xfe
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[2+2*i:], unit)
	}
	return data
}

func decodeUTF16LE(data []byte) string {
	units := make([]uint16, 0, (len(data)+1)/2)
	for len(data) >= 2 {
		units = append(units, binary.LittleEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) == 1 {
		units = append(units, uint16(utf8.RuneError))
	}
	return string(utf16.Decode(units))
}
