package daemon

import "github.com/EricWvi/circus/internal/clock"

// Clock supplies time and timers for execution deadlines and retention.
type Clock = clock.Clock

type realClock = clock.Real
