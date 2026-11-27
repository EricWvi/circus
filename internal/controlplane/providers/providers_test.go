package providers_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/controlplane/providers"
	"github.com/EricWvi/circus/internal/store"
)

func newBox(t *testing.T) *providers.Box {
	t.Helper()
	b, err := providers.NewBox(bytes.Repeat([]byte{7}, providers.KeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var secrets = providers.Secrets{Key: "sk-fake-0123456789", Env: []providers.EnvValue{{Name: "FOO", Value: "foo-value"}, {Name: "EMPTY", Value: ""}}}

func TestSealOpen(t *testing.T) {
	b := newBox(t)
	sealed, err := b.Seal("p1", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sk-fake")) || bytes.Contains(sealed, []byte("foo-value")) {
		t.Fatal("sealed blob holds plaintext")
	}
	got, err := b.Open("p1", sealed)
	if err != nil || !got.Equal(secrets) {
		t.Fatalf("open = %v, %v", got.Key == secrets.Key, err)
	}
	// A fresh nonce every time.
	again, err := b.Seal("p1", secrets)
	if err != nil || bytes.Equal(again, sealed) || bytes.Equal(again[:12], sealed[:12]) {
		t.Fatalf("second seal reused its nonce: %v", err)
	}
	// The provider id is bound: the blob does not open under another id.
	if _, err := b.Open("p2", sealed); !errors.Is(err, providers.ErrUnreadable) {
		t.Fatalf("open under another id: %v", err)
	}
	// Tampering, truncation and another key are all unreadable.
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	other, err := providers.NewBox(bytes.Repeat([]byte{8}, providers.KeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]func() error{
		"tampered":  func() error { _, err := b.Open("p1", tampered); return err },
		"truncated": func() error { _, err := b.Open("p1", sealed[:20]); return err },
		"empty":     func() error { _, err := b.Open("p1", nil); return err },
		"other key": func() error { _, err := other.Open("p1", sealed); return err },
	} {
		if err := open(); !errors.Is(err, providers.ErrUnreadable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := providers.NewBox(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte key was accepted")
	}
}

func TestOpenProviderChecksNames(t *testing.T) {
	b := newBox(t)
	sealed, err := b.Seal("p1", secrets)
	if err != nil {
		t.Fatal(err)
	}
	p := store.Provider{ID: "p1", Secrets: sealed, EnvNames: []string{"FOO", "EMPTY"}}
	if !b.Readable(p) {
		t.Fatal("matching provider unreadable")
	}
	p.EnvNames = []string{"EMPTY", "FOO"}
	if b.Readable(p) {
		t.Fatal("variables out of order read")
	}
	var nilBox *providers.Box
	if nilBox.Readable(p) {
		t.Fatal("nil box read")
	}
}

func TestSecretsNeverFormat(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log.Debug("secrets", "s", secrets)
	out := buf.String() + fmt.Sprint(secrets) + fmt.Sprintf("%v %+v %#v %s", secrets, secrets, secrets, secrets)
	for _, v := range []string{secrets.Key, "foo-value"} {
		if strings.Contains(out, v) {
			t.Fatalf("formatted secrets leak %q: %s", v, out)
		}
	}
}

func TestLaunchEnv(t *testing.T) {
	x := store.Provider{Scheme: store.ProviderSchemeXAPIKey}
	if got, want := providers.LaunchEnv(x, providers.Secrets{Key: "k1"}), []providers.Var{{"ANTHROPIC_API_KEY", "k1"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("x-api-key = %v", got)
	}
	bearer := store.Provider{Scheme: store.ProviderSchemeBearer, BaseURL: "https://gw.example/v1", FableModel: "f", OpusModel: "o", HaikuModel: "h", EnvNames: []string{"FOO", "EMPTY"}}
	want := []providers.Var{
		{"ANTHROPIC_BASE_URL", "https://gw.example/v1"}, {"ANTHROPIC_AUTH_TOKEN", secrets.Key},
		{"ANTHROPIC_DEFAULT_FABLE_MODEL", "f"}, {"ANTHROPIC_DEFAULT_OPUS_MODEL", "o"}, {"ANTHROPIC_DEFAULT_HAIKU_MODEL", "h"},
		{"FOO", "foo-value"}, {"EMPTY", ""},
	}
	if got := providers.LaunchEnv(bearer, secrets); !reflect.DeepEqual(got, want) {
		t.Fatalf("bearer = %v", got)
	}
}

func TestCheckEnvName(t *testing.T) {
	for name, ok := range map[string]bool{
		"FOO": true, "_X": true, "A1_B2": true, strings.Repeat("A", 128): true,
		"": false, "1A": false, "foo": false, "A-B": false, "A B": false, strings.Repeat("A", 129): false,
		"HOME": false, "PATH": false, "CLAUDE_CONFIG_DIR": false, "CLAUDE_CODE_OAUTH_TOKEN": false,
		"CIRCUS_TOKEN": false, "CIRCUS": true, "ANTHROPIC_API_KEY": false, "ANTHROPIC_BASE_URL": false,
		"ANTHROPIC_AUTH_TOKEN": false, "ANTHROPIC_DEFAULT_FABLE_MODEL": false, "ANTHROPIC_DEFAULT_OPUS_MODEL": false, "ANTHROPIC_DEFAULT_SONNET_MODEL": false,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL": false, "ANTHROPIC_OTHER": true, "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": false,
		"CLAUDE_CODE_USE_BEDROCK": true,
	} {
		if got := providers.CheckEnvName(name) == ""; got != ok {
			t.Errorf("CheckEnvName(%q) ok = %v, want %v", name, got, ok)
		}
	}
}

func TestLast4(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "abc", "abcdef": "cdef", "ключики": "чики"} {
		if got := providers.Last4(in); got != want {
			t.Errorf("Last4(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyFile(t *testing.T) {
	dir := t.TempDir()
	if got := providers.KeyPath(filepath.Join(dir, "circus.db"), ""); got != filepath.Join(dir, "provider.key") {
		t.Fatalf("default key path %s", got)
	}
	if got := providers.KeyPath(filepath.Join(dir, "circus.db"), "/etc/k"); got != "/etc/k" {
		t.Fatalf("override key path %s", got)
	}
	path := filepath.Join(dir, "provider.key")
	key, err := providers.LoadOrCreateKey(path)
	if err != nil || len(key) != providers.KeyBytes {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != providers.KeyBytes {
		t.Fatalf("key file %v %v", info, err)
	}
	again, err := providers.LoadOrCreateKey(path)
	if err != nil || !bytes.Equal(again, key) {
		t.Fatalf("reload: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("leftover files: %v %v", entries, err)
	}

	// Group or other permissions are refused, as is a wrong size.
	must(t, os.Chmod(path, 0o640))
	if _, err := providers.LoadOrCreateKey(path); err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("0640: %v", err)
	}
	must(t, os.Chmod(path, 0o604))
	if _, err := providers.LoadOrCreateKey(path); err == nil {
		t.Fatal("0604 accepted")
	}
	short := filepath.Join(dir, "short.key")
	must(t, os.WriteFile(short, []byte("short"), 0o600))
	if _, err := providers.LoadOrCreateKey(short); err == nil || !strings.Contains(err.Error(), "exactly 32 bytes") {
		t.Fatalf("short key: %v", err)
	}
	must(t, os.Mkdir(filepath.Join(dir, "adir"), 0o700))
	if _, err := providers.LoadOrCreateKey(filepath.Join(dir, "adir")); err == nil {
		t.Fatal("a directory was accepted")
	}
	if _, err := providers.LoadOrCreateKey(filepath.Join(dir, "missing", "k")); err == nil {
		t.Fatal("a key in a missing directory was created")
	}
}

// TestKeyFileConcurrentCreate: concurrent first starts agree on one key.
func TestKeyFileConcurrentCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.key")
	keys := make([][]byte, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Go(func() {
			k, err := providers.LoadOrCreateKey(path)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		})
	}
	wg.Wait()
	for _, k := range keys[1:] {
		if !bytes.Equal(k, keys[0]) {
			t.Fatal("concurrent creations disagree")
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
