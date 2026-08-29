<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Restore Pod contract

A Pod requests restore by naming a ready `PodSnapshot` in its namespace. The
Snapshot node agent restores only Pods that implement this contract.

Go integrations should use:

```go
restored, err := snapshotv1alpha1.BuildRestorePod(
    pod,
    snapshotName,
    mappings,
    snapshotv1alpha1.RestorePodOptions{
        SeccompProfile: snapshotv1alpha1.DefaultSeccompLocalhostProfile,
    },
)
```

`BuildRestorePod` is a pure, atomic transformation: it returns a deep copy or
an error and never mutates its input. Reapplying it with the same arguments is
idempotent. It emits one canonical representation and validates that exact
output. `ValidateRestorePod` checks the stable runtime contract without
mutation or Kubernetes API reads, accepting supported equivalent completion
gates and caller-selected probe timing. Conflicting annotations, volumes,
mounts, environment, and security settings are rejected instead of overwritten.

The producer derives each typed mapping source from the referenced
`PodSnapshot`. The builder validates one-source-to-many-destination consistency
but deliberately performs no API read to discover the captured source.

Non-Go producers can implement the declarative form directly. This fan-out
example restores the captured `main` process into two destination containers:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: restored-worker
  annotations:
    nvidia.com/restore-from: worker-snapshot
    nvidia.com/restore-container-map: main=engine-0,main=engine-1
spec:
  securityContext:
    seccompProfile:
      type: Localhost
      localhostProfile: profiles/block-iouring.json
  volumes:
    - name: snapshot-control
      emptyDir: {}
  containers:
    - name: engine-0
      image: worker:latest
      # The caller supplies an inert, long-running entrypoint.
      command: ["/bin/sh", "-c", "exec sleep infinity"]
      env:
        - name: SNAPSHOT_CONTROL_DIR
          value: /snapshot-control
      volumeMounts:
        - name: snapshot-control
          mountPath: /snapshot-control
          subPath: engine-0
      startupProbe:
        exec:
          command: ["cat", "/snapshot-control/restore-complete"]
        timeoutSeconds: 1
        periodSeconds: 1
        failureThreshold: 1800
        successThreshold: 1
    - name: engine-1
      image: worker:latest
      command: ["/bin/sh", "-c", "exec sleep infinity"]
      env:
        - name: SNAPSHOT_CONTROL_DIR
          value: /snapshot-control
      volumeMounts:
        - name: snapshot-control
          mountPath: /snapshot-control
          subPath: engine-1
      startupProbe:
        exec:
          command: ["cat", "/snapshot-control/restore-complete"]
        timeoutSeconds: 1
        periodSeconds: 1
        failureThreshold: 1800
        successThreshold: 1
```

The container mapping is optional for a single same-name restore. Every
destination mounts the one shared `snapshot-control` `emptyDir` at
`/snapshot-control` with `subPath` equal to its container name. The canonical
`SNAPSHOT_CONTROL_DIR` environment variable points to that mount. The builder
also injects the deprecated `DYN_SNAPSHOT_CONTROL_DIR` alias during the
migration window; hand-authored Pods may omit the alias.

The `restore-complete` sentinel probe shown above is the authoritative startup
gate. Kubernetes supports only one startup probe, so the builder replaces any
existing startup probe with the canonical restore gate while keeping workload
liveness and readiness probes unchanged. The runtime validator also accepts a
direct `test -f /snapshot-control/restore-complete` gate and does not pin probe
timing to a particular builder release. The canonical builder's extended
failure threshold allows 1,800 consecutive one-second startup-probe failures.
Kubernetes pauses liveness and readiness probes until the startup gate
succeeds; if restoration exceeds that failure budget, kubelet restarts the
placeholder according to the Pod's restart policy.

`RestorePodOptions.SeccompProfile` controls Snapshot's pod-level localhost
profile. An empty value leaves seccomp unmanaged. A destination container must
not override a requested profile with a conflicting container-level profile.

Snapshot does not modify container commands and does not inject
`SNAPSHOT_RESTORE_STANDBY`, its deprecated
`DYN_SNAPSHOT_RESTORE_STANDBY` alias, or any other workload-specific standby
setting. Both names are exported by the Go API so application owners can set
the convention they support. The producer must ensure each destination process
remains alive and inert until the agent replaces it with the restored process.
