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
   `kubernetes.io/os=darwin`, `kubernetes.io/arch=<host arch>`,
   `kubernetes.io/hostname=<Node name>`, and the taint
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
enforce it **and** the operating system is capable of it, and refused when either
is missing — never quietly dropped.

Macker's volume primitive is a symlink from the container rootfs to the live host
path (`installVolume` → `os.Symlink(mount.HostPath, target)`), and a symlink
carries no write restriction. Read-only is therefore enforced below the symlink,
by mapping the directory with nullfs (`mount -t null -o ro`, or the
`mount_nullfs` helper on platforms that ship it), so create, modify, and unlink
inside the mount fail with `EROFS` for the workload process. maclet negotiates
that capability in two stages:

- maclet asks the installed Macker what it can enforce, strongest evidence first.
  `macker volumes --probe --format json` (Macker ≥ `f61c2bb`, with `probe_status` from
  `52338ec`) attempts a real temporary read-only mount and a write inside it, so its
  `probe_status` is the only answer that cannot be produced by a mount helper merely
  being present; maclet branches on that field rather than reading the prose. `enforced`
  is a yes. `mount_refused`, `write_accepted` (mounted but still writable),
  `helper_unavailable`, `not_attempted_os`, and `unmount_failed` are no, and Macker's
  own `reason` is carried verbatim into the Pod's refusal. A probe that was never
  allowed to mount (`mount_not_attempted_privilege`) says nothing about the kernel, so
  maclet falls back to the unproven verdict from `macker volumes --format json` (Macker
  ≥ `4527b97`), which is the runtime's own answer about the mount path it will take,
  including a vendored helper named by `MACKER_MOUNT_NULLFS`. Macker's `guidance` field
  is a node-operator checklist and is logged on the node, not put in a Pod's status.
  Where neither subcommand exists (the answer comes back as usage text), maclet falls
  back to two checks of its own: whether `macker run --help` advertises the marker
  `HOST:CONTAINER[:ro]`, which is a contract/version check only, and whether this OS
  can make a read-only bind mount at all. The answer is cached; a negative answer is
  re-probed after 5 minutes, so upgrading Macker or the OS heals an already-joined
  node without restarting maclet.
- **Advertised, and the OS has nullfs:** `readOnly` is passed through as
  `-v HOST:CONTAINER:ro`.
- **Either answer no:** the Pod is refused at admission, and maclet never sends
  `:ro` to a runtime that would ignore it. An older Macker fails mid-start on a
  three-field spec, which is worse than a `Pending` Pod.

The OS check exists because **the marker describes Macker's argument grammar, not
the kernel underneath it**. Measured on the icarus node (macOS 26.6.2, Darwin
25.6.0, arm64), the userland a read-only bind mount needs is absent:

```console
absent  /sbin/mount_nullfs      absent  /Library/Filesystems/null.fs
absent  /sbin/mount_null        absent  /System/Library/Filesystems/null.fs
absent  /usr/sbin/mount_nullfs  absent  sysctl vfs.generic.nullfs
$ ls /sbin | grep '^mount'   # mount, mount_apfs, mount_nfs, mount_tmpfs, ... no null
$ sudo -n /sbin/mount -t null -o ro /tmp/msrc /tmp/mdst
mount: exec /Library/Filesystems/null.fs/Contents/Resources/mount_null for /private/tmp/mdst: No such file or directory
mount: /private/tmp/mdst failed with 72
```

So on that node **no Macker build can enforce a read-only `hostPath` mount**. The userland
is absent (no `mount_nullfs` anywhere, no `null.fs` bundle for `mount(8)` to exec, which is
what error 72 reports), and the kernel path is closed behind a door root cannot open. The
measurement that settles it is macker's, run as root on this same node:

