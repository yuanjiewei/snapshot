// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"path"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const restoreStartupFailureThreshold int32 = 1800 // 30 minutes at 1s cadence.

// RestorePodOptions controls generic Snapshot restore Pod shaping. An empty
// SeccompProfile leaves the Pod's seccomp configuration unchanged.
// +kubebuilder:object:generate=false
type RestorePodOptions struct {
	// SeccompProfile is the kubelet-local profile applied at Pod scope. Empty
	// leaves seccomp configuration entirely caller-owned.
	SeccompProfile string
}

// BuildRestorePod returns a restore-shaped deep copy of pod. The caller's Pod
// is never mutated, including when validation fails. Workload-specific standby
// behavior and container commands remain the caller's responsibility. Mapping
// sources must come from the referenced PodSnapshot; this pure builder performs
// no API read to derive them.
func BuildRestorePod(
	pod *corev1.Pod,
	snapshotName string,
	mappings []RestoreContainerMapping,
	options RestorePodOptions,
) (*corev1.Pod, error) {
	if pod == nil {
		return nil, fmt.Errorf("restore pod is nil")
	}
	snapshotName, source, err := validateRestorePodRequest(snapshotName, mappings)
	if err != nil {
		return nil, err
	}
	result := pod.DeepCopy()
	if err := ensureRestoreAnnotations(result, snapshotName, source, mappings); err != nil {
		return nil, err
	}
	if err := ensureRestorePodSpec(&result.Spec, mappings, options); err != nil {
		return nil, err
	}
	if err := validateCanonicalRestorePod(result, snapshotName, mappings, options); err != nil {
		return nil, fmt.Errorf("validate shaped restore pod: %w", err)
	}
	return result, nil
}

// ValidateRestorePod verifies that pod implements the declarative Snapshot
// restore runtime contract for snapshotName and mappings. It accepts supported
// restore-completion gates independently of their probe timing so Pods remain
// valid across builder versions. It performs no API reads and never mutates the
// Pod.
func ValidateRestorePod(
	pod *corev1.Pod,
	snapshotName string,
	mappings []RestoreContainerMapping,
	options RestorePodOptions,
) error {
	return validateRestorePod(pod, snapshotName, mappings, options, validateRestoreStartupProbe)
}

func validateCanonicalRestorePod(
	pod *corev1.Pod,
	snapshotName string,
	mappings []RestoreContainerMapping,
	options RestorePodOptions,
) error {
	return validateRestorePod(pod, snapshotName, mappings, options, validateCanonicalRestoreStartupProbe)
}

func validateRestorePod(
	pod *corev1.Pod,
	snapshotName string,
	mappings []RestoreContainerMapping,
	options RestorePodOptions,
	validateStartupProbe func(*corev1.Container) error,
) error {
	if pod == nil {
		return fmt.Errorf("restore pod is nil")
	}
	snapshotName, source, err := validateRestorePodRequest(snapshotName, mappings)
	if err != nil {
		return err
	}
	if err := validateRestoreAnnotations(pod.Annotations, snapshotName, source, mappings); err != nil {
		return err
	}
	if err := validateControlVolume(&pod.Spec); err != nil {
		return err
	}
	for _, mapping := range mappings {
		container := findContainer(&pod.Spec, mapping.Destination)
		if container == nil {
			return fmt.Errorf("restore pod has no destination container named %q", mapping.Destination)
		}
		if err := validateControlMount(container); err != nil {
			return err
		}
		if err := validateControlEnvironment(container); err != nil {
			return err
		}
		if err := validateStartupProbe(container); err != nil {
			return err
		}
		if err := validateContainerSeccompProfile(container, options.SeccompProfile); err != nil {
			return err
		}
	}
	return validatePodSeccompProfile(&pod.Spec, options.SeccompProfile)
}

func validateRestorePodRequest(snapshotName string, mappings []RestoreContainerMapping) (string, string, error) {
	snapshotName, err := validateRestoreFromSnapshotName(snapshotName)
	if err != nil {
		return "", "", err
	}
	if len(mappings) == 0 {
		return "", "", fmt.Errorf("restore container mapping must contain at least one destination")
	}
	source := mappings[0].Source
	if err := ValidateRestoreContainerMappings(mappings, source); err != nil {
		return "", "", err
	}
	return snapshotName, source, nil
}

