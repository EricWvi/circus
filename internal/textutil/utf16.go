package textutil

import (
	"encoding/binary"
	"io"
	"strings"
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

// Decoded is file text together with its original encoding and decoding validity.
type Decoded struct {
	Text         string
	Encoding     Encoding
	InvalidUTF8  bool
	InvalidUTF16 bool
}

// HasUTF16LEBOM reports whether data starts with the UTF-16LE byte-order mark.
func HasUTF16LEBOM(data []byte) bool {
	return len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe
}

// Decode converts UTF-8 or BOM-marked UTF-16LE bytes to UTF-8 text.
func Decode(data []byte) Decoded {
	if HasUTF16LEBOM(data) {
		// Inferred: UTF-16BE and other BOMs are treated as non-UTF-8 until captured.
		return Decoded{
			Text: decodeUTF16LE(data[2:]), Encoding: EncodingUTF16LE,
			InvalidUTF16: !validUTF16LE(data[2:]),
		}
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
	size := 2
	for _, r := range text {
		if r > 0xffff {
			size += 4
		} else {
			size += 2
		}
	}
	data := make([]byte, size)
	data[0], data[1] = 0xff, 0xfe
	offset := 2
	for _, r := range text {
		if r <= 0xffff {
			binary.LittleEndian.PutUint16(data[offset:], uint16(r))
			offset += 2
			continue
		}
		high, low := utf16.EncodeRune(r)
		binary.LittleEndian.PutUint16(data[offset:], uint16(high))
		binary.LittleEndian.PutUint16(data[offset+2:], uint16(low))
		offset += 4
	}
	return data
}

func decodeUTF16LE(data []byte) string {
	var text strings.Builder
	text.Grow((len(data) + 1) / 2)
	for len(data) >= 2 {
		unit := binary.LittleEndian.Uint16(data)
		data = data[2:]
		r := rune(unit)
		switch {
		case 0xd800 <= unit && unit <= 0xdbff:
			r = utf8.RuneError
			if len(data) >= 2 {
				next := binary.LittleEndian.Uint16(data)
				if 0xdc00 <= next && next <= 0xdfff {
					r = utf16.DecodeRune(rune(unit), rune(next))
					data = data[2:]
				}
			}
		case 0xdc00 <= unit && unit <= 0xdfff:
			r = utf8.RuneError
		}
		text.WriteRune(r)
	}
	if len(data) == 1 {
		text.WriteRune(utf8.RuneError)
	}
	return text.String()
}

func validUTF16LE(data []byte) bool {
	if len(data)%2 != 0 {
		return false
	}
	for len(data) > 0 {
		unit := binary.LittleEndian.Uint16(data)
		data = data[2:]
		switch {
		case 0xd800 <= unit && unit <= 0xdbff:
			if len(data) == 0 {
				return false
			}
			next := binary.LittleEndian.Uint16(data)
			if next < 0xdc00 || next > 0xdfff {
				return false
			}
			data = data[2:]
		case 0xdc00 <= unit && unit <= 0xdfff:
			return false
		}
	}
	return true
}

// NewUTF16LEDecoder incrementally converts BOM-marked UTF-16LE to UTF-8.
// Malformed code units and a trailing odd byte decode to U+FFFD, matching Decode.
func NewUTF16LEDecoder(source io.Reader) io.Reader {
	return &utf16leDecoder{source: source}
}

type utf16leDecoder struct {
	source  io.Reader
	input   []byte
	output  []byte
	raw     [32 * 1024]byte
	eof     bool
	started bool
}

func (d *utf16leDecoder) Read(p []byte) (int, error) {
	for len(d.output) == 0 {
		if d.eof && len(d.input) == 0 {
			return 0, io.EOF
		}
		if !d.eof {
			n, err := d.source.Read(d.raw[:])
			d.input = append(d.input, d.raw[:n]...)
			if err == io.EOF {
				d.eof = true
			} else if err != nil {
				return 0, err
			} else if n == 0 {
				return 0, nil
			}
		}
		d.decodeAvailable()
	}
	n := copy(p, d.output)
	d.output = d.output[n:]
	return n, nil
}

func (d *utf16leDecoder) decodeAvailable() {
	if !d.started {
		if len(d.input) < 2 {
			if d.eof && len(d.input) == 1 {
				d.output = utf8.AppendRune(d.output[:0], utf8.RuneError)
				d.input = nil
				d.started = true
			}
			return
		}
		d.input = d.input[2:]
		d.started = true
	}

	d.output = d.output[:0]
	consumed := 0
	for len(d.input)-consumed >= 2 {
		unit := binary.LittleEndian.Uint16(d.input[consumed:])
		switch {
		case 0xd800 <= unit && unit <= 0xdbff:
			if len(d.input)-consumed < 4 {
				if !d.eof {
					d.input = append(d.input[:0], d.input[consumed:]...)
					return
				}
				d.output = utf8.AppendRune(d.output, utf8.RuneError)
				consumed += 2
				continue
			}
			next := binary.LittleEndian.Uint16(d.input[consumed+2:])
			if 0xdc00 <= next && next <= 0xdfff {
				d.output = utf8.AppendRune(d.output, utf16.DecodeRune(rune(unit), rune(next)))
				consumed += 4
				continue
			}
			d.output = utf8.AppendRune(d.output, utf8.RuneError)
			consumed += 2
		case 0xdc00 <= unit && unit <= 0xdfff:
			d.output = utf8.AppendRune(d.output, utf8.RuneError)
			consumed += 2
		default:
			d.output = utf8.AppendRune(d.output, rune(unit))
			consumed += 2
		}
	}
	if d.eof && len(d.input)-consumed == 1 {
		d.output = utf8.AppendRune(d.output, utf8.RuneError)
		consumed++
	}
	d.input = append(d.input[:0], d.input[consumed:]...)
}
