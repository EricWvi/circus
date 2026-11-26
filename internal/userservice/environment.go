package userservice

import (
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/circushome"
)

// recorded are the variables every service records from the installing
// shell, besides the locale (LANG and LC_*).
var recorded = []string{
	"PATH", circushome.Env, "HOME", "USER", "SHELL", "LANG",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// Environment returns the current shell's settings a service records, as
// sorted KEY=VALUE entries: PATH, CIRCUS_HOME (made absolute), HOME, USER,
// SHELL, the locale, the proxy variables, the XDG base directories, the
// TLS certificate locations and the extra variables named. Unset
// variables are left out; skipped names variables whose values a service
// file can't hold (control characters or invalid UTF-8).
func Environment(extra ...string) (env, skipped []string, err error) {
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if !slices.Contains(recorded, key) && !slices.Contains(extra, key) && !strings.HasPrefix(key, "LC_") {
			continue
		}
		if key == circushome.Env && validEnvironmentValue(value) {
			value, err = circushome.Root()
			if err != nil {
				return nil, nil, err
			}
			entry = key + "=" + value
		}
		if !validEnvironmentValue(value) {
			skipped = append(skipped, key)
			continue
		}
		env = append(env, entry)
	}
	slices.Sort(env)
	slices.Sort(skipped)
	return env, skipped, nil
}

func validEnvironmentValue(value string) bool {
	return utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) == -1
}
