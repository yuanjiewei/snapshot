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
idempotent. `ValidateRestorePod` checks the same contract without mutation or
Kubernetes API reads. Conflicting annotations, volumes, mounts, environment,
probes, and security settings are rejected instead of overwritten.

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
gate. The builder keeps workload liveness and readiness probes unchanged, but
rejects a preexisting startup probe that does not already match the restore
gate because Kubernetes cannot compose two startup probes. The extended
failure threshold prevents kubelet liveness checks from killing the
placeholder while restore is in progress.

`RestorePodOptions.SeccompProfile` controls Snapshot's pod-level localhost
profile. An empty value leaves seccomp unmanaged. A destination container must
not override a requested profile with a conflicting container-level profile.

Snapshot does not modify container commands and does not inject
`DYN_SNAPSHOT_RESTORE_STANDBY` or any other workload-specific standby setting.
The producer must ensure each destination process remains alive and inert until
the agent replaces it with the restored process.
