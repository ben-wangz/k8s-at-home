package prepare

import "syscall"

// syscallStat is the platform stat type used for ownership checks.
type syscallStat = syscall.Stat_t