func ensureRestoreAnnotations(pod *corev1.Pod, snapshotName, source string, mappings []RestoreContainerMapping) error {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string, 2)
	}
	if existing, found := pod.Annotations[RestoreFromAnnotation]; found {
		resolved, err := validateRestoreFromSnapshotName(existing)
		if err != nil {
			return err
		}
		if resolved != snapshotName {
			return fmt.Errorf("%s names %q, conflicting with requested PodSnapshot %q", RestoreFromAnnotation, resolved, snapshotName)
		}
	}
	pod.Annotations[RestoreFromAnnotation] = snapshotName

	formatted, needsMapping := formatRestoreContainerMappings(mappings)
	if _, found := pod.Annotations[RestoreContainerMapAnnotation]; found {
		existingMappings, err := RestoreContainerMappingsFromAnnotations(pod.Annotations, source)
		if err != nil {
			return err
		}
		if err := ValidateRestoreContainerMappings(existingMappings, source); err != nil {
			return err
		}
		if !sameRestoreContainerMappings(existingMappings, mappings) {
			return fmt.Errorf("%s conflicts with requested restore mappings", RestoreContainerMapAnnotation)
		}
		if needsMapping {
			pod.Annotations[RestoreContainerMapAnnotation] = formatted
		}
		return nil
	}
	if needsMapping {
		pod.Annotations[RestoreContainerMapAnnotation] = formatted
	}
	return nil
}

func validateRestoreAnnotations(annotations map[string]string, snapshotName, source string, mappings []RestoreContainerMapping) error {
	resolved, err := GetRestoreFromSnapshotName(annotations)
	if err != nil {
		return err
	}
	if resolved != snapshotName {
		return fmt.Errorf("%s names %q, expected %q", RestoreFromAnnotation, resolved, snapshotName)
	}
	annotatedMappings, err := RestoreContainerMappingsFromAnnotations(annotations, source)
	if err != nil {
		return err
	}
	if err := ValidateRestoreContainerMappings(annotatedMappings, source); err != nil {
		return err
	}
	if !sameRestoreContainerMappings(annotatedMappings, mappings) {
		return fmt.Errorf("%s does not match the requested restore mappings", RestoreContainerMapAnnotation)
	}
	return nil
}

func formatRestoreContainerMappings(mappings []RestoreContainerMapping) (string, bool) {
	if len(mappings) == 1 && mappings[0].Source == mappings[0].Destination {
		return "", false
	}
	formatted := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		formatted = append(formatted, mapping.Source+"="+mapping.Destination)
	}
	return strings.Join(formatted, ","), true
}

func sameRestoreContainerMappings(left, right []RestoreContainerMapping) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[RestoreContainerMapping]struct{}, len(right))
	for _, mapping := range right {
		want[mapping] = struct{}{}
	}
	for _, mapping := range left {
		if _, found := want[mapping]; !found {
			return false
		}
	}
	return true
}

func ensureRestorePodSpec(spec *corev1.PodSpec, mappings []RestoreContainerMapping, options RestorePodOptions) error {
	if err := ensureControlVolume(spec); err != nil {
		return err
	}
	if err := ensurePodSeccompProfile(spec, options.SeccompProfile); err != nil {
		return err
	}
	for _, mapping := range mappings {
		container := findContainer(spec, mapping.Destination)
		if container == nil {
			return fmt.Errorf("restore pod has no destination container named %q", mapping.Destination)
		}
		if err := validateContainerSeccompProfile(container, options.SeccompProfile); err != nil {
			return err
		}
		if err := ensureControlMount(container); err != nil {
			return err
		}
		if err := ensureControlEnvironment(container); err != nil {
			return err
		}
		ensureRestoreStartupProbe(container)
	}
	return nil
}