```console
mount(2) syscall 167, four arguments (type, path, flags, data)
  type "nullfs"     -> EPERM (1)      reproducible; EPERM even with data nil
  type "null"       -> 19 = ENODEV    identical to a made-up name "bogusxyz"
  type "devfs"      -> mounts, releases          (calibration control)
  type apfs|hfs|autofs|lifs -> EINVAL           (bogus-spec control)
  type "nfs"        -> 75
```

Two corrections to what this document said before, both mine, and they point the same way.
The registered name on this OS is `nullfs`, not Linux's `null`, so my `ENODEV` for `null`
never meant "no such filesystem" -- it meant "no filesystem by that name". And `errno 19` on
Darwin **is** `ENODEV`; `ENOTSUP` is 45 (Go's generated Darwin table: `ENODEV = 0x13`,
`ENOTSUP = 0x2d`), which is worth writing down because the pair is easy to invert from Linux,
and because `EPERM` on the correct name is the result that actually decides this. My earlier
`sysctl vfs.generic.nullfs` evidence proves nothing either: `vfs.generic` is a tunable
namespace, not the mount registry -- it lists 119 names and omits `devfs`, which demonstrably
mounts -- so its absence cannot adjudicate whether a filesystem is registered.

What the `EPERM` does establish, and it matches Apple's source for `xnu-12377.121.6` (the
generation matching Darwin 25): `bsd/miscfs/nullfs/` is built into this kernel, and
`nullfs_mount()` opens with
`if (!IOCurrentTaskHasEntitlement("com.apple.private.nullfs_allow")) return EPERM;` **with no
superuser fallback**, forcing `MNT_RDONLY` on every mount. The filesystem is present and
reachable only by a process holding a private entitlement no third-party binary can acquire:
no helper path, no `MACKER_MOUNT_NULLFS` override, and no amount of `sudo` produces the
read-only view a `readOnly` volume asks for. maclet turns that into an admission refusal
instead of a doomed launch. macker's `--probe` is the authoritative statement of it on this
node (`probe_status: not_attempted_os`, a `reason` for the Pod, remedies in `guidance`), and
maclet branches on that status rather than on any of this prose.

The failure is not privilege either: the same node grants `(ALL) NOPASSWD: ALL` to the
user maclet runs as (uid 501), `sudo -n -v` succeeds, and the `mount` above ran as root and
returned mount's own error 72, not a `sudo` complaint.

maclet's own offline OS probe stays deliberately weaker than the mount-registry question it
cannot ask without root: it checks helper and bundle presence and treats a
`vfs.generic.nullfs` sysctl as a weak positive signal, for the reason macker warns against
reading its absence -- it can grant nothing on its own, and the runtime answer outranks it at
every rung of the ladder.

What `test-native-workload-lifecycle.sh` asserts about read-only on this node is the
**refusal contract**, not `EROFS`: `EROFS` inside a container is blocked by the OS here, so
a stage that demanded it would be asserting a capability no darwin node in the fleet has.
The stage requires the runtime's honest answer first -- it reads `.status.message` on the
refused Pod and fails if that text describes a mount that *accepted writes*, because "the
runtime refused honestly" and "the runtime failed to enforce" are the same visible outcome
at admission and only the reason distinguishes them -- and it fails if no Pod carrying a
`readOnly` mount reached the node at all, so a manifest that silently did not apply cannot
pass. On a node whose macOS does provide a usable read-only bind mount, the same stage
exercises the enforcement path unchanged: the Pod runs, its writes fail, and it never
becomes Ready.

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
  ConfigMap; maclet also asks for `:ro` on that directory when the runtime
  advertises it.

Three properties of the enforcement are worth stating plainly, because they are
not what a Linux reader assumes:

