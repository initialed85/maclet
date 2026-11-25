# maclet and ordinary Kubernetes semantics

This document answers one question for anyone writing a manifest, a scheduler, or
an admission policy: **when a workload is written against ordinary Kubernetes
semantics, does a maclet node honour them?**

maclet is the node agent for Darwin nodes (`macker://trusted-native`, e.g.
`icarus-2.local`). It is not a kubelet, and this node class is not a Linux node.
The rules below are what maclet guarantees, what it knowingly does differently,
and how each deviation behaves so it is found in a manifest review rather than at
runtime.

## The stance

1. **The node says what it is.** maclet registers with
   `kubernetes.io/os=darwin`, `kubernetes.io/arch=<host arch>`, and the taint
   `k8s-darwin.dev/native=true:NoSchedule`. A workload lands on a maclet node
   only if it tolerates that taint, so nothing arrives here by accident.
2. **A core field is honoured when the runtime can enforce it.** Where Macker
   gives maclet a primitive that matches Kubernetes behaviour, maclet uses it
   (`hostPath` types, `subPath`, `env`, `envFrom`, `valueFrom`, downward-API
   `fieldRef`, ports, `workingDir`, `command`/`args`, `hostNetwork`).
3. **A core field is refused when the runtime cannot enforce it.** maclet does
   not silently reinterpret a field, because a Pod that starts while ignoring
   `readOnly` is worse than a Pod that does not start: it looks compliant and
   is not. Refusals are reported on the Pod as `Pending` with an actionable
   message, deliberately not `Failed`, so a ReplicaSet does not spawn an
   unbounded replacement stream against a manifest the node simply cannot run.
4. **Absence is not a policy boundary.** Darwin workloads are native processes
   supervised through Macker with a symlink-assembled rootfs; there is no
   isolation, no per-Pod user, and no seccomp/AppArmor analogue. Everything on a
   maclet node is *trusted-native*: the taint is the boundary, not the runtime.
   Fields whose silent absence would weaken a policy or corrupt data fail closed
   (rule 3). Fields whose absence only changes availability, readiness, or
   scheduling quality — probes, `resources`, `imagePullPolicy`, `initContainers`
   — are ignored and listed below rather than refused, because refusing them
   would break workloads that are otherwise running correctly here.

## Volume semantics

`readOnly: true` on a `hostPath` volume mount is the field this document exists
because of. It is honoured on a maclet node whenever the installed Macker can
enforce it, and refused when it cannot — never quietly dropped.

Macker's volume primitive is a symlink from the container rootfs to the live host
path (`installVolume` → `os.Symlink(mount.HostPath, target)`), and a symlink
carries no write restriction. Read-only is therefore enforced below the symlink:
a recent Macker maps a `:ro` volume with `mount_nullfs -o ro`, so create,
modify, and unlink inside the mount fail with `EROFS` for the workload process.
maclet negotiates that capability:

- maclet asks the installed Macker whether `macker run --help` advertises the
  marker `HOST:CONTAINER[:ro]`. The answer is cached; a negative answer is
  re-probed after 5 minutes, so upgrading Macker heals an already-joined node
  without restarting maclet.
- **Advertised:** `readOnly` is passed through as `-v HOST:CONTAINER:ro`.
- **Not advertised:** the Pod is refused with a message naming the marker, and
  maclet never sends `:ro` to a binary that would ignore it. An older Macker
  fails mid-start on a three-field spec, which is worse than a `Pending` Pod.

The one shape read-only cannot cover is a **single file**. macOS
`mount_nullfs` mounts directory subtrees, not files, so maclet refuses a
`readOnly` mount whose resolved host path is not a directory — including the
`hostPath type: File` case and the `subPath` of a directory that lands on a file
— and says so at admission rather than letting the mount fail inside the
container. The portable manifest rewrite is to mount the containing directory
read-only and address the file inside it.

Read-only state that maclet enforces outside Macker:

- `PersistentVolumeClaim` volumes are mounted on the host through `mount_nfs`,
  so a `readOnly` PVC is read-only at the host mount regardless of Macker.
- `configMap` volumes are materialized into a per-Pod directory owned by maclet.
  Writes by the workload stay in that copy and never reach the Kubernetes
  ConfigMap; once `:ro` is available maclet asks for it on that directory too.

Two properties of the enforcement are worth stating plainly, because they are not
what a Linux reader assumes:

- Read-only volumes need root or passwordless `sudo` on the node, the same
  privilege maclet already needs for PF mappings and NFS mounts.
- The nullfs mount is **node-wide for the mount's lifetime**, not per-process
  sandboxing. It is the right primitive for a `readOnly` volume; it is not
  isolation, and rule 4 below still holds.

## Pod and scheduling semantics

