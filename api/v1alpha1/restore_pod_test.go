// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func restorePodFixture() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "restore-worker",
			Namespace:   "inference",
			Annotations: map[string]string{"example.com/team": "inference"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "main",
					Image:   "worker:latest",
					Command: []string{"python3"},
					Args:    []string{"serve.py"},
				},
				{Name: "sidecar", Image: "sidecar:latest"},
			},
		},
	}
}

func singleRestoreMapping() []RestoreContainerMapping {
	return []RestoreContainerMapping{{Source: "main", Destination: "main"}}
}

func TestBuildRestorePodShapesSingleDestination(t *testing.T) {
	original := restorePodFixture()
	before := original.DeepCopy()
	options := RestorePodOptions{SeccompProfile: DefaultSeccompLocalhostProfile}

	shaped, err := BuildRestorePod(original, "snapshot-a", singleRestoreMapping(), options)
	if err != nil {
		t.Fatalf("BuildRestorePod() failed: %v", err)
	}
	if !reflect.DeepEqual(original, before) {
		t.Fatal("BuildRestorePod() mutated its input")
	}
	if shaped.Annotations[RestoreFromAnnotation] != "snapshot-a" {
		t.Fatalf("%s = %q", RestoreFromAnnotation, shaped.Annotations[RestoreFromAnnotation])
	}
	if _, found := shaped.Annotations[RestoreContainerMapAnnotation]; found {
		t.Fatalf("same-name restore unexpectedly set %s", RestoreContainerMapAnnotation)
	}
	if shaped.Annotations["example.com/team"] != "inference" {
		t.Fatal("unrelated annotation was not preserved")
	}
	if len(shaped.Spec.Volumes) != 1 || shaped.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("expected one snapshot control emptyDir, got %#v", shaped.Spec.Volumes)
	}

	main := &shaped.Spec.Containers[0]
	if !reflect.DeepEqual(main.Command, []string{"python3"}) || !reflect.DeepEqual(main.Args, []string{"serve.py"}) {
		t.Fatalf("workload command changed: command=%v args=%v", main.Command, main.Args)
	}
	if len(main.VolumeMounts) != 1 || main.VolumeMounts[0].SubPath != "main" {
		t.Fatalf("unexpected snapshot control mount: %#v", main.VolumeMounts)
	}
	for _, name := range []string{SnapshotControlDirEnv, LegacySnapshotControlDirEnv} {
		if got := restoreEnvValue(main.Env, name); got != SnapshotControlMountPath {
			t.Fatalf("%s = %q, want %q", name, got, SnapshotControlMountPath)
		}
	}
	if got := restoreEnvValue(main.Env, "DYN_SNAPSHOT_RESTORE_STANDBY"); got != "" {
		t.Fatalf("generic builder injected workload-specific standby value %q", got)
	}
	if main.StartupProbe == nil || main.StartupProbe.Exec == nil ||
		!reflect.DeepEqual(main.StartupProbe.Exec.Command, []string{"cat", SnapshotControlMountPath + "/" + RestoreCompleteFile}) {
		t.Fatalf("unexpected restore startup gate: %#v", main.StartupProbe)
	}
	if shaped.Spec.SecurityContext == nil ||
		!matchesLocalhostSeccompProfile(shaped.Spec.SecurityContext.SeccompProfile, DefaultSeccompLocalhostProfile) {
		t.Fatalf("missing expected seccomp profile: %#v", shaped.Spec.SecurityContext)
	}
	if !reflect.DeepEqual(shaped.Spec.Containers[1], before.Spec.Containers[1]) {
		t.Fatal("non-destination sidecar was modified")
	}
	if err := ValidateRestorePod(shaped, "snapshot-a", singleRestoreMapping(), options); err != nil {
		t.Fatalf("ValidateRestorePod() rejected builder output: %v", err)
	}
}

