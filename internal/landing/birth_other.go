//go:build !darwin

package landing

import (
	"os"
	"time"
)

// birth is not known on this system; a note then counts only as changed
// (its creation time is read as zero).
func birth(os.FileInfo) (time.Time, bool) { return time.Time{}, false }