- Read-only mounts need root or passwordless `sudo` **at mount time**. That is not
  the constraint on this node class: the node grants `(ALL) NOPASSWD: ALL` to the
  maclet user, and maclet execs Macker directly as its own user (only the commands
  *maclet* runs itself — `ifconfig`, `route`, `arp`, `mount_nfs` — go through
  `sudo -n`; port forwarding is installed by Macker, not maclet. An earlier
  version of this document blamed the service `PATH` for
  lacking `/sbin`; that was wrong, Macker resolves the helper by absolute path
  first. The real gap is the OS missing nullfs, measured above.
- The nullfs mount is per mount point inside that container's rootfs and
  **node-wide for the mount's lifetime**. It is not per-process.
- It is **not a sandbox, and not containment**. The workload is an ordinary
  Darwin process, launched without `chroot`, so it can open the same host path
  directly with its own host permissions and never touch the read-only mount at
  all. `readOnly` on a maclet node means *writes through that mount fail with
  `EROFS`* — kernel-enforced, and enough to stop a workload writing where it
  should not. It does **not** mean the workload cannot read that tree, cannot
  reach it another way, or is confined. A policy of "mount sensitive trees
  read-only" is a guard against incidental writes here, not a security boundary;
  host access is the boundary, and a darwin-native workload has host access.
  Rule 4 still holds.

## Pod and scheduling semantics

| Field | On a maclet node |
| --- | --- |
| `nodeSelector: k8s-darwin.dev/native: "true"` + toleration | Required to schedule here at all |
| `nodeSelector: kubernetes.io/hostname: <Node name>` | Honoured: maclet registers the label every kubelet sets, so pinning a workload to one Mac works. It was absent on maclet nodes until this build, and a Pod asking for it failed with only the scheduler's "didn't match Pod's node affinity/selector" |
| Pod label `k8s-darwin.dev/native=true` | **Required to be run.** maclet only supervises Pods carrying this label. An unlabelled Pod bound to the node is reported `Pending` with reason `MacletMissingNativeLabel` and a message naming the label, instead of sitting unexplained forever |
| `spec.containers` | Exactly one. Sidecars are refused (`MacletUnsupportedPod`) |
| `spec.initContainers` | Not executed, and not refused — see rule 4. Do not rely on an init container for ordering or setup here |
| `spec.hostNetwork` | Supported with `dnsPolicy: ClusterFirstWithHostNet`; other host-network DNS policies stay `Pending`. The container binds its declared port on the host directly, exactly as on Linux — no Macker port-forward is installed. Verified on `icarus-2.local` 2026-11-25: a hostNetwork nginx declaring `containerPort: 8084` listened on `*:8084` and answered over the node address |
| `spec.os`, `nodeOS` affinity | The node reports `darwin`; the cluster scheduler decides, maclet does not validate image platforms |
| Image platform | Darwin-native Mach-O images only. A `linux/amd64` manifest (e.g. `busybox:1.36`) fails in Macker with an OS mismatch and the Pod stays `Pending`. Build a Darwin image (see the `macgrubernetes`/Macker `Mackerfile` workflow) or schedule on a Linux node |
| `ports[].containerPort` | **Required, not informational** on this node class — see "Ports and Macker config tokens". A PodIP workload becomes `-p CONTAINER:auto/tcp` (PodIP + PF remapping); a hostNetwork workload binds the declared port directly |
| `ports[].hostPort` | Refused, because maclet does not implement host-port mapping — **except** the `hostPort == containerPort` value the API server writes by itself for a hostNetwork Pod, which is accepted as the declared port |
| `livenessProbe`, `readinessProbe`, `startupProbe` | Not enforced (rule 4). `Ready` reflects "Macker reports the process running", nothing more |
| `resources` | Not enforced. The node advertises the host machine |
| `imagePullPolicy` | Not honoured; a missing image is pulled lazily, a present one is never re-pulled |
| `serviceAccountName` + projected token volume | The token volume mount at `/var/run/secrets/kubernetes.io/serviceaccount` is skipped, so in-cluster credentials do not exist on this node class |
| `subPathExpr` | Refused (not expanded) |
| `env`, `envFrom`, `valueFrom.secretKeyRef`, `valueFrom.configMapKeyRef`, `fieldRef` | Supported; Secret/ConfigMap reads need the explicitly authorized `--peer-kubeconfig` identity |
| `workingDir`, `command`, `args` | Supported, passed to Macker |
| `restartPolicy` | `Always`, `OnFailure`, `Never` are all handled; a succeeded/failed Pod is not resurrected against a non-`Always` policy |
| Pod deletion | Acknowledged by maclet for **every** Pod bound to this node, supervised or not — see "Who finishes a Pod deletion on a darwin node" |

### Who finishes a Pod deletion on a darwin node

On Linux, a deleted Pod leaves the API server's view because kubelet kills the
containers and then deletes the Pod object itself once it has reported a terminal
phase. Nothing else does that here, so maclet owns it, and it owns it for Pods it
*refused* too: the sweeper lists every Pod with `spec.nodeName=<this node>` (no label
selector, because a node credential is authorized for exactly that read) and
force-deletes any of them whose `deletionTimestamp` is past the stale window. Both
halves matter. maclet's own teardown path writes a terminal `Failed` phase with reason
`MacletWorkloadDeleted` before the API server completes a graceful deletion of an
adopted Pod. And a Pod maclet never adopted — unlabelled, or refused as unsupported —
has no agent at all otherwise, so it sits `Terminating` indefinitely, which is worse
than cosmetic: a Deployment or StatefulSet rolling a Pod onto a darwin node by mistake
waits on a replacement slot held by the object that is already gone. `staleAfter` is
45s cluster-wide and 5s for native Pods, so the acknowledgement is bounded and the
window is short enough that a genuinely slow teardown is not cut off.

Measured on `icarus-2.local` 2026-11-25: an unlabelled Pod bound to the node kept
`deletionTimestamp: 2026-11-25T09:38:54Z` and `phase: Failed` long after deletion and
needed `kubectl delete pod --force --grace-period=0`; the fix is the label-free
sweeper described above. The force-delete path itself is verified by tests, and a
refused Pod still gets its `MacletMissingNativeLabel` Event while it is alive, so the
reason a workload was never run here is visible before it is removed.

The sweeper is now the backstop rather than the only path. maclet deletes the object
itself in the pass that stops the workload -- report terminal phase, then delete with
`gracePeriodSeconds=0`, kubelet's order -- so a deletion maclet can see is bounded by one
reconcile interval instead of by the 45s window. Measured on `icarus-2.local` 2026-11-25
during a Deployment teardown: three ReplicaSet generations coexisted, two Pods Terminating
and a third created one second before the snapshot, because a ReplicaSet that is itself
terminating keeps reconciling toward its desired count while its Pods wait to be swept.
All three were acknowledged and removed inside one pass (20:08:52.04 to 20:08:52.46) once
the API cascade had given them deletion timestamps; the minutes of apparent lag were the
cascade's, not maclet's. A Pod whose workload cannot be stopped has its failure reported
and the acknowledgement skipped, so an object never vanishes without an account of it, and
the sweeper still catches it.

## How a refusal surfaces

A Pod a maclet node cannot run stays `Pending`. While a Pod is `Pending`,
`.status.reason` carries maclet's own named reason, which is what a client should
branch on (`kubectl get pod -o jsonpath='{.status.reason}'`). For `Running`,
`Succeeded`, and `Failed` the field is left empty, as Kubernetes expects, and the
canonical phase carries the state. The detail is on the first container's waiting
state, which stays inside the kubelet vocabulary and distinguishes the two kinds
of "cannot":

| `.status.reason` while Pending | Waiting reason | Meaning |
| --- | --- | --- |
| `MacletUnsupportedPod` | `CreateContainerConfigError` | The **manifest** asks for something this node class refuses — a policy decision, retrying will not help |
| `MacletMissingNativeLabel` | `CreateContainerConfigError` | The Pod is bound to the node but maclet will never supervise it: it lacks `k8s-darwin.dev/native=true` |
| `MacletMackerLaunchFailed` | `RunContainerError` | The node could not carry the Pod out — Macker failed to launch, or the host path was unreachable |

maclet files the same account as an `Event`, so `kubectl describe pod` shows the
node's own line rather than only the scheduler's. Each Event carries maclet's reason
in `.reason`, the detail plus a pointer to this section in `.message`, and
`source.component=maclet` with `source.host` set to the node. They are deduplicated
against the state they report: one Event per distinct `phase`/`reason`/`message`, with
a `count` that rises if the state persists past 30 minutes, because reconciliation
rewrites the same status every few seconds and an unthrottled writer would bury the
line that matters. A refusal therefore appears once, with a stable name like
`maclet.pending.unsupported.<pod>.<hash>` -- and the hash always survives, because when
the whole name will not fit in 63 characters it is the readable part that is cut. Cutting
the hash instead collapsed two startup reports of one long-named workload onto one object,
and one line vanished. The count is maclet's own tally, merged in
with a patch: NodeRestriction lets a node create and patch Events but not read them
back (a `GET` returns 403 for `system:node`), so no author on a node can increment a
number it cannot see.

One gap remains, stated rather than promised: the Ready condition reason is the generic
`ContainersNotReady`. The named reasons above are greppable in `.status.reason`, in the
waiting reason, in the Events, and in the maclet log.

Before 2026-11-25 every refusal reported `RunContainerError`, which read as "the
runtime crashed" and invited a retry loop rather than a manifest change, and an
unlabelled Pod inherited `ContainerCreating` — which claimed maclet was still
working on a Pod it had never intended to run.

Measured on `icarus-2.local` on 2026-11-25, from the node's own status message:

```console
macker run --detach ... -v /tmp/maclet-ro-check-251125:/var/run:ro -p 8080:auto/tcp
  docker.io/initialed85/nginx:latest: exit status 1
error: mount_nullfs was not found; it ships with macOS
```

That is the stance working as intended on a node that cannot deliver the field:
maclet forwarded `:ro` because the installed Macker advertises the marker, Macker
could not perform the mount, and the Pod stayed `Pending` — it was never run with
the sensitive tree writable.

**Conclusion as of 2026-11-25, replacing what this section previously claimed:**
read-only `hostPath` enforcement is **not achievable on macOS 26 / Darwin 25.6
arm64**. The OS provides no nullfs — not as a helper, not as a `mount(8)`
filesystem, not as a kernel feature, and (measured with `mount(2)` directly) not as a
registered VFS in the release kernel. It is a platform limit, not a build gap, not a
PATH gap, not a privilege gap, and not something a vendored helper or an entitlement
overrides. Rolling a newer Macker does not change it. maclet therefore asks the
runtime what it can enforce, falling back to marker-plus-platform where the runtime
cannot answer, and a `readOnly: true` `hostPath` mount on a node without nullfs is
refused at admission:

```console
volume mount "/var/data" requests readOnly, but this node cannot enforce a
read-only hostPath mount: Macker must accept "HOST:CONTAINER[:ro]" and this
kernel must support read-only bind mounts, and maclet verifies both. ...
```

which clients see as `Pending` / `CreateContainerConfigError` with
`.status.reason: MacletUnsupportedPod` — the manifest-versus-node signal — rather
than a `RunContainerError` that reads like a runtime crash and invites a retry.

What is verified on real hardware: maclet's forwarding (`:ro` reaches Macker as a
third field while writable mounts keep the two-field form), the refusal path, and
that no Pod is ever started with a mount it was promised read-only. What is
**not** verified anywhere, and must not be claimed: `EROFS` inside a read-only
`hostPath` mount on a Darwin node. If nullfs ever appears in a macOS release,
maclet's probe finds it and the guarantee becomes real without a maclet change;
until then the enforced read-only options on this node class are a `readOnly`
`PersistentVolumeClaim` (host-side `mount_nfs -o ro`) or a Linux node.