func TestBuildRestorePodShapesFanoutIdempotently(t *testing.T) {
	pod := restorePodFixture()
	pod.Spec.Containers = []corev1.Container{{Name: "engine-0"}, {Name: "engine-1"}}
	mappings := []RestoreContainerMapping{
		{Source: "main", Destination: "engine-0"},
		{Source: "main", Destination: "engine-1"},
	}

	first, err := BuildRestorePod(pod, "snapshot-a", mappings, RestorePodOptions{})
	if err != nil {
		t.Fatalf("first BuildRestorePod() failed: %v", err)
	}
	second, err := BuildRestorePod(first, "snapshot-a", mappings, RestorePodOptions{})
	if err != nil {
		t.Fatalf("second BuildRestorePod() failed: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("BuildRestorePod() is not idempotent")
	}
	if got := first.Annotations[RestoreContainerMapAnnotation]; got != "main=engine-0,main=engine-1" {
		t.Fatalf("%s = %q", RestoreContainerMapAnnotation, got)
	}
	if len(first.Spec.Volumes) != 1 {
		t.Fatalf("expected one shared volume, got %d", len(first.Spec.Volumes))
	}
	for i, name := range []string{"engine-0", "engine-1"} {
		container := &first.Spec.Containers[i]
		if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].SubPath != name {
			t.Fatalf("container %q mount = %#v", name, container.VolumeMounts)
		}
	}
}

func TestBuildRestorePodPreservesWorkloadProbes(t *testing.T) {
	pod := restorePodFixture()
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/live"}},
		PeriodSeconds: 10,
	}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready"}},
		PeriodSeconds: 5,
	}
	beforeLiveness := pod.Spec.Containers[0].LivenessProbe.DeepCopy()
	beforeReadiness := pod.Spec.Containers[0].ReadinessProbe.DeepCopy()

	shaped, err := BuildRestorePod(pod, "snapshot-a", singleRestoreMapping(), RestorePodOptions{})
	if err != nil {
		t.Fatalf("BuildRestorePod() failed: %v", err)
	}
	main := &shaped.Spec.Containers[0]
	if !reflect.DeepEqual(main.LivenessProbe, beforeLiveness) || !reflect.DeepEqual(main.ReadinessProbe, beforeReadiness) {
		t.Fatal("workload liveness or readiness probe was modified")
	}
	if main.StartupProbe == nil || main.StartupProbe.HTTPGet == nil || main.StartupProbe.HTTPGet.Path != "/live" {
		t.Fatalf("liveness handler was not copied into the restore startup gate: %#v", main.StartupProbe)
	}
	if main.StartupProbe.InitialDelaySeconds != 0 || main.StartupProbe.PeriodSeconds != 1 ||
		main.StartupProbe.FailureThreshold != restoreStartupFailureThreshold || main.StartupProbe.SuccessThreshold != 1 {
		t.Fatalf("restore startup gate timing is incorrect: %#v", main.StartupProbe)
	}
}

func TestBuildRestorePodCanonicalizesEquivalentMapping(t *testing.T) {
	pod := restorePodFixture()
	pod.Spec.Containers = []corev1.Container{{Name: "engine-0"}, {Name: "engine-1"}}
	pod.Annotations[RestoreContainerMapAnnotation] = "main=engine-1, main=engine-0"
	mappings := []RestoreContainerMapping{
		{Source: "main", Destination: "engine-0"},
		{Source: "main", Destination: "engine-1"},
	}

	shaped, err := BuildRestorePod(pod, "snapshot-a", mappings, RestorePodOptions{})
	if err != nil {
		t.Fatalf("BuildRestorePod() failed: %v", err)
	}
	if got := shaped.Annotations[RestoreContainerMapAnnotation]; got != "main=engine-0,main=engine-1" {
		t.Fatalf("canonical mapping = %q", got)
	}
}

