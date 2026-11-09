package textutil

import "testing"

func TestNodeUTF8(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{name: "empty", in: nil, want: ""},
		{name: "ASCII", in: []byte("plain text\n"), want: "plain text\n"},
		{name: "valid multibyte", in: []byte("中文, café, 😀"), want: "中文, café, 😀"},
		{name: "independent invalid bytes", in: []byte{0xff, 0xfe, 0xfd}, want: "\uFFFD\uFFFD\uFFFD"},
		{name: "truncated three byte sequence", in: []byte{0xe2, 0x82}, want: "\uFFFD"},
		{name: "truncated four byte sequence", in: []byte{0xf0, 0x9f, 0x98}, want: "\uFFFD"},
		{name: "truncated sequence before ASCII", in: []byte{0xe2, 0x82, 'A'}, want: "\uFFFDA"},
		{name: "invalid continuation", in: []byte{0xe2, '(', 0xa1}, want: "\uFFFD(\uFFFD"},
		{name: "overlong two byte encoding", in: []byte{0xc0, 0xaf}, want: "\uFFFD\uFFFD"},
		{name: "overlong three byte encoding", in: []byte{0xe0, 0x80, 0xaf}, want: "\uFFFD\uFFFD\uFFFD"},
		{name: "overlong four byte encoding", in: []byte{0xf0, 0x80, 0x80, 0xaf}, want: "\uFFFD\uFFFD\uFFFD\uFFFD"},
		{name: "UTF-16 surrogate", in: []byte{0xed, 0xa0, 0x80}, want: "\uFFFD\uFFFD\uFFFD"},
		{name: "above maximum code point", in: []byte{0xf4, 0x90, 0x80, 0x80}, want: "\uFFFD\uFFFD\uFFFD\uFFFD"},
		{name: "invalid bytes around valid text", in: []byte{'a', 0xff, 0xe4, 0xb8, 0xad, 0xfe, 'b'}, want: "a\uFFFD中\uFFFDb"},
		{name: "valid prefix and truncated suffix", in: []byte{0xf0, 0x9f, 0x98, 0x80, 0xe2, 0x82}, want: "😀\uFFFD"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := NodeUTF8(test.in); got != test.want {
				t.Fatalf("NodeUTF8(%x): got %q, want %q", test.in, got, test.want)
			}
		})
	}
}