## Volume sources

| Source | On a maclet node |
| --- | --- |
| `hostPath` (`Directory`, `DirectoryOrCreate`, `File`, `FileOrCreate`) | Supported, writable live symlink; `readOnly` honoured on directories **only where the OS provides nullfs**, refused otherwise (macOS 26 does not) — and refused on single files everywhere |
| `hostPath` with no `type` | Supported when the path already exists |
| `hostPath` `Socket`, `CharDevice`, `BlockDevice` | Validated as the right object kind, then passed through |
| `configMap` | Supported (`items`, `defaultMode`, `binaryData`, `optional`), refreshed in place without a restart |
| `persistentVolumeClaim` | Supported through the `nfs.csi.k8s.io` gate, experimental; host-side `mount_nfs`, so `readOnly` is real |
| `emptyDir`, `secret`, `downwardAPI`, `projected`, `csi`, ... | Refused when mounted: "volume ... is not a supported hostPath volume" |
| `subPath` | Supported, contained; a `subPath` that escapes the `hostPath` (including through a symlink) is refused |
| Single-file content mounts | Use a `Directory` `hostPath` plus `subPath` and mount the full destination path, or a `File` `hostPath` without `subPath` |

## Ports and Macker config tokens

In ordinary Kubernetes `spec.containers[].ports` is informational: a container
that listens on 8080 works whether or not the manifest declares it, and the only
field with functional effect is `hostPort`. On a maclet node that is not true.