| Field | On a maclet node |
| --- | --- |
| `nodeSelector: k8s-darwin.dev/native: "true"` + toleration | Required to schedule here at all |
| Pod label `k8s-darwin.dev/native=true` | **Required to be run.** maclet only supervises Pods carrying this label. An unlabelled Pod bound to the node is reported `Pending` with reason `MacletMissingNativeLabel` and a message naming the label, instead of sitting unexplained forever |
| `spec.containers` | Exactly one. Sidecars are refused (`MacletUnsupportedPod`) |
| `spec.initContainers` | Not executed, and not refused — see rule 4. Do not rely on an init container for ordering or setup here |
| `spec.hostNetwork` | Supported with `dnsPolicy: ClusterFirstWithHostNet`; other host-network DNS policies stay `Pending` |
| `spec.os`, `nodeOS` affinity | The node reports `darwin`; the cluster scheduler decides, maclet does not validate image platforms |
| Image platform | Darwin-native Mach-O images only. A `linux/amd64` manifest (e.g. `busybox:1.36`) fails in Macker with an OS mismatch and the Pod stays `Pending`. Build a Darwin image (see the `macgrubernetes`/Macker `Mackerfile` workflow) or schedule on a Linux node |
| `ports[].hostPort` | Refused; PodIP port remapping through PF is the model |
| `livenessProbe`, `readinessProbe`, `startupProbe` | Not enforced (rule 4). `Ready` reflects "Macker reports the process running", nothing more |
| `resources` | Not enforced. The node advertises the host machine |
| `imagePullPolicy` | Not honoured; a missing image is pulled lazily, a present one is never re-pulled |
| `serviceAccountName` + projected token volume | The token volume mount at `/var/run/secrets/kubernetes.io/serviceaccount` is skipped, so in-cluster credentials do not exist on this node class |
| `subPathExpr` | Refused (not expanded) |
| `env`, `envFrom`, `valueFrom.secretKeyRef`, `valueFrom.configMapKeyRef`, `fieldRef` | Supported; Secret/ConfigMap reads need the explicitly authorized `--peer-kubeconfig` identity |
| `workingDir`, `command`, `args` | Supported, passed to Macker |
| `restartPolicy` | `Always`, `OnFailure`, `Never` are all handled; a succeeded/failed Pod is not resurrected against a non-`Always` policy |

## Volume sources

| Source | On a maclet node |
| --- | --- |
| `hostPath` (`Directory`, `DirectoryOrCreate`, `File`, `FileOrCreate`) | Supported, writable live symlink; `readOnly` honoured on directories, refused on single files |
| `hostPath` with no `type` | Supported when the path already exists |
| `hostPath` `Socket`, `CharDevice`, `BlockDevice` | Validated as the right object kind, then passed through |
| `configMap` | Supported (`items`, `defaultMode`, `binaryData`, `optional`), refreshed in place without a restart |
| `persistentVolumeClaim` | Supported through the `nfs.csi.k8s.io` gate, experimental; host-side `mount_nfs`, so `readOnly` is real |
| `emptyDir`, `secret`, `downwardAPI`, `projected`, `csi`, ... | Refused when mounted: "volume ... is not a supported hostPath volume" |
| `subPath` | Supported, contained; a `subPath` that escapes the `hostPath` (including through a symlink) is refused |
| Single-file content mounts | Use a `Directory` `hostPath` plus `subPath` and mount the full destination path, or a `File` `hostPath` without `subPath` |

## Verifying it from the cluster

Three probes, all against the darwin nginx image (`test-native-workload-lifecycle.sh`
runs them as part of the lifecycle pass on a Mac node). Namespace and host paths
are disposable.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: ro-write
  labels:
    k8s-darwin.dev/native: "true"
spec:
  nodeSelector: {k8s-darwin.dev/native: "true"}
  tolerations:
    - {key: k8s-darwin.dev/native, operator: Equal, value: "true", effect: NoSchedule}
  volumes:
    - name: run
      hostPath: {path: /tmp/maclet-parity-probe, type: DirectoryOrCreate}
  containers:
    - name: nginx
      image: docker.io/initialed85/nginx:latest
      ports: [{containerPort: 8080}]
      volumeMounts:
        - {name: run, mountPath: /var/run, readOnly: true}
```

nginx writes its PID file into `/var/run`, so this Pod is a write test that needs
no shell in the image:

1. **`ro-write`** (`readOnly: true` at a path the workload must write): on a node
   whose Macker advertises `HOST:CONTAINER[:ro]` the workload must **not** come up
   — nginx dies on `EROFS`. If it reaches `Running`, read-only is theatre. On an
   older Macker it stays `Pending` with a message naming the marker.
2. **`ro-read`** (same volume, mounted at the document root
   `/usr/share/nginx/html` with `readOnly: true`): must reach `Running` and serve
   the host file, proving `readOnly` is not simply "unusable".
3. **`ro-off`** (identical to `ro-write` with `readOnly` removed): must reach
   `Running`. The pair with `ro-write` is the inversion that makes the guarantee
   meaningful — the same process, denied by one field.

A `hostPath` of type `File` (or a `subPath` that resolves to a file) with
`readOnly: true` never reaches the runtime at all: maclet refuses it during
reconciliation with "not a directory: macOS mount_nullfs cannot mount a single
file read-only".

`test-native-workload-lifecycle.sh` also covers the writable `hostPath` lifecycle
against a real cluster, including that an invalid volume rollout leaves the Pod
`Pending` rather than terminal, so a ReplicaSet does not stream replacements
against a manifest the node cannot run.
