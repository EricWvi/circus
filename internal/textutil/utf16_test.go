package textutil

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"
)

func TestDecodeInvalidUTF8AndLiteralReplacement(t *testing.T) {
	invalid := Decode([]byte{'a', 0xff, 'b', 0xfe, 0xfd, 'c', '\n'})
	if !invalid.InvalidUTF8 || invalid.Encoding != EncodingUTF8 || invalid.Text != "a\uFFFDb\uFFFD\uFFFDc\n" {
		t.Fatalf("invalid UTF-8 decode: %#v", invalid)
	}
	literal := Decode([]byte("literal \uFFFD\n"))
	if literal.InvalidUTF8 || literal.Text != "literal \uFFFD\n" {
		t.Fatalf("literal U+FFFD was treated as invalid: %#v", literal)
	}
}

func TestUTF16LEStreamingMatchesWholeDecode(t *testing.T) {
	tests := [][]byte{
		{0xff, 0xfe, 'h', 0, 'i', 0, '\n', 0},
		{0xff, 0xfe, 0x3d, 0xd8, 0x00, 0xde, '\r', 0, '\n', 0},
		{0xff, 0xfe, 'h', 0, 0x00, 0xd8, 'i', 0},
		{0xff, 0xfe, 'h', 0, 'i', 0, 0x5a},
		{0xff, 0xfe, 0x00, 0xd8, 0x5a},
	}
	for _, raw := range tests {
		for name, source := range map[string]io.Reader{
			"chunked":  bytes.NewReader(raw),
			"one-byte": iotest.OneByteReader(bytes.NewReader(raw)),
		} {
			got, err := io.ReadAll(NewUTF16LEDecoder(source))
			if err != nil {
				t.Fatal(err)
			}
			want := Decode(raw).Text
			if string(got) != want {
				t.Errorf("%s streaming decode: expected %q, got %q", name, want, got)
			}
		}
	}
}

func TestUTF16LEDecoderOneByteInput(t *testing.T) {
	got, err := io.ReadAll(NewUTF16LEDecoder(bytes.NewReader([]byte{0xff})))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\uFFFD" {
		t.Fatalf("one-byte decode = %q, want replacement rune", got)
	}
}

func TestUTF16LERoundTrip(t *testing.T) {
	raw := []byte{0xff, 0xfe, 'h', 0, 'i', 0, '\n', 0}
	decoded := Decode(raw)
	if decoded.InvalidUTF8 || decoded.Encoding != EncodingUTF16LE || decoded.Text != "hi\n" {
		t.Fatalf("UTF-16LE decode: %#v", decoded)
	}
	encoded := decoded.Encode("hello\n")
	want := []byte{0xff, 0xfe, 'h', 0, 'e', 0, 'l', 0, 'l', 0, 'o', 0, '\n', 0}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("UTF-16LE encode: expected %v, got %v", want, encoded)
	}
}