Macker fills `____MACKER_PORT_N____` tokens in image config files from the
published ports **in `-p` order** — the first `-p` supplies `MACKER_PORT_1`, and
so on — and it refuses to start a workload whose rootfs contains a token with no
published port behind it:

```console
error: configure container rootfs: substitute .../rootfs/etc/nginx/nginx.conf:
  token ____MACKER_PORT_1____ has no runtime value
```

maclet publishes one `-p` per declared `ports[]` entry and cannot invent more: it
does not read the image rootfs, and guessing which port a token wants would
break rule 2 by running a workload on a mapping nobody asked for. So on this node
class **an image with config tokens needs its ports declared**, and a Pod that
omits `ports:` is refused rather than started half-configured.

Measured 2026-11-25 on `icarus-2.local`, three Pods, same image
(`initialed85/nginx:latest`), same builds, only the manifest varying:

| Pod | Result |
| --- | --- |
| `ports: [{containerPort: 8080}]`, writable `hostPath` mount | `Running` — "Macker is running the trusted native workload" |
| no `ports:`, same writable mount | refused, token error above; the captured argv contains no `-p` at all |
| `ports: [{containerPort: 8080}]`, `readOnly: true` mount | `:ro` forwarded correctly; failed later at the mount, see below |

### hostNetwork Pods bind the declared port, and their defaulted hostPort is not a mapping request

