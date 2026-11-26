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
  being present; maclet branches on that field rather than reading the prose. Because the
  probe performs a real mount it needs root or passwordless sudo, and it inherits maclet's
  privilege rather than escalating -- on this node maclet is a root launchd daemon, so the
  probe is attempted. `enforced`
  is a yes. `mount_refused`, `write_accepted` (mounted but still writable),
  `helper_unavailable`, `not_attempted_os`, and `unmount_failed` are no, and Macker's
  own `reason` is carried verbatim into the Pod's refusal. A probe that was never
  allowed to mount (`mount_not_attempted_privilege`) says nothing about the kernel, so
  maclet falls back to the unproven verdict from `macker volumes --format json` (Macker
  ≥ `4527b97`), which is the runtime's own answer about the mount path it will take,
  including a vendored helper named by `MACKER_MOUNT_NULLFS`. Macker's `guidance` field
  is a node-operator checklist: maclet writes it to the node log once per distinct
  paragraph (a negative capability answer is re-probed every few minutes, and the
  checklist is long), and never puts it in a Pod's status.
- Two things maclet reports about the probe itself, both on the node log and neither in a
  Pod's status. `probe.unmount_error` is logged whatever the status says, because
  `write_accepted` is a verdict that both refuses and can leave the mount sitting on the
  node, and maclet is the long-lived process there. And each capability answer is logged
  with the evidence that produced it — a probe status, an unproven `volumes` verdict, the
  help marker, or maclet's own reading of the OS — because a Macker too old to probe and a
  Macker refused by the kernel look identical in a Pod's status, and only one of them is
  fixed by upgrading Macker. Macker reports its status both as a top-level `probe_status`
  and inside the `probe` record beside the mount facts; maclet reads either.
- Carrying the runtime's `reason` verbatim is a deliberate inheritance, and its cost is
  accuracy: whatever Macker says about the mechanism becomes maclet's stated cause in a
  Pod's status. That is not hypothetical. The build deployed on this node (`dee408c`) states
  that "this kernel does not register the null filesystem at all", measured false — the
  registered name is `nullfs` and `mount(2)` as root answers `EPERM` for it — and a
  read-only Pod filed on the node reproduces that sentence verbatim in its `Pending` status.
  Macker corrected it from `7468fd2` onward, which is why the roll target is the pinned
  descendant rather than the build already on the node. maclet does not rewrite a runtime's
  mechanism claim: the runtime owns the mount path, and a maclet-side correction would put
  two causes in one status message. What maclet does instead is keep its own measurements in
  this document, so a runtime claim can be checked against something other than prose.
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
measurement that settles it is macker's, reproduced independently by maclet on the same node
with the same call form (libc `mount()`, which is `mount(2)`, `MNT_RDONLY`, and mount data as a
`/dev/null` string or as a nil pointer; the third form the probe now adds has not been run on a
node yet):

```console
mount(2), four arguments (type, path, flags, data)
  type "nullfs"     -> EPERM (1)      both measured data forms, and with the data pointer nil too;
                                      a vfs entry point refusing, not a spec being misread
  type "null"       -> ENODEV (19)    identical to a made-up name "bogusfsxyz"
  type "bindfs"     -> ENODEV (19)    all three data shapes; the name is not registered here
  type "devfs"      -> mounts, unmounts        (the control that calibrates the probe)
  type apfs         -> EINVAL with a device spec, EFAULT (14) with "/dev/null"
  type "nfs"        -> EPROGMISMATCH (75)
```

`bindfs` is the other name worth asking about, and the probe includes it. Its answer is now a
**reading, not a prediction**, and it is the opposite of what the source says: on macOS 26.6.2 the
type name `bindfs` is **not registered at all** and draws `ENODEV` (19) in all three mount-data
shapes, identically to a made-up name, so there is no bindfs mount entry point on this OS for
anybody to be refused by. xnu `xnu-12377.121.6:bsd/miscfs/bindfs/bind_vfsops.c:140` opens
`bindfs_mount()` with an entitlement gate under `com.apple.private.bindfs-allow` that is
structurally identical to nullfs's, and that file is exactly why this row had to be measured:
reading it tells you bindfs is present-and-gated, which is false on the shipping kernel. So
bindfs is closed at a *stronger* level than nullfs -- nothing reaches the gate to be refused.

