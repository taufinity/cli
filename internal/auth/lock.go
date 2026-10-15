package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RefreshLockMaxWait bounds how long a process waits for another process's
// refresh to finish. It is longer than one complete refresh request (see
// api.refreshHTTPConfig), so a waiter normally gets the lock and then finds the
// token already renewed.
const RefreshLockMaxWait = 60 * time.Second

// lockPollInterval is how often a waiter retries the non-blocking lock.
const lockPollInterval = 50 * time.Millisecond

// LockRefresh takes an exclusive, cross-process lock for renewing the
// credentials in path, and returns the func that releases it.
//
// Why: the server rotates the refresh token on every use and treats a rotated
// token presented again more than a minute later as theft, revoking every CLI
// session of the user. With many taufinity processes sharing one credentials
// file (MCP bridges in every Claude session, scripts calling `auth token`),
// two of them renewing at once used to race. Under the lock, the second one
// re-reads the file, sees the renewed pair, and never sends the old token.
//
// The lock is an OS file lock on path+".lock", so it is released when the
// holder exits, even on a crash; a stale lock file on disk is harmless.
func LockRefresh(ctx context.Context, path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open refresh lock: %w", err)
	}
	deadline := time.Now().Add(RefreshLockMaxWait)
	for {
		ok, lerr := tryLockFile(f)
		if lerr != nil {
			f.Close()
			return nil, fmt.Errorf("refresh lock: %w", lerr)
		}
		if ok {
			return func() {
				_ = unlockFile(f)
				_ = f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another taufinity process has been renewing the session for over %s", RefreshLockMaxWait)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}
