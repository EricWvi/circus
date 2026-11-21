package workspace

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
)

// maxMountinfoBytes bounds how much of /proc/self/mountinfo is read.
const maxMountinfoBytes = 16 << 20

// mountBelow returns a mount point strictly below dir, as listed in
// /proc/self/mountinfo, so a removal can refuse before deleting anything.
// It is a courtesy: the walk itself never crosses a mount. An unreadable
// mountinfo reports none.
func mountBelow(dir string) string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	prefix := strings.TrimSuffix(dir, "/") + "/"
	scanner := bufio.NewScanner(io.LimitReader(f, maxMountinfoBytes))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		if point := unescapeMountinfo(fields[4]); strings.HasPrefix(point, prefix) {
			return point
		}
	}
	return ""
}

// unescapeMountinfo decodes the octal escapes (\040 and the like) that
// mountinfo uses for spaces, tabs, newlines and backslashes.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