For a PodIP workload, `-p CONTAINER:auto/tcp` is right: Macker allocates a private
process port, injects it as `MACKER_PORT_N`, and PF redirects PodIP:CONTAINER to
it, so overlapping rollout generations can coexist. For a `hostNetwork: true`
workload that shape is wrong twice over — the process would listen on an
allocated 30000-32767 port instead of the port the Pod declared, and PF would sit
on the declared port pretending to be the app. maclet therefore publishes nothing
for a hostNetwork Pod and supplies `--env MACKER_PORT_N=<containerPort>` instead,
which is what the declared port means on Linux.

**Corrected 2026-11-25, and the earlier version of this paragraph was wrong.** An
earlier measurement here claimed the PodIP half of that chain had failed on
`icarus-2.local` and advised holding Service-backed darwin rollouts. It was a
measurement artifact, and the redirect has since been confirmed working on a
maclet-supervised Pod. Two errors made it look broken:

1. `pfctl -s Anchors` without `-vv` does not list the `com.apple/*` child anchors
   Macker loads on macOS 26 — it showed no `macker-*` anchor while eleven were loaded
   and carrying traffic, one of which was verified to do so with `Evaluations: 142`
   from off-node attempts. Read the redirect state with `sudo macker ports
   [--format json]` (Macker 55fa6d6+), which names each container's own anchor and
   prints the kernel's `Evaluations`/`States` for its rule.