func findContainer(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

func ensureControlVolume(spec *corev1.PodSpec) error {
	found, err := hasValidControlVolume(spec)
	if err != nil {
		return err
	}
	if !found {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         SnapshotControlVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	return nil
}

func validateControlVolume(spec *corev1.PodSpec) error {
	found, err := hasValidControlVolume(spec)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("missing %s emptyDir volume", SnapshotControlVolumeName)
	}
	return nil
}

func hasValidControlVolume(spec *corev1.PodSpec) (bool, error) {
	found := false
	for i := range spec.Volumes {
		volume := &spec.Volumes[i]
		if volume.Name != SnapshotControlVolumeName {
			continue
		}
		if found {
			return false, fmt.Errorf("duplicate %s volume", SnapshotControlVolumeName)
		}
		found = true
		if !isEmptyDirVolumeSource(volume.VolumeSource) {
			return false, fmt.Errorf("volume %q must be an emptyDir", SnapshotControlVolumeName)
		}
	}
	return found, nil
}

func isEmptyDirVolumeSource(source corev1.VolumeSource) bool {
	return source.EmptyDir != nil && reflect.DeepEqual(source, corev1.VolumeSource{EmptyDir: source.EmptyDir})
}

func ensureControlMount(container *corev1.Container) error {
	found, err := hasValidControlMount(container)
	if err != nil {
		return err
	}
	if !found {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      SnapshotControlVolumeName,
			MountPath: SnapshotControlMountPath,
			SubPath:   container.Name,
		})
	}
	return nil
}

func validateControlMount(container *corev1.Container) error {
	found, err := hasValidControlMount(container)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("container %q is missing %s mounted at %s", container.Name, SnapshotControlVolumeName, SnapshotControlMountPath)
	}
	return nil
}

func hasValidControlMount(container *corev1.Container) (bool, error) {
	found := false
	for i := range container.VolumeMounts {
		mount := &container.VolumeMounts[i]
		if mount.Name != SnapshotControlVolumeName && mount.MountPath != SnapshotControlMountPath {
			continue
		}
		if found {
			return false, fmt.Errorf("container %q has duplicate snapshot control mounts", container.Name)
		}
		found = true
		if err := validateControlMountValue(container.Name, mount); err != nil {
			return false, err
		}
	}
	return found, nil
}

func validateControlMountValue(containerName string, mount *corev1.VolumeMount) error {
	if mount.Name != SnapshotControlVolumeName || mount.MountPath != SnapshotControlMountPath || mount.SubPath != containerName {
		return fmt.Errorf("container %q requires volume %q mounted at %s with subPath %q", containerName, SnapshotControlVolumeName, SnapshotControlMountPath, containerName)
	}
	if mount.ReadOnly || mount.RecursiveReadOnly != nil || mount.SubPathExpr != "" ||
		(mount.MountPropagation != nil && *mount.MountPropagation != corev1.MountPropagationNone) {
		return fmt.Errorf("container %q has conflicting options on the snapshot control mount", containerName)
	}
	return nil
}

func ensureControlEnvironment(container *corev1.Container) error {
	for _, name := range []string{SnapshotControlDirEnv, LegacySnapshotControlDirEnv} {
		if err := ensureControlEnv(container, name); err != nil {
			return err
		}
	}
	return nil
}

func ensureControlEnv(container *corev1.Container, name string) error {
	found, err := hasValidControlEnv(container, name)
	if err != nil {
		return err
	}
	if !found {
		container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: SnapshotControlMountPath})
	}
	return nil
}

func validateControlEnvironment(container *corev1.Container) error {
	found, err := hasValidControlEnv(container, SnapshotControlDirEnv)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("container %q is missing %s environment variable", container.Name, SnapshotControlDirEnv)
	}
	if _, err := hasValidControlEnv(container, LegacySnapshotControlDirEnv); err != nil {
		return err
	}
	return nil
}

