// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

// A lock or checkpoint that never returns is bounded, and says what it ran on.
// Without a bound, a helper call that never returned held the GPU and the
// capture with no log line until the Pod was deleted (#592). Every helper call
// logs one line when it starts (the pid, the argv, and the libcuda the helper
// process actually mapped, see #412) and one when it ends, with the duration
// and whether its bound expired.
var (
	// lockTimeout bounds the lock. Healthy locks took 0.3-1s in our captures:
	// the target is idle at the snapshot marker, so the lock only waits for
	// already-submitted work. 60s is ~60x the slowest seen, and short against
	// a GPU held by a capture that cannot finish. It is passed to the driver as --timeout so the driver can give
	// up cleanly (CUDA_ERROR_TIMEOUT), and the context kills the helper
	// helperKillGrace later in case the driver does not.
	lockTimeout = 60 * time.Second
	// checkpointTimeout bounds the checkpoint, which copies the GPU's memory
	// to the host: 96GiB takes under 7 minutes even at
	// a pessimistic 250MiB/s; whole captures we measured took 25-70s. The
	// helper has no timeout flag for it, so only the context bounds it.
	checkpointTimeout = 10 * time.Minute
	helperKillGrace   = 10 * time.Second
	// libcudaObserveWindow is how long the start line waits for the helper's
	// dynamic loader to map libcuda (normally a few milliseconds).
	libcudaObserveWindow = 2 * time.Second
)

var errHelperTimedOut = errors.New("cuda-checkpoint-helper did not finish within its bound")

// helperBound is the bound of action ("" for none) and the arguments it adds
// to the helper's argv.
func helperBound(action string) (time.Duration, []string) {
	switch action {
	case actionLock:
		return lockTimeout + helperKillGrace, []string{"--timeout", strconv.FormatInt(lockTimeout.Milliseconds(), 10)}
	case actionCheckpoint:
		return checkpointTimeout, nil
	}
	return 0, nil
}

// runHelper runs the helper with args for action on pid: every action of the
// agent funnels through it, so none runs unlogged and none of the bounded
// ones can hang.
//
// ownProcessGroup follows runAction: the agent runs lock and checkpoint
// directly, so they get their own process group and cancellation (a bound
// expiring included) kills their children too. Restore and unlock run inside
// nsrestore and stay in its group, so the agent's kill of that group reaches
// them.
func runHelper(ctx context.Context, pid int, action string, args []string, helperBinaryPath string, ownProcessGroup bool, log logr.Logger) error {
	bound, extra := helperBound(action)
	args = append(args, extra...)
	callerCtx := ctx
	if bound > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bound)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, helperBinaryPath, args...)
	if ownProcessGroup {
		snapshotruntime.SetProcessGroupCancellation(cmd, helperWaitDelay)
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	details := snapshotruntime.ProcessDetails{
		ObservedPID:   pid,
		OutermostPID:  pid,
		InnermostPID:  pid,
		NamespacePIDs: []int{pid},
	}
	if process, err := snapshotruntime.ReadProcessDetails("/proc", pid); err == nil {
		details = process
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		log.Error(err, "cuda-checkpoint-helper did not start", "pid", pid, "action", action, "argv", cmd.Args)
		return fmt.Errorf("cuda-checkpoint-helper %v did not start for pid %d: %w", args, pid, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	libcuda, version, waited := observeLibcuda(cmd.Process.Pid, done)
	log.Info("cuda-checkpoint-helper started",
		"pid", pid,
		"innermost_pid", details.InnermostPID,
		"cmdline", details.Cmdline,
		"action", action,
		"argv", cmd.Args,
		"helper_pid", cmd.Process.Pid,
		"libcuda", libcuda,
		"libcuda_version", version,
		"ld_library_path", os.Getenv("LD_LIBRARY_PATH"),
		"bound", bound,
	)
	var err error
	if waited != nil {
		err = *waited
	} else {
		err = <-done
	}
	duration := time.Since(start)
	out := strings.TrimSpace(output.String())
	if err != nil {
		timedOut := bound > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) && callerCtx.Err() == nil
		switch {
		case timedOut:
			err = fmt.Errorf("%w (%s): %w", errHelperTimedOut, bound, ctx.Err())
		case ctx.Err() != nil:
			err = ctx.Err()
		}
		log.Error(err, "cuda-checkpoint-helper command failed",
			"pid", pid,
			"outermost_pid", details.OutermostPID,
			"innermost_pid", details.InnermostPID,
			"cmdline", details.Cmdline,
			"action", action,
			"timed_out", timedOut,
			"bound", bound,
			"duration", duration,
			"libcuda", libcuda,
			"output", out,
		)
		return fmt.Errorf("cuda-checkpoint-helper %v failed for pid %d after %s: %w (output: %s)", args, pid, duration, err, out)
	}
	log.Info("cuda-checkpoint-helper command succeeded",
		"pid", pid,
		"outermost_pid", details.OutermostPID,
		"innermost_pid", details.InnermostPID,
		"cmdline", details.Cmdline,
		"action", action,
		"duration", duration,
		"output", out,
	)
	return nil
}

// observeLibcuda reads which libcuda helper process helperPID mapped, polling
// its /proc maps until the dynamic loader has mapped one, the helper exits
// (its Wait result is then returned, so the caller does not wait twice), or
// libcudaObserveWindow passes. The path is the file actually mapped, so a
// symlink such as libcuda.so.1 is already resolved to its versioned target.
func observeLibcuda(helperPID int, done <-chan error) (path, version string, exited *error) {
	deadline := time.Now().Add(libcudaObserveWindow)
	for {
		if f, err := os.Open("/proc/" + strconv.Itoa(helperPID) + "/maps"); err == nil {
			path, version = libcudaFromMaps(f)
			f.Close()
			if path != "" {
				return path, version, nil
			}
		}
		select {
		case err := <-done:
			return "not observed: the helper exited first", "", &err
		case <-time.After(2 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return "not observed: not mapped within " + libcudaObserveWindow.String(), "", nil
		}
	}
}

// libcudaFromMaps is the libcuda a /proc/<pid>/maps lists, and its version:
// the file name's suffix after "libcuda.so." ("" for an unversioned name).
func libcudaFromMaps(maps io.Reader) (path, version string) {
	scanner := bufio.NewScanner(maps)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		candidate := strings.Join(fields[5:], " ")
		name := candidate[strings.LastIndex(candidate, "/")+1:]
		if strings.HasPrefix(name, "libcuda.so") {
			return candidate, strings.TrimPrefix(strings.TrimPrefix(name, "libcuda.so"), ".")
		}
	}
	return "", ""
}