2. A PodIP is aliased locally on the node (`route -n get <PodIP>` → interface `lo0`,
   flags `LOCAL`), so traffic the node originates to its own PodIP is never evaluated
   by PF at all. The same rule read `Evaluations: 0` under node-local attempts. A
   node-local `curl http://<PodIP>:<port>/` proves nothing in either direction.

Measured properly, on a Pod maclet supervises: `maclet-pf-check-long-name-abc-de`
with `ports: [{containerPort: 8080}]`, PodIP `10.42.6.3`, requested from a workload Pod
on a different host (source address `10.42.1.29`) —

```console
HTTP/1.1 404 Not Found
Server: nginx/1.31.4
```

which is nginx's own answer on the container's allocated process port, reached through
the redirect. **Service traffic to a PodIP darwin workload is dependable on this node
and no rollout hold applies.** To check a node yourself, curl the PodIP from another
host; the node-local form of the check is `127.0.0.1:<nodePort>`, which exercises the
bind rather than the forwarding layer.

That path also exposed a self-inflicted impossibility, measured on `icarus-2.local`
the same day: the Kubernetes API server defaults `hostPort` to `containerPort`
for a hostNetwork Pod, and maclet refused any non-zero `hostPort`, so

```console
container port 8080 requests hostPort 8080; maclet does not support host-port mapping yet
```

rejected *every* hostNetwork Pod that declared ports — a manifest ordinary
Kubernetes runs happily, killed by a field the user never wrote. maclet now
accepts the hostNetwork default as the declared port and still refuses a genuine
mapping request (`hostPort != containerPort`, or any `hostPort` on a PodIP Pod).

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
   that can enforce read-only mounts the workload must **not** come up — nginx
   dies on `EROFS`. On macOS 26, where the OS has no nullfs, this Pod is refused at
   admission with `MacletUnsupportedPod` / `CreateContainerConfigError`, which is
   the same honest answer one stage earlier. In neither case may it reach `Running`
   with a writable mount; if it does, read-only is theatre.
2. **`ro-read`** (same volume, mounted at the document root
   `/usr/share/nginx/html` with `readOnly: true`): must reach `Running` and serve
   the host file, proving `readOnly` is not simply "unusable". On a node without
   nullfs it is refused too, so this one is a probe for enforceable nodes only.
3. **`ro-off`** (identical to `ro-write` with `readOnly` removed): must reach
   `Running`. The pair with `ro-write` is the inversion that makes the guarantee
   meaningful — the same process, denied by one field. Give the `hostPath` a
   disposable world-writable directory, which is what the lifecycle script uses
   (`mktemp -d` under `$TMPDIR`): mounting the host's real `/var/run` instead makes
   the workload's PID path root-owned 0755, and then the writable twin fails for a
   reason that has nothing to do with `readOnly`.

A `hostPath` of type `File` (or a `subPath` that resolves to a file) with
`readOnly: true` never reaches the runtime at all: maclet refuses it during
reconciliation with "not a directory: macOS mount_nullfs cannot mount a single
file read-only". Macker carries the same refusal, so if that message is what
surfaces instead of maclet's, maclet's admission check missed the shape and the
fixture should be reported as a maclet bug.

On the node itself, confirm the mount rather than inferring it from the argv:
`macker inspect --format json` reports `read_only` per volume and
`mounted_volumes` with the mount point under the container rootfs, so "the Pod
never became Ready" can be paired with "the mount really exists, read-only".

`test-native-workload-lifecycle.sh` also covers the writable `hostPath` lifecycle
against a real cluster, including that an invalid volume rollout leaves the Pod
`Pending` rather than terminal, so a ReplicaSet does not stream replacements
against a manifest the node cannot run. Every Pod the script renders declares
`ports:` deliberately: otherwise a failure would be the config-token refusal
above and could not be read as a verdict on `readOnly`.
