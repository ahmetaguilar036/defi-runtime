# defi-runtime

Captures and archives goroutine stack traces on demand or on an interval into a rotating set of files for post-mortem analysis.

```go
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	defiruntime "github.com/example/defi-runtime"
)

func main() {
	d := defiruntime.NewDumper("/var/log/defi-runtime", 10, nil)

	stop := make(chan struct{})
	go d.Run(stop, 30*time.Second, func(err error) {
		log.Println("capture failed:", err)
	})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1, syscall.SIGTERM)
	<-sig
	close(stop)
}
```

Each capture is written as `goroutines-<UTC timestamp>-<counter>.txt`. Because the timestamp is zero-padded and the counter is fixed-width, lexical order of the file names is chronological order, which is what the pruner relies on.

## Why this exists

When a long-running service wedges, the interesting evidence is usually the goroutine dump, and by the time an operator attaches a debugger or sends a signal the process may already be gone. The alternative is to keep `runtime/pprof` running continuously, but that produces binary profiles aimed at a different question. This library takes the narrow position that a plain-text stack dump, written on a schedule and on demand, is enough for most incidents.

The trade-off is deliberate: each capture is a full stop-the-world `runtime.Stack` call, so the interval should be measured in tens of seconds, not milliseconds. The dumper also does not compress or rotate by size, only by count. That keeps the code small and the on-disk format trivially readable with `cat`, at the cost of disk usage when dumps are large.

## The awkward edge

`Capture` serializes on an internal mutex, and the prune step runs while that mutex is held. If the capture directory lives on a slow filesystem, a capture blocks every other capture for the duration of the write and the directory scan. That is the correct behaviour for correctness — two concurrent captures must never race on the counter or the prune — but it means you should not call `Capture` from a latency-sensitive path. Use `Run` and let the background goroutine absorb the cost.

A second edge: retention is by count only. Setting `keep` to zero disables pruning entirely rather than keeping zero files, because "keep nothing" is never what an operator actually wants.

## API

- `NewDumper(dir string, keep int, clock Clock) *Dumper` — `clock` may be nil, in which case `time.Now` is used.
- `(*Dumper) Capture() (string, error)` — writes one dump and returns its path.
- `(*Dumper) List() ([]string, error)` — capture names, oldest first; returns `nil, nil` if the directory does not exist yet.
- `(*Dumper) Run(stop <-chan struct{}, interval time.Duration, onErr func(error))` — captures immediately, then on each tick until `stop` is closed.
- `type Clock func() time.Time`
