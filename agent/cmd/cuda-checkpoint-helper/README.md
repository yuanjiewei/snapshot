# CUDA checkpoint helper

This directory contains Snapshot's node-local CUDA checkpoint helper. The
helper isolates CUDA driver calls and CustomStorage callbacks from the Go
agent. It is an implementation detail of the Snapshot agent, not a
workload-facing lifecycle API and not the PageBroker transaction protocol.

## Communication boundary

```mermaid
flowchart LR
    C["Snapshot controller"] -->|"checkpoint or restore work"| A["Snapshot node agent"]
    A -->|"resolve and validate target identity"| R["container runtime and /proc"]
    A <-->|"versioned, bounded Unix SOCK_SEQPACKET RPC"| H["privileged CUDA helper"]
    H <-->|"CUDA driver checkpoint/restore"| P["target process"]
    H <-->|"transfer adapter"| B["artifact data plane"]
```

The controller decides which workload operation is running. The node agent
owns target discovery, ordering with CRIU, durable manifest construction, and
the final checkpoint or restore result. The helper owns CUDA driver calls,
CustomStorage callback lifetime, per-operation transfer cancellation, and
reporting the observed CUDA target state.

The helper RPC is local to one Snapshot node agent. It does not define:

- the Kubernetes Snapshot API;
- workload quiesce or resume semantics;
- durable `checkpoint_id` or manifest publication;
- PageBroker transaction, commit, or abort semantics; or
- a network API that PageBroker must implement.

A PageBroker GPU engine may reuse the CUDA operation and transfer behavior
without adopting this socket protocol. Conversely, Snapshot's local path may
provide a transfer adapter without changing the workload lifecycle.

## Running

The Snapshot integration configures one privileged helper beside each node
agent. The helper needs host PID visibility and CUDA driver access so it can
validate and checkpoint CUDA-owning processes on that node. The operation
socket is private to the pod and is shared with the agent through an `emptyDir`
volume.

```text
cuda-checkpoint-helper --daemon \
  --socket /run/cuda-checkpoint-helper/helper.sock \
  --max-operation-seconds 3600
```

The chart checks readiness through the separate health socket derived from the
same path:

```text
cuda-checkpoint-helper --health \
  --socket /run/cuda-checkpoint-helper/helper.sock
```

Health succeeds only after the daemon has bound both sockets and advertised
the deferred-CUDA capability. CustomStorage availability is reported as a
separate capability so callers can fail before state-changing work when the
driver or transfer adapter is unavailable.

## Request and response envelope

Each request contains the protocol version, action, validated node-local PID,
PID identity, storage mode, device mapping, selected GPU UUIDs, and
operation-specific paths. The bounded request is carried in one
`SOCK_SEQPACKET` message so the daemon never accepts a partial request as a
complete operation.

Each response contains the protocol version, operation result, capability and
fatal-state flags, and a bounded diagnostic payload. Health responses advertise
capabilities before the agent starts state-changing CUDA work.

The agent must revalidate PID identity before the helper signals or mutates a
target. Raw host PIDs are node-local execution details and are never durable
checkpoint identity.

## Operation lifecycle

For checkpoint, the helper locks the target, starts the CUDA checkpoint
operation, transfers every CustomStorage extent through the selected adapter,
and completes the CUDA operation handle before returning success. For restore,
it restores the target from the recorded extent manifest and completes the
handle. After all restore targets succeed, the agent sends a separate unlock
request for each target.

One helper request operates on one CUDA-owning PID. The Snapshot agent may
issue several requests for one workload, but it retains ordering and an
individual result for every target.

The daemon retains primary contexts only for the request's selected GPU set.
After a successful operation, it associates those references with the exact
target PID, process start time, and cgroup. It releases them only after `/proc`
confirms that target exited or its PID was reused, or during daemon shutdown.
An inconclusive identity read retains the contexts and defers new work rather
than risking release underneath a live restored target. A release failure is
fatal because continuing would make GPU-resource ownership ambiguous.

## Failure rules

- Failure of any extent cancels sibling transfers for that operation.
- The helper applies one configured cooperative watchdog, capped at one hour,
  to extent transfers and reports an unhealthy in-flight operation after that
  threshold. CUDA driver calls are not forcibly interruptible. The client waits
  up to five minutes longer; an absent response is an unknown outcome and is
  not replayed.
- Once a state-changing request may have reached the helper, an unknown result
  is not replayed automatically.
- A CUDA operation handle must be completed or resolved before the helper
  reports a reusable target. An unresolved handle is fatal to that helper
  process.
- Storage cleanup, including a future PageBroker abort, does not prove that the
  CUDA target or workload is safe to resume.

The no-backend build used by the first stack slice validates the driver and RPC
state machines without choosing a production transfer implementation. The
Snapshot-local NIXL/POSIX adapter and its rollout are added separately.
