package monitor

import "errors"

// lockFile is the data directory's lock, held by the running monitor.
const lockFile = "stream-analyzer.lock"

// errCannotLock marks a data directory on a filesystem without locking,
// such as some network mounts.
var errCannotLock = errors.New("the filesystem doesn't support locking")
