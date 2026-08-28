// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import corev1 "k8s.io/api/core/v1"

// RestoreOutcome is the stable, consumer-facing state of a restore request.
type RestoreOutcome string

const (
	// RestoreOutcomePending means restore has not reached a terminal outcome.
	// It includes dependency waits and active restore execution.
	RestoreOutcomePending RestoreOutcome = "Pending"
	// RestoreOutcomeSucceeded means every requested destination was restored.
	RestoreOutcomeSucceeded RestoreOutcome = "Succeeded"
	// RestoreOutcomeFailed means no requested destination was restored.
	RestoreOutcomeFailed RestoreOutcome = "Failed"
	// RestoreOutcomePartiallySucceeded means some, but not all, requested
	// destinations were restored.
	RestoreOutcomePartiallySucceeded RestoreOutcome = "PartiallySucceeded"
)

// Stable reasons used on the nvidia.com/Restored Pod condition. Dependency-wait
// reasons remain agent-internal; consumers should use ClassifyRestoreOutcome.
const (
	// RestoreReasonInProgress marks active restore execution.
	RestoreReasonInProgress = "RestoreInProgress"
	// RestoreReasonSucceeded marks a terminal all-destinations success.
	RestoreReasonSucceeded = "RestoreSucceeded"
	// RestoreReasonFailed marks a terminal all-destinations failure.
	RestoreReasonFailed = "RestoreFailed"
	// RestoreReasonPartiallySucceeded marks a terminal mixed destination outcome.
	RestoreReasonPartiallySucceeded = "RestorePartiallySucceeded"
)

// ClassifyRestoreOutcome returns the public restore outcome represented by Pod
// conditions. A missing Restored condition, an unknown status, or any
// nonterminal false reason is Pending.
func ClassifyRestoreOutcome(conditions []corev1.PodCondition) RestoreOutcome {
	for _, condition := range conditions {
		if condition.Type != corev1.PodConditionType(RestoredCondition) {
			continue
		}
		if condition.Status == corev1.ConditionTrue {
			return RestoreOutcomeSucceeded
		}
		if condition.Status != corev1.ConditionFalse {
			return RestoreOutcomePending
		}
		switch condition.Reason {
		case RestoreReasonFailed:
			return RestoreOutcomeFailed
		case RestoreReasonPartiallySucceeded:
			return RestoreOutcomePartiallySucceeded
		default:
			return RestoreOutcomePending
		}
	}
	return RestoreOutcomePending
}
