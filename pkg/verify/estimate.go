package verify

import (
	"runtime"
	"time"
)

// Estimate is what a verify of a set of packages is expected to cost, from the last check time
// of each package (the durations history verify keeps). Cached packages are not counted out: a
// cache hit is rare when the change reaches this many packages (they all take a new input).
type Estimate struct {
	Packages int           // packages to check
	Timed    int           // of which a last check time is known
	Serial   time.Duration // sum of the known check times
	Wall     time.Duration // Serial spread over verify's parallelism, untimed packages at the timed average
}

// EstimateVerify estimates checking names in the repository at root.
func EstimateVerify(root string, names []string) Estimate {
	times := loadDurations(root)
	e := Estimate{Packages: len(names)}
	for _, n := range names {
		if ms := times[n]; ms > 0 {
			e.Timed++
			e.Serial += time.Duration(ms * float64(time.Millisecond))
		}
	}
	if e.Timed == 0 {
		return e
	}
	total := e.Serial + time.Duration(int64(e.Serial)/int64(e.Timed)*int64(e.Packages-e.Timed))
	workers := min(max(runtime.NumCPU(), 2), 8) // as VerifyPackages
	e.Wall = total / time.Duration(min(workers, e.Packages))
	return e
}
