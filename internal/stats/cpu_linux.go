package stats

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// procCPU returns the process's CPU time (user + system), read from /proc/<pid>/stat
// (utime and stime, in 1/100 s ticks).
func procCPU(pid int) (time.Duration, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// the process name is in parentheses and may contain spaces
	s := string(raw)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseInt(f[11], 10, 64)
	stime, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return time.Duration(utime+stime) * 10 * time.Millisecond, true
}
