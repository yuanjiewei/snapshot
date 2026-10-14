// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"strconv"

	"github.com/go-logr/logr"
)

func lockWithJobFile(ctx context.Context, pid int, jobFile string, log logr.Logger) error {
	if jobFile == "" {
		return lock(ctx, pid, log)
	}
	return runActionWithJobFile(ctx, pid, actionLock, jobFile, log)
}

func checkpointWithJobFile(ctx context.Context, pid int, jobFile string, log logr.Logger) error {
	if jobFile == "" {
		return checkpoint(ctx, pid, log)
	}
	return runActionWithJobFile(ctx, pid, actionCheckpoint, jobFile, log)
}

func runActionWithJobFile(ctx context.Context, pid int, action, jobFile string, log logr.Logger) error {
	args := []string{"--action", action, "--pid", strconv.Itoa(pid), "--job-file", jobFile}
	return runHelper(ctx, pid, action, args, cudaCheckpointHelperBinary, true, log)
}
