package landing

import (
	"os"
	"syscall"
	"time"
)

// birth returns when a file was created.
func birth(fi os.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec), true
}
