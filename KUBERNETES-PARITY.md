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
because of. Measured on `icarus-2.local` on 2026-11-25:

| Pod | Result |
| --- | --- |
| `hostPath` `DirectoryOrCreate`, mount `readOnly` omitted | `Running` — "Macker is running the trusted native workload" |
| same, mount `readOnly: true` | `Pending` — `volume mount "/var/run" requests readOnly, but this Macker binary does not advertise "HOST:CONTAINER[:ro]" ...` |

The reason is a Macker primitive, not a maclet choice. Macker implements `-v` by
creating a symlink inside the container rootfs that points at the live host path
(`installVolume` → `os.Symlink(mount.HostPath, target)`), and its volume parser
accepts exactly two fields, `HOST:CONTAINER` (`parseVolume`). The workload is
then launched as an ordinary host process. A symlink carries no write
restriction, so there is nothing for maclet to set that the kernel would
enforce: the container process and the host process are the same process.

maclet therefore negotiates it:

- maclet asks the installed Macker binary whether it advertises the marker
  `HOST:CONTAINER[:ro]` in `macker run --help`. The answer is cached; a negative
  answer is re-probed after 5 minutes so upgrading Macker heals a node that is
  already joined, without restarting maclet.
- **Advertised:** maclet passes `readOnly` through as `-v HOST:CONTAINER:ro` and
  the runtime enforces it for the workload process.
- **Not advertised:** maclet refuses the Pod with the message above. It never
  sends `:ro` to a binary that would either reject the spec or, worse, accept and
  ignore it.

Read-only state that maclet *can* enforce today is enforced outside Macker:

- `PersistentVolumeClaim` volumes are mounted on the host through `mount_nfs`,
  so a `readOnly` PVC really is read-only at the host mount, whatever Macker does.
- `configMap` volumes are materialized into a per-Pod directory owned by maclet.
  Writes by the workload stay in that copy and never reach the Kubernetes
  ConfigMap. Until Macker advertises `:ro`, such a mount is passed writable and
  the copy is what protects the API object; once `:ro` is advertised, maclet
  requests it too.

If your policy is "mount sensitive trees read-only", on a maclet node that
policy currently cannot hold: it fails closed instead of pretending, and the
fix lives in Macker, not in a manifest.

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
| `hostPath` (`Directory`, `DirectoryOrCreate`, `File`, `FileOrCreate`) | Supported, writable live symlink; `readOnly` per the negotiation above |
| `hostPath` with no `type` | Supported when the path already exists |
| `hostPath` `Socket`, `CharDevice`, `BlockDevice` | Validated as the right object kind, then passed through |
| `configMap` | Supported (`items`, `defaultMode`, `binaryData`, `optional`), refreshed in place without a restart |
| `persistentVolumeClaim` | Supported through the `nfs.csi.k8s.io` gate, experimental; host-side `mount_nfs`, so `readOnly` is real |
| `emptyDir`, `secret`, `downwardAPI`, `projected`, `csi`, ... | Refused when mounted: "volume ... is not a supported hostPath volume" |
| `subPath` | Supported, contained; a `subPath` that escapes the `hostPath` (including through a symlink) is refused |
| Single-file content mounts | Use a `Directory` `hostPath` plus `subPath` and mount the full destination path, or a `File` `hostPath` without `subPath` |

## Verifying it from the cluster

The probe that produced the table above (namespace and paths are disposable):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: ro-on
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

Duplicate it with `readOnly: true` removed. Expect: the writable Pod reaches
`Running`; the `readOnly` Pod stays `Pending` with the message quoted above, or,
on a node whose Macker advertises `HOST:CONTAINER[:ro]`, reaches `Running` and
then *fails to start nginx*, because nginx writes its PID file into `/var/run`
and the mount is genuinely read-only. That last inversion is the acceptance
check for read-only enforcement: the same Pod must not be able to run both a
write-requiring and a write-denying workload.

`test-native-workload-lifecycle.sh` covers the writable `hostPath` lifecycle
against a real cluster, including that an invalid volume rollout leaves the Pod
`Pending` rather than terminal.
