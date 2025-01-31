package werf

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/werf/lockgate"
	"github.com/werf/werf/pkg/util/timestamps"
)

func getWerfLastRunAtPath() string {
	return filepath.Join(GetServiceDir(), "var", "last_werf_run_at")
}

func getWerfFirstRunAtPath() string {
	return filepath.Join(GetServiceDir(), "var", "first_werf_run_at")
}

func SetWerfLastRunAt(ctx context.Context) error {
	path := getWerfLastRunAtPath()
	if _, lock, err := AcquireHostLock(ctx, path, lockgate.AcquireOptions{OnWaitFunc: func(lockName string, doWait func() error) error { return doWait() }}); err != nil {
		return fmt.Errorf("error locking path %q: %s", path, err)
	} else {
		defer ReleaseHostLock(lock)
	}

	return timestamps.WriteTimestampFile(path, time.Now())
}

func GetWerfLastRunAt(ctx context.Context) (time.Time, error) {
	path := getWerfLastRunAtPath()
	if _, lock, err := AcquireHostLock(ctx, path, lockgate.AcquireOptions{OnWaitFunc: func(lockName string, doWait func() error) error { return doWait() }}); err != nil {
		return time.Time{}, fmt.Errorf("error locking path %q: %s", path, err)
	} else {
		defer ReleaseHostLock(lock)
	}

	return timestamps.ReadTimestampFile(path)
}

func SetWerfFirstRunAt(ctx context.Context) error {
	path := getWerfFirstRunAtPath()
	if _, lock, err := AcquireHostLock(ctx, path, lockgate.AcquireOptions{OnWaitFunc: func(lockName string, doWait func() error) error { return doWait() }}); err != nil {
		return fmt.Errorf("error locking path %q: %s", path, err)
	} else {
		defer ReleaseHostLock(lock)
	}

	if exists, err := timestamps.CheckTimestampFileExists(path); err != nil {
		return fmt.Errorf("error checking existance of %q: %s", path, err)
	} else if !exists {
		return timestamps.WriteTimestampFile(path, time.Now())
	}
	return nil
}

func GetWerfFirstRunAt(ctx context.Context) (time.Time, error) {
	path := getWerfFirstRunAtPath()
	if _, lock, err := AcquireHostLock(ctx, path, lockgate.AcquireOptions{OnWaitFunc: func(lockName string, doWait func() error) error { return doWait() }}); err != nil {
		return time.Time{}, fmt.Errorf("error locking path %q: %s", path, err)
	} else {
		defer ReleaseHostLock(lock)
	}

	return timestamps.ReadTimestampFile(path)
}
