package daemonservice

import (
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/circushome"
)

func serviceEnvironment() (env, skipped []string, err error) {
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "PATH", circushome.Env, "HOME", "USER", "SHELL", "LANG",
			"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
			"http_proxy", "https_proxy", "all_proxy", "no_proxy",
			"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
			"SSL_CERT_FILE", "SSL_CERT_DIR":
		default:
			if !strings.HasPrefix(key, "LC_") {
				continue
			}
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
