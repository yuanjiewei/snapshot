# Restore a replica

Restoring starts a new replica from a snapshot instead of cold-starting it. The
restored pods carry the `nvidia.com/restore-from` annotation, naming the
`PodSnapshot` to restore from; the node agent restores the checkpointed state into
the container during pod startup.

## Prerequisites

- A ready `PodSnapshot` exists (see [Checkpoint a replica](checkpoint.md)).
- The restored replica reuses the source's snapshot-ready pod spec, provided as a
  ready-to-apply `restore-deployment.yaml` in each build-and-deploy guide.

## Example

Each build-and-deploy guide ships a ready-to-apply `restore-deployment.yaml` next
to its `deployment.yaml`: the same manifest with the
`nvidia.com/snapshot-is-checkpoint-source` label removed and an
`nvidia.com/restore-from` annotation added, naming the `PodSnapshot` to restore
from. Download it for the framework in use ([vLLM](vllm/restore-deployment.yaml),
[SGLang](sglang/restore-deployment.yaml),
[TensorRT-LLM](tensorrt-llm/restore-deployment.yaml)).

In the manifest, set the container `image` to the one built for the source and set
the `restore-from` annotation to the `PodSnapshot` name, then apply it and watch the
rollout:

```bash
kubectl apply -f restore-deployment.yaml
kubectl rollout status deployment/vllm-restored -n my-inference --timeout=30m
```

The node agent adds a `snapshot/Restored` condition to the pod once the restore
completes — watch it, along with pod readiness, to confirm. If the restored
workload serves an API, sending a request is a good end-to-end check that it
resumed correctly.

The restored process resumes from the checkpointed state, skipping model loading
and warm-up. In practice, higher-level systems create these restored Deployments
rather than applying them by hand.