func TestBuildRestorePodRejectsConflictsAtomically(t *testing.T) {
	runtimeDefault := corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	tests := []struct {
		name     string
		mutate   func(*corev1.Pod)
		mappings []RestoreContainerMapping
	}{
		{
			name: "restore annotation",
			mutate: func(pod *corev1.Pod) {
				pod.Annotations[RestoreFromAnnotation] = "snapshot-b"
			},
		},
		{
			name: "mapping annotation",
			mutate: func(pod *corev1.Pod) {
				pod.Annotations[RestoreContainerMapAnnotation] = "main=sidecar"
			},
		},
		{
			name: "control volume",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Volumes = []corev1.Volume{{
					Name: SnapshotControlVolumeName,
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "wrong",
					}},
				}}
			},
		},
		{
			name: "control mount name",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
					Name: SnapshotControlVolumeName, MountPath: "/wrong", SubPath: "main",
				}}
			},
		},
		{
			name: "control mount path",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
					Name: "other", MountPath: SnapshotControlMountPath,
				}}
			},
		},
		{
			name: "control environment",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: SnapshotControlDirEnv, Value: "/wrong"}}
			},
		},
		{
			name: "startup probe",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					Exec:    &corev1.ExecAction{Command: []string{"true"}},
					HTTPGet: &corev1.HTTPGetAction{Path: "/ready"},
				}}
			},
		},
		{
			name: "pod seccomp",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: runtimeDefault.DeepCopy()}
			},
		},
		{
			name: "container seccomp",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{SeccompProfile: runtimeDefault.DeepCopy()}
			},
		},
		{
			name:     "missing destination",
			mappings: []RestoreContainerMapping{{Source: "main", Destination: "missing"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := restorePodFixture()
			if test.mutate != nil {
				test.mutate(pod)
			}
			before := pod.DeepCopy()
			mappings := test.mappings
			if mappings == nil {
				mappings = singleRestoreMapping()
			}

			if _, err := BuildRestorePod(
				pod,
				"snapshot-a",
				mappings,
				RestorePodOptions{SeccompProfile: DefaultSeccompLocalhostProfile},
			); err == nil {
				t.Fatal("BuildRestorePod() unexpectedly succeeded")
			}
			if !reflect.DeepEqual(pod, before) {
				t.Fatal("BuildRestorePod() mutated input after failure")
			}
		})
	}
}

func TestValidateRestorePodRejectsContractDrift(t *testing.T) {
	options := RestorePodOptions{SeccompProfile: DefaultSeccompLocalhostProfile}
	valid, err := BuildRestorePod(restorePodFixture(), "snapshot-a", singleRestoreMapping(), options)
	if err != nil {
		t.Fatalf("BuildRestorePod() failed: %v", err)
	}
	tests := map[string]func(*corev1.Pod){
		"annotation": func(pod *corev1.Pod) {
			delete(pod.Annotations, RestoreFromAnnotation)
		},
		"volume": func(pod *corev1.Pod) {
			pod.Spec.Volumes = nil
		},
		"mount": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].VolumeMounts[0].SubPath = "other"
		},
		"environment": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Env = removeRestoreEnv(pod.Spec.Containers[0].Env, SnapshotControlDirEnv)
		},
		"startup gate": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].StartupProbe.PeriodSeconds = 2
		},
		"seccomp": func(pod *corev1.Pod) {
			pod.Spec.SecurityContext.SeccompProfile = nil
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			pod := valid.DeepCopy()
			mutate(pod)
			if err := ValidateRestorePod(pod, "snapshot-a", singleRestoreMapping(), options); err == nil {
				t.Fatal("ValidateRestorePod() unexpectedly succeeded")
			}
		})
	}
}

func TestBuildRestorePodAllowsUnmanagedSeccomp(t *testing.T) {
	pod := restorePodFixture()
	shaped, err := BuildRestorePod(pod, "snapshot-a", singleRestoreMapping(), RestorePodOptions{})
	if err != nil {
		t.Fatalf("BuildRestorePod() failed: %v", err)
	}
	if shaped.Spec.SecurityContext != nil {
		t.Fatalf("empty option unexpectedly changed security context: %#v", shaped.Spec.SecurityContext)
	}
}

func restoreEnvValue(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name {
			return item.Value
		}
	}
	return ""
}

func removeRestoreEnv(env []corev1.EnvVar, name string) []corev1.EnvVar {
	result := make([]corev1.EnvVar, 0, len(env))
	for _, item := range env {
		if item.Name != name {
			result = append(result, item)
		}
	}
	return result
}