func hasValidControlEnv(container *corev1.Container, name string) (bool, error) {
	found := false
	for i := range container.Env {
		env := &container.Env[i]
		if env.Name != name {
			continue
		}
		if found {
			return false, fmt.Errorf("container %q has duplicate %s environment variables", container.Name, name)
		}
		found = true
		if env.Value != SnapshotControlMountPath || env.ValueFrom != nil {
			return false, fmt.Errorf("container %q has conflicting %s environment variable", container.Name, name)
		}
	}
	return found, nil
}

func ensureRestoreStartupProbe(container *corev1.Container) {
	container.StartupProbe = canonicalRestoreStartupProbe()
}

func canonicalRestoreStartupProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"cat", path.Join(SnapshotControlMountPath, RestoreCompleteFile)},
		}},
		TimeoutSeconds:   1,
		PeriodSeconds:    1,
		FailureThreshold: restoreStartupFailureThreshold,
		SuccessThreshold: 1,
	}
}

func validateRestoreStartupProbe(container *corev1.Container) error {
	probe := container.StartupProbe
	if probe == nil {
		return fmt.Errorf("container %q is missing the restore startup gate", container.Name)
	}
	if probe.Exec == nil || !isRestoreCompletionProbeCommand(probe.Exec.Command) ||
		probe.HTTPGet != nil || probe.TCPSocket != nil || probe.GRPC != nil {
		return fmt.Errorf("container %q restore startup gate must check %s", container.Name, path.Join(SnapshotControlMountPath, RestoreCompleteFile))
	}
	return nil
}

func validateCanonicalRestoreStartupProbe(container *corev1.Container) error {
	if !reflect.DeepEqual(container.StartupProbe, canonicalRestoreStartupProbe()) {
		return fmt.Errorf("container %q has a conflicting restore startup gate", container.Name)
	}
	return nil
}

func isRestoreCompletionProbeCommand(command []string) bool {
	completionPath := path.Join(SnapshotControlMountPath, RestoreCompleteFile)
	switch {
	case len(command) == 2 && isSupportedProbeExecutable(command[0], "cat"):
		return command[1] == completionPath
	case len(command) == 3 && isSupportedProbeExecutable(command[0], "test"):
		return command[1] == "-f" && command[2] == completionPath
	default:
		return false
	}
}

func isSupportedProbeExecutable(actual string, executable string) bool {
	return actual == executable || actual == "/bin/"+executable || actual == "/usr/bin/"+executable
}

func ensurePodSeccompProfile(spec *corev1.PodSpec, expected string) error {
	if expected == "" {
		return nil
	}
	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if spec.SecurityContext.SeccompProfile == nil {
		spec.SecurityContext.SeccompProfile = localhostSeccompProfile(expected)
		return nil
	}
	if !matchesLocalhostSeccompProfile(spec.SecurityContext.SeccompProfile, expected) {
		return fmt.Errorf("pod has a conflicting seccomp profile; expected localhost profile %q", expected)
	}
	return nil
}

func validatePodSeccompProfile(spec *corev1.PodSpec, expected string) error {
	if expected == "" {
		return nil
	}
	if spec.SecurityContext == nil || !matchesLocalhostSeccompProfile(spec.SecurityContext.SeccompProfile, expected) {
		return fmt.Errorf("pod must use localhost seccomp profile %q", expected)
	}
	return nil
}

func validateContainerSeccompProfile(container *corev1.Container, expected string) error {
	if expected == "" || container.SecurityContext == nil || container.SecurityContext.SeccompProfile == nil {
		return nil
	}
	if !matchesLocalhostSeccompProfile(container.SecurityContext.SeccompProfile, expected) {
		return fmt.Errorf("container %q overrides required localhost seccomp profile %q", container.Name, expected)
	}
	return nil
}

func localhostSeccompProfile(profile string) *corev1.SeccompProfile {
	return &corev1.SeccompProfile{
		Type:             corev1.SeccompProfileTypeLocalhost,
		LocalhostProfile: &profile,
	}
}

func matchesLocalhostSeccompProfile(profile *corev1.SeccompProfile, expected string) bool {
	return profile != nil && profile.Type == corev1.SeccompProfileTypeLocalhost &&
		profile.LocalhostProfile != nil && *profile.LocalhostProfile == expected
}
