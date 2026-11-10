package fstools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"github.com/EricWvi/circus/internal/textutil"
)

func TestStreamReadRangeCRLFAtBufferBoundary(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, streamReadBufferSize-1)
	text := string(payload) + "\r\n"
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "UTF-8", raw: []byte(text)},
		{name: "UTF-16LE", raw: textutil.Decoded{Encoding: textutil.EncodingUTF16LE}.Encode(text)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := filepath.Join("..", "..", "..", ".tmp")
			if err := os.MkdirAll(base, 0o755); err != nil {
				t.Fatal(err)
			}
			dir, err := os.MkdirTemp(base, "fstools-read-test-*")
			if err != nil {
				t.Fatal(err)
			}
			dir, err = filepath.Abs(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(dir); err != nil {
					t.Error(err)
				}
			})
			leakcheck.Track(t, dir)
			path := filepath.Join(dir, "boundary.txt")
			if err := os.WriteFile(path, test.raw, 0o600); err != nil {
				t.Fatal(err)
			}

			result, err := NewRead().Run(context.Background(), &circusv1.DeviceCall{
				Tool: &circusv1.DeviceCall_Read{Read: &circusv1.ReadRequest{Path: path, Cwd: dir, Limit: 1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.GetOutput().GetIsError() {
				t.Fatalf("Read failed: %v", result)
			}
			want := "1\t" + string(payload)
			if got := result.GetOutput().GetContent()[0].GetText(); got != want {
				t.Fatalf("CRLF at buffer boundary was not stripped: got %q, want %q", got, want)
			}
		})
	}
}

func TestReadSizeTextMatchesNativeProbes(t *testing.T) {
	tests := []struct {
		size int64
		want string
	}{
		{size: 400000, want: "390.6KB"},
		{size: 524288, want: "512KB"},
		{size: 524289, want: "512KB"},
		{size: 600000, want: "585.9KB"},
		{size: 1048576, want: "1MB"},
		{size: 2000000, want: "1.9MB"},
		{size: 3200000, want: "3.1MB"},
		{size: 3200001, want: "3.1MB"},
		{size: 4000000, want: "3.8MB"},
		{size: 8000000, want: "7.6MB"},
		{size: 16777216, want: "512KB"},
		{size: 33554432, want: "512KB"},
		{size: 104857600, want: "512KB"},
		{size: 12388608, want: "512KB"},
		{size: 10194304, want: "9.7MB"},
		{size: 11291456, want: "512KB"},
		{size: 10742880, want: "512KB"},
		{size: 10468592, want: "10MB"},
		{size: 10605736, want: "512KB"},
		{size: 10537164, want: "512KB"},
		{size: 10502878, want: "512KB"},
		{size: 10485735, want: "10MB"},
		{size: 10494306, want: "512KB"},
		{size: 10490020, want: "512KB"},
		{size: 10487877, want: "512KB"},
		{size: 10486806, want: "512KB"},
		{size: 10486270, want: "512KB"},
		{size: 10486002, want: "512KB"},
		{size: 10485868, want: "512KB"},
		{size: 10485801, want: "512KB"},
		{size: 10485768, want: "512KB"},
		{size: 10485751, want: "10MB"},
		{size: 10485759, want: "10MB"},
		{size: 10485763, want: "512KB"},
		{size: 10485761, want: "512KB"},
		{size: 10485760, want: "512KB"},
	}
	for _, test := range tests {
		t.Run(strconv.FormatInt(test.size, 10), func(t *testing.T) {
			if got := readSize(test.size); got != test.want {
				t.Fatalf("readSize(%d) = %q, want %q", test.size, got, test.want)
			}
		})
	}
}