Two measurement traps live in that table, and they are how this question got argued in two
directions on one node. macOS `strerror(19)` reads *"Operation not supported by device"*, so
a probe that prints the string instead of the macro reports 19 as though it were `ENOTSUP`;
the macro for 19 in `<sys/errno.h>` and in xnu's tables is `ENODEV`, and `ENOTSUP` is 45 (Go's
generated Darwin table agrees: `ENODEV = 0x13`, `ENOTSUP = 0x2d`). A second trap is the
bogus-spec control: what `apfs` answers depends on the mount data you hand it -- `EINVAL` (22)
with a nullfs-shaped conf, `EFAULT` (14) with `/dev/null` -- so the calibration that matters is
the name that **succeeds**, `devfs` mounting read-only and releasing cleanly, not whatever errno
a filesystem you were never going to mount returns.

A third trap produced the wrong half of this conclusion, and it is the call form itself.
`mount(2)` on Darwin is `mount(type, path, flags, data)`. A probe that hands the kernel a source
path where the type belongs, a struct size where the flags belong, a `struct mount_args *` as
its only argument (the pre-BSD ABI, which is what `xnu`'s 167 does not expect), or glibc's
five-argument Linux `mount(src, dst, fstype, flags, data)` is answering a question about its own
arguments, not about the kernel -- in the five-argument case the filesystem name the kernel is
asked for is a directory path, so a case whose own output line reads "nullfs spelled out" never
asked for `nullfs` at all. Darwin's `unmount` is syscall 159, and 166 raises `SIGSYS` on arm64,
which is how one version of this probe died holding a mount it had just made. The reading that
settled the question used the four-argument form -- the same one that calibrates `devfs` and
`apfs` -- and it answers `EPERM` for `nullfs` while `null` and `bogusfsxyz` both answer 19.
Reading `null` alone therefore settles nothing: it is simply not the registered name, and it
behaves exactly like an invented one.

The instrument is `tools/nullfs-name-probe.c` in this repository, so this claim is checkable on
any darwin node rather than a transcript of one:
`clang -o nullfs-probe tools/nullfs-name-probe.c && sudo ./nullfs-probe /tmp/nullfs-probe null
nullfs bindfs bogusfsxyz apfs devfs`. It takes the probe root and the names to ask, prints the
errno **number and macro name** for each name under three mount-data forms -- a `/dev/null`
string, a nil pointer, and a spec built the way xnu reads one (`struct null_mount_conf` then the
source path) -- and unmounts everything it mounted before it exits, returning nonzero if any
mount survives it. Printing numbers rather than `strerror` text is the point: 19's comment in
Apple's header reads like Linux's `ENOTSUP`, and that confusion cost this document two wrong
conclusions. An earlier version of this probe used the deprecated `syscall()` shim, which raised
SIGSYS partway through on arm64, and it died holding a `devfs` mount it had just made.

Two corrections to what this document said before, both mine, and they point the same way.
The registered name on this OS is `nullfs`, not Linux's `null`, so my `ENODEV` for `null`
never meant "no such filesystem" -- it meant "no filesystem by that name", and `EPERM` on the
correct name is the result that actually decides this. My earlier `sysctl vfs.generic.nullfs`
evidence proves nothing either: `vfs.generic` is a tunable namespace, not the mount registry --
it lists 119 names and omits `devfs`, which demonstrably mounts -- so its absence cannot
adjudicate whether a filesystem is registered.

What the `EPERM` does establish, and it matches Apple's source at tag `xnu-12377.121.6`
(the generation matching Darwin 25), read directly from `apple-oss-distributions/xnu`:
`bsd/miscfs/nullfs/` is built into this kernel, and
`nullfs_mount()` opens with
`if (!IOCurrentTaskHasEntitlement("com.apple.private.nullfs_allow")) return EPERM;` **with no
superuser fallback**, forcing `MNT_RDONLY` on every mount. Three details from that file are
worth carrying, because each one is a way to misread a probe:

- the gate sits at line 142, **before** any `copyin` of mount data, which is why `EPERM` arrives
  with the data pointer nil too and cannot be blamed on a malformed spec. The corollary is that a
  well-formed spec buys nothing, and the probe's new `conf+path` pass exists to show that on a
  node rather than by reading this file;
- mount data is not a string there. `nullfs_mount` `copyin`s a `struct null_mount_conf`
  (`{ uint64_t flags; }`) and then `copyinstr`s the source path from
  `user_data + sizeof(conf)`, so the spec this filesystem wants is a conf followed by a path,
  not a `fstype=…,from=…` string. `MNT_RDONLY` is forced, not requested; and
- `bindfs` -- `bsd/miscfs/bindfs/`, present in the same tree and also a nullfs derivative --
  opens its mount with the identical gate under `com.apple.private.bindfs-allow` **in the source
  tree**, but the shipping kernel on macOS 26.6.2 does not expose the name at all (`ENODEV`, above).
  So there is no second filesystem to route around either: nullfs is present and gated where root
  cannot follow, and bindfs is not registered so there is nothing to be refused by. The asymmetry is
  the lesson -- a source reading named both as gated, and measurement separated them.

That errno naming needs pinning too, because an intermediate Macker build published the wrong
version of it and maclet carries a runtime `reason` verbatim. Apple's own
`bsd/sys/errno.h` at `xnu-12377.121.6` numbers `ENODEV` 19 -- commented *"Operation not
supported by device"*, which is why a probe printing `strerror` text reads like Linux's
`ENOTSUP` -- and `ENOTSUP` 45, with `EOPNOTSUPP` aliased to `ENOTSUP` under the full C level.
Linux, on the same question, is `ENODEV` 19, `ENOTSUP` = `EOPNOTSUPP` 95, `EOVERFLOW` 75, and
has no `EPROGMISMATCH` at all; Darwin's `EPROGMISMATCH` is 75, which is the number `nfs`
returned above. So **19 is `ENODEV` on both OSes, and only `ENOTSUP` moves**. Any statement that
"19 on Darwin is `ENOTSUP`", or that Linux "numbers 19 the other way round", is false; Macker
corrected its own copy of that claim in `ff7b159`, and a node still running a build from before
it will publish the wrong sentence inside its Pod `reason`.
The filesystem is present and
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
cannot ask without root: `hasNullfs()` accepts only a route a mount call or helper can take
-- a helper binary, or a filesystem bundle `mount(8)` can exec -- and the
`vfs.generic.nullfs` sysctl is read for *wording* only, never as a grant. Two measurements
say why that distinction is load-bearing here rather than theoretical: `vfs.generic` is a
tunable namespace that omits `devfs`, which demonstrably mounts and unmounts, so the oid is
not a mount registry in either direction; and this kernel does register `nullfs` while
`mount(2)` still answers `EPERM`, so a node that looked registered to a sysctl reader was a
node that could not enforce a read-only mount. The runtime answer outranks it at every rung
of the ladder.

What `test-native-workload-lifecycle.sh` asserts about read-only on this node is the
**refusal contract**, not `EROFS`: `EROFS` inside a container is blocked by the OS here, so
a stage that demanded it would be asserting a capability no darwin node in the fleet has.
The stage requires the runtime's honest answer first -- it reads `.status.message` on the
refused Pod and fails if that text describes a mount that *accepted writes*, because "the
runtime refused honestly" and "the runtime failed to enforce" are the same visible outcome
at admission and only the reason distinguishes them -- and it fails if no Pod carrying a
`readOnly` mount reached the node at all, so a manifest that silently did not apply cannot
pass. The check on the reason text is deliberately two-sided. A positive match alone -- "the
reason names some mechanism" -- is satisfiable by a mechanism that does not exist, and that is
not a hypothetical: macker build-33 (`dee408c`) publishes "this kernel does not register the
null filesystem at all" into every refused `:ro` Pod's status on a node where `nullfs` *is*
registered and answers `EPERM` at an entitlement gate. So the stage also fails on the claims
maclet has falsified, verified against the two builds' own text rather than against a guess:
`dee408c`'s reason trips the negative assertion and `866c98f`'s does not. That makes a red stage
on an unrolled node the expected result, and it is what turns "stage green" into evidence about
truth instead of only about shape. The same table also catches a claim maclet itself repeated: the
paragraph above used to state, from a source reading, that bindfs "is gated the same way", and
macker build-40 (`2250a51`) published that into its `guidance` -- which maclet copies to the node
log -- before re-measuring on the node at build-41 (`7837864`) and finding the name is not
registered at all. So `logFalsifiedReadOnlyReason` flags both the nullfs-registration falsehood and
the bindfs closure, and the fragment is pinned against the two builds' real text rather than a
paraphrase: `2250a51` trips it, `7837864` does not, even though the corrected paragraph still
discusses `bindfs-allow` and `EPERM` at length. maclet's own runtime path mirrors the concern without editing
any Pod: `logFalsifiedReadOnlyReason` says on the node log that a published claim is false and
what replaced it, and the refusal text itself stays verbatim, because silently rewriting a
runtime's explanation would make a Pod's status uncheckable against the binary that produced it.
The roll target this leaves is macker **build-41 (`7837864`)**, not build-35 (`7468fd2`): measured
against this very matcher and this very stage regex, `7468fd2` publishes "errno 19 on Darwin is
ENOTSUP" and so trips both, while `866c98f` (build-38) and `7837864` (build-41) are clean on both.
Build-38 to build-41 is a one-line change inside `guidance` and no change at all to the `reason`
maclet copies into a Pod's status, so the choice between them is only about what the node log tells
an operator.

**That roll has been done, and both halves of the expectation are now measurements rather than
predictions.** Applied to icarus-2 at 2026-11-26T04:27:51Z as maclet **build-73 (`7fd7e86e`)** plus
macker **build-41 (`78378640`)** by copy, re-sign, move, kickstart; installed binaries sha256
`2e8ca75f0890…` and `4a7d904cf409…`, paired with their commits in `~/.macgrubernetes/provenance.log`,
node `Ready` throughout and the workload process up at the first check. Pre-roll I captured the red
half myself through the API: a native-labelled `:ro` Deployment on a real host directory was refused
with build-33's `reason` containing *"this kernel does not register the null filesystem at all"* — the
falsified sentence, in a Pod status, on a live node. Post-roll the same filing is refused with:

> probe not attempted: this macOS ships no null filesystem bundle (looked for a bundle named
> nullfs.fs or null.fs under /Library/Filesystems) and no mount_nullfs helper exists, and mount(2)
> as root draws EPERM for the type nullfs because Apple gates that mount entry point behind a
> private entitlement no third-party helper can hold.

No falsified claim in it, and the stage exits **0** on the build-73 tree with the refusal lines
printed — so it passed without skipping the `:ro` block, which is the only way that green means
anything. Two details worth recording, because both are easy to read as something they are not:

- **The `probe not attempted:` prefix survived the roll.** macker build-41 still short-circuits on
  the absent bundle and helper rather than reaching `mount(2)`, and `macker volumes --probe` answers
  `probe_status: not_attempted_os` with `read_only_supported: false`. So maclet's decision here comes
  from the blockage shortcut (`readOnlyMackerBlocked` matching the bundle and helper clauses) plus the
  mount-route OS check, exactly as it did on build-33; what changed is the inherited *text*, not the
  path. A node that gets a bundle or helper is when the probe-status ladder starts deciding this.
- **The errno rows above are now two-run measurements on the node, in every mount-data shape.**
  Re-compiled probe run as root: `nullfs` → `EPERM` (1) with the data as a string, as `NULL`, and as
  an `MS_NULLFS_SB`-shaped `{spec, fspec}` struct — the refusal is at the entry point and does not
  depend on the spec being well-formed; `null`, `bindfs`, and a made-up name → `ENODEV` (19) in all
  three shapes. Both runs agree with each other and with macker's earlier relayed numbers, so the
  bindfs row that was a single reported value is now closed as fact: no `bindfs` name is registered
  on macOS 26.6.2, which is a stronger closure than the entitlement gate nullfs sits behind.

The node still refuses `:ro` and that is the correct steady state — build-41 adds no mount route,
so nothing about enforcement changed. What the roll bought is that the reason a workload is refused
is now true, on the Pod, on the node log, and in `macker volumes` output alike.
On a node whose macOS does provide a usable read-only bind mount, the same stage
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
  first. The real gap is the OS having no reachable nullfs, measured above.
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

**Conclusion as of 2026-11-25, replacing what this section previously claimed:** read-only
`hostPath` enforcement is **not achievable on macOS 26 / Darwin 25.6 arm64** -- and the reason
is narrower than "the OS has no nullfs", which is what this section used to say and which the
`EPERM` disproves. The OS provides no *reachable* nullfs: not as a helper, not as a
`mount(8)` filesystem type (no `null.fs` bundle for `mount(8)` to exec), and the VFS the
kernel does register under `nullfs` refuses its mount entry point without the private
entitlement `com.apple.private.nullfs_allow`, with no superuser fallback. It is a platform
limit, not a build gap, not a PATH gap, not a privilege gap, and not something a vendored
helper or an entitlement a third-party binary can acquire overrides. Rolling a newer Macker
does not change it. maclet therefore asks the runtime what it can enforce, falling back to
marker-plus-platform where the runtime cannot answer, and a `readOnly: true` `hostPath` mount
on a node that cannot reach nullfs is refused at admission:

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

**The scope of that refusal is `readOnly`, and nothing wider.** Writable `hostPath` works on every
darwin node in the fleet today: there is no `mount(2)`, no filesystem and no entitlement anywhere in
its path, because Macker resolves the volume with a `Symlink` into the container root. So no
statement of the form "`hostPath` is impossible on macOS 26" is true, and it is worth naming why
that sentence kept getting produced: the read-only investigation ran `mount(2)` probes, and a reader
who takes "this node cannot make a read-only mount" and drops "read-only" arrives at exactly the
false claim. The narrower truth is that the OS field `readOnly` cannot be honoured on this OS while
the whole volume source behind it is supported.

## Volume sources

| Source | On a maclet node |
| --- | --- |
| `hostPath` (`Directory`, `DirectoryOrCreate`, `File`, `FileOrCreate`) | Supported, writable **live symlink** (Macker `os.Symlink`s the host path into the container root, so writes land on the host immediately and the container sees host-side edits); `readOnly` honoured on directories **only where a nullfs mount can actually be made**, refused otherwise -- on macOS 26 nullfs *is* registered but `mount(2)` answers `EPERM` behind a private entitlement, so it is refused there and not because the filesystem is absent -- and refused on single files everywhere |
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
   [--format json]`, which names each container's own anchor and prints the kernel's
   `Evaluations`/`States` for its rule -- the build on this node answers it, so no
   upgrade is needed to ask this question.
2. A PodIP is aliased locally on the node (`route -n get <PodIP>` → interface `lo0`,
   flags `LOCAL`), so traffic the node originates to its own PodIP is never evaluated
   by PF at all. The same rule read `Evaluations: 0` under node-local attempts. A
   node-local `curl http://<PodIP>:<port>/` proves nothing in either direction.

Reproduced from maclet's side, same node, same instant, one Pod: `pf8090` with
`containerPort: 8090`, PodIP `10.42.6.4`, requested from this repository's own workload
Pod (`192.168.137.150`) -- `http://10.42.6.4:8090/` returned nginx's own 404 body, 153
bytes, while a `curl` from the node to the same address returned no HTTP status at all and
`macker ports` reported `8090:30109/tcp` with `Evaluations 251, States 1`. Off-node works
and node-local does not, on the same rule, which is the whole artifact in one measurement.

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
