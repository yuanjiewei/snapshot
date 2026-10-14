// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
)

// A lock whose helper never returns (#592) fails on its own bound with no deadline from the caller,
// passes that bound to the driver as --timeout, and says so in two lines: one
// at start naming the argv and the libcuda the helper mapped, one at the end
// with timed_out and the duration.
func TestALockThatNeverReturnsIsBoundedAndSaysSo(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	installFakeCUDAHelper(t, fmt.Sprintf("echo \"$@\" > %s\nsleep 300 &\nwait\n", argv))
	savedLock, savedGrace := lockTimeout, helperKillGrace
	lockTimeout, helperKillGrace = 200*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { lockTimeout, helperKillGrace = savedLock, savedGrace })
	var mu sync.Mutex
	var lines []string
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{})

	result := make(chan error, 1)
	started := time.Now()
	go func() { result <- lock(context.Background(), 11, log) }()
	var err error
	select {
	case err = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("lock() has not returned after 10s with a 300ms bound: a hung lock holds the capture until someone deletes the Pod")
	}
	if duration := time.Since(started); duration > lockTimeout+helperKillGrace+helperWaitDelay+time.Second {
		t.Fatalf("lock() returned after %s, want within its bound", duration)
	}
	if !errors.Is(err, errHelperTimedOut) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock() error = %v, want %v", err, errHelperTimedOut)
	}
	if got, readErr := os.ReadFile(argv); readErr != nil || strings.TrimSpace(string(got)) != "--action lock --pid 11 --timeout 200" {
		t.Fatalf("helper argv %q (%v), want the driver timeout passed as --timeout 200", got, readErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 || !strings.Contains(lines[0], `"cuda-checkpoint-helper started"`) || !strings.Contains(lines[0], `"--timeout" "200"`) ||
		!strings.Contains(lines[0], `"libcuda"`) || !strings.Contains(lines[1], `"timed_out"=true`) || !strings.Contains(lines[1], `"duration"`) {
		t.Fatalf("log lines %q, want a start line with the argv and libcuda, then a timed_out=true line with the duration", lines)
	}
}

// The start line names the libcuda the helper actually mapped, resolved and
// versioned, e.g. an image's CUDA forward-compat copy rather than the host's.
func TestLibcudaFromMapsNamesTheMappedFileAndItsVersion(t *testing.T) {
	maps := strings.Join([]string{
		"55d0c0a00000-55d0c0a02000 r--p 00000000 00:2e 1234 /usr/local/bin/cuda-checkpoint-helper",
		"7f1c2a000000-7f1c2a2a0000 r--p 00000000 00:2e 5678 /usr/local/cuda/compat/lib.real/libcuda.so.580.95.05",
		"7f1c2b000000-7f1c2b020000 r--p 00000000 00:2e 9012 /usr/lib/x86_64-linux-gnu/libc.so.6",
		"7ffd0e000000-7ffd0e021000 rw-p 00000000 00:00 0 [stack]",
	}, "\n")
	path, version := libcudaFromMaps(strings.NewReader(maps))
	if path != "/usr/local/cuda/compat/lib.real/libcuda.so.580.95.05" || version != "580.95.05" {
		t.Fatalf("libcudaFromMaps = %q, %q", path, version)
	}
}
