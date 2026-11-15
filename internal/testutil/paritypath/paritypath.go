// Package paritypath creates parity-replay directories whose paths have the
// same length as the native capture's, so output that contains the working
// directory has the same size, and the same preview cut, in both.
package paritypath

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// LengthEnv names the byte length of the native capture's working directory,
// which the differential runner exports for external replays.
const LengthEnv = "CIRCUS_PARITY_CWD_LENGTH"

// Mkdir creates a new directory in base for a replay whose working directory
// is that directory followed by suffix. With LengthEnv unset it behaves like
// os.MkdirTemp(base, prefix+"*"); otherwise the name is random, without the
// prefix, and sized so that the working directory path is exactly LengthEnv
// bytes long.
func Mkdir(base, prefix, suffix string) (string, error) {
	value := os.Getenv(LengthEnv)
	if value == "" {
		return os.MkdirTemp(base, prefix+"*")
	}
	length, err := strconv.Atoi(value)
	if err != nil {
		return "", fmt.Errorf("%s: %w", LengthEnv, err)
	}
	base = filepath.Clean(base)
	random := length - len(base) - 1 - len(suffix)
	if random < 4 {
		return "", fmt.Errorf("%s=%d leaves %d random name bytes below %s", LengthEnv, length, random, base)
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	for range 100 {
		var name []byte
		for len(name) < random {
			name = append(name, rand.Text()...)
		}
		for i := range name {
			name[i] = alphabet[int(name[i])%len(alphabet)]
		}
		dir := filepath.Join(base, string(name[:random]))
		if err := os.Mkdir(dir, 0o700); err == nil {
			return dir, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("no unused directory name of length %d below %s", random, base)
}
