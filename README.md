# maclet

`maclet` is an experimental Darwin/Apple Silicon Kubernetes node agent. Its
purpose is to let a macOS host participate in a K3s cluster and run selected
native workloads before there is a full Darwin container implementation.

Current capabilities include:

- authenticate to K3s with the cluster join token;
- obtain a node client certificate from K3s;
- create and maintain a Kubernetes `Node` object;
- advertise `darwin`/`arm64`, CPU/memory capacity, and report `Ready` heartbeats and Leases;
- retain the controller-assigned PodCIDR;
- optionally participate in the K3s Flannel VXLAN network;
- publish the node's InternalIP, ExternalIP, and `macker://trusted-native`
  runtime identity;
- run explicitly opted-in single-container Pods through Macker as trusted
  native processes on direct PodCIDR address aliases;
- configure macOS's `cluster.local` resolver to use the cluster's CoreDNS
  Service when VXLAN and peer credentials are available;
- expose kubelet resource metrics for `kubectl top nodes` and `kubectl top pods`;
- print pods assigned to the node as JSON.

This is **not** a secure container runtime or a full kubelet/CRI
implementation. It does not provide containerd, Linux namespaces, chroot
isolation, or CNI isolation. Macker supervises supported Pods as ordinary host
processes. The node is intentionally tainted so the scheduler will not place
ordinary workloads on it:

```text
label: kubernetes.io/os=darwin
label: kubernetes.io/arch=arm64
label: k8s-darwin.dev/native=true
taint: k8s-darwin.dev/native=true:NoSchedule
```

## Build

```sh
go test ./...
go vet ./...
go build -o maclet .
```

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the process, API, networking, DNS,
and workload-reconciliation overview. The root `main.go` is a small executable
wrapper around the implementation in `pkg/maclet`; `pkg/kube` owns the narrow
HTTPS transport and aliases the upstream Kubernetes API objects (`core/v1`,
`coordination/v1`, and
`metav1`). maclet still uses a deliberately small HTTP surface rather than
client-go. When automatic VXLAN peer discovery is enabled, it invokes the
locally installed `kubectl` only to load the selected kubeconfig as JSON; a
static `--vxlan-gateway-mac` override avoids that helper dependency.

## CI and releases

`.github/workflows/release.yml` uses a build-numbered release pattern.
GitHub Actions runs the Go tests, vet, and
lifecycle-script syntax check, then cross-compiles the root `maclet` executable
for Darwin/arm64 with CGO disabled. It uploads a tarball and SHA-256 checksum
as an artifact. Pushes to `master` and manual workflow runs also create a
GitHub release tagged `build-N`, where `N` is the Actions run number; pull
requests run the checks and build but do not publish a release.

## Join a K3s cluster

The K3s API endpoint for the current home cluster is:

```text
https://192.168.1.111:6443
```

The join token is a secret. Prefer reading it from a protected file or stdin;
do not commit it or put it in a process argument. For the current cluster,
from the Mac, a temporary token file can be prepared with:

```sh
umask 077
ssh edward@192.168.1.111 \
  'sudo cat /var/lib/rancher/k3s/server/agent-token' \
  >/tmp/maclet-token
```

Then start the long-running node heartbeat process:

```sh
./maclet join \
  --server https://192.168.1.111:6443 \
  --token-file /tmp/maclet-token \
  --node-name maclet \
  --node-ip 192.168.137.111
```

`--node-name` is optional. For a new state directory, maclet derives a
Kubernetes-compatible DNS name from the Mac's local hostname, lowercasing it
and normalizing characters that are not valid in a Node name. When reusing
existing state, maclet uses the persisted Node name; an explicitly supplied
name must match it. This follows K3s's hostname-based default while keeping the
result valid for Kubernetes. `--node-ip` is the address advertised as the
node's `InternalIP`. If omitted, maclet chooses the local address used to reach
the API server. `--external-ip` controls the Kubernetes `ExternalIP` address
and defaults to `--vxlan-local` or `--node-ip`; it is also useful for exposing
the node's underlay address in `kubectl get nodes -o wide`. The Flannel
`public-ip` annotation is the address that actually drives VXLAN peer setup.
The default state directory is `~/.maclet`; use `--state-dir` to override it.
When run through `sudo`, maclet keeps this path anchored to the invoking user's
home directory and restores ownership of newly written state files to that
user.

For a registration/heartbeat smoke test that exits after one update:

```sh
./maclet join \
  --server https://192.168.1.111:6443 \
  --node-name maclet \
  --node-ip 192.168.137.111 \
  --once
```

On first join maclet:

1. downloads `/cacerts` and validates the CA hash embedded in a full K3s
   `K10...` token;
2. authenticates to the K3s agent endpoint with the token's password as the
   `node` Basic Auth credential;
3. generates a local ECDSA key and requests a `system:node:maclet` client
   certificate;
4. creates the Node and waits for the Kubernetes controller-manager to assign a
   PodCIDR;
5. when VXLAN is enabled, starts `darwin-vxlan`, discovers its bridge MAC,
   reads the selected Linux Flannel gateway MAC through the peer kubeconfig,
   installs local Pod/Service routes, and publishes the Flannel Node
   annotations;
6. for a long-running VXLAN join, discovers the `kube-dns` Service through
   the peer client and configures `/etc/resolver/cluster.local`;
7. updates the Node status and creates the matching `kube-node-lease` Lease.

The client key, certificate, CA, node password, and state metadata are stored
under the state directory with restrictive permissions. The join token is not
stored after bootstrap. The peer kubeconfig is not copied into state; only its
path and optional context are persisted. When VXLAN is enabled, maclet uses
`kubectl config view`
with `$KUBECONFIG` or `~/.kube/config` by default to list peer Nodes and read
their Flannel annotations. Prefer a dedicated least-privilege kubeconfig
rather than an administrator kubeconfig for production use. `kubectl` must be
available on `PATH` for automatic peer discovery. Reuse that same state
directory for subsequent starts:
K3s stores only a hash of the per-node password, so a fresh bootstrap for an
existing node name cannot generate a replacement password. If the state is
lost, delete the Node and its `kube-system/<node>.node-password.k3s` Secret
before joining that name again.

The daemon refreshes Node status and the Lease every 10 seconds. Stop it with
`Ctrl-C`; the Node remains in the cluster and will eventually become `NotReady`
until maclet is started again, as with a normal node agent.

Inspect the resulting object with:

```sh
kubectl --context home-dev get node maclet -o wide --show-labels
kubectl --context home-dev get node maclet -o yaml
kubectl --context home-dev -n kube-node-lease get lease maclet
```

Node registration, API access, and workload inspection do not require root.
VXLAN mode additionally mutates the host network and starts the vmnet-backed
`darwin-vxlan` helper, so it needs root or passwordless `sudo -n`. If
`sudo -n true` succeeds, maclet remains running as the normal user and wraps
only `darwin-vxlan`, `arp`, `route`, and the macOS resolver helper with
`sudo -n`. Otherwise, rerun the complete `join` command via `sudo`. The state
remains under the invoking user's `~/.maclet` in either case.

## Inspect scheduled workloads

The `workloads` command uses the persisted client certificate to query pods
whose `spec.nodeName` is this node and emits a small JSON snapshot. When
`join` is started with VXLAN enabled, maclet also reconciles explicitly opted-in
Pods through Macker:

```sh
./maclet workloads
```

Example when no pod has been explicitly assigned:

```json
{
  "node": "maclet",
  "generatedAt": "2026-08-30T07:41:12Z",
  "workloads": []
}
```

The NoSchedule taint means a workload must explicitly tolerate the Darwin
runtime and select the node. The repository includes a complete Deployment,
Service, and Traefik Ingress example at
[`examples/nginx-native.yaml`](examples/nginx-native.yaml):

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-native
  labels:
    app: nginx-native
spec:
  replicas: 1
  # maclet's default PF port remapping gives each generation a distinct
  # internal port, so generations can overlap during a normal rollout.
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  selector:
    matchLabels:
      app: nginx-native
  template:
    metadata:
      labels:
        app: nginx-native
        k8s-darwin.dev/native: "true"
    spec:
      nodeSelector:
        k8s-darwin.dev/native: "true"
      tolerations:
        - key: k8s-darwin.dev/native
          operator: Equal
          value: "true"
          effect: NoSchedule
      volumes:
        - name: index-html
          hostPath:
            path: /Users/edwardbeech/Desktop
            type: Directory
      containers:
        - name: nginx
          image: docker.io/initialed85/nginx:latest
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
          volumeMounts:
            - name: index-html
              mountPath: /usr/share/nginx/html/index.html
              subPath: index.html
---
apiVersion: v1
kind: Service
metadata:
  name: nginx-native
  labels:
    app: nginx-native
spec:
  selector:
    app: nginx-native
  ports:
    - name: http
      port: 80
      targetPort: http
      protocol: TCP
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: nginx-native
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
spec:
  ingressClassName: traefik
  rules:
    - host: nginx.dev.initialed85.cc
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: nginx-native
                port:
                  name: http
  tls:
    - hosts:
        - nginx.dev.initialed85.cc
      secretName: nginx-dev-initialed85-cc
```

Start maclet with VXLAN and Macker, make sure the Darwin image is already in
Macker's local image store (Macker does not pull images during reconciliation),
and then apply the manifest:

```sh
macker pull docker.io/initialed85/nginx:latest
./maclet join \
  --server https://192.168.1.111:6443 \
  --node-name maclet \
  --node-ip 192.168.137.111 \
  --vxlan-binary ../darwin-vxlan/target/release/darwin-vxlan \
  --vxlan-remote 192.168.1.111 \
  --drain-timeout 10s \
  --macker-binary "$(command -v macker)"

kubectl --context home-dev apply -f examples/nginx-native.yaml
kubectl --context home-dev rollout status deployment/nginx-native
kubectl --context home-dev get pod,svc,ingress -l app=nginx-native -o wide
kubectl --context home-dev get endpointslice -l kubernetes.io/service-name=nginx-native -o wide
```

For a repeatable create/rollout/volume/teardown check, run
[`test-native-workload-lifecycle.sh`](test-native-workload-lifecycle.sh). It
uses a temporary namespace and hostPath, checks that an invalid volume leaves
one Pending Pod instead of creating replacements, verifies a valid file mount,
and ensures teardown leaves no native Pods behind. By default it uses PF port
remapping and declared port 8080; set
`MACLET_TEST_REMOTE=user@linux-node` to validate the full PodIP path from a
Linux cluster node (local macOS loopback does not exercise PF rdr rules).

Such a Pod is eligible for the initial native runtime when `join` has VXLAN
enabled and can find Macker. maclet advertises capacity for up to 110 Pods so
the Kubernetes scheduler can place workloads; this is a scheduling hint, not a
hard process or resource limit. maclet currently supports one container per
Pod, uses the PodCIDR's `.1` bridge address and `.2` synthetic gateway as
reserved addresses, and allocates workload aliases from `.3` upward.
Native processes share the host network stack. Workloads that cannot consume
a dynamic port must avoid overlap (`maxSurge: 0`, `maxUnavailable: 1`) and opt
out of PF remapping. Workloads whose image honors `MACKER_PORT_N`
use per-Pod PF remapping by default, allowing normal overlapping rollouts;
set the `k8s-darwin.dev/disable-port-forward: "true"` Pod-template annotation
only for the direct-port fallback. It invokes:

```text
macker run --detach --net=external --interface <vxlan-bridge> --ip <pod-ip>
  --host-interface <vxlan-bridge> --host-ip <bridge-ip> --name <generated-name>
  [-v HOST:CONTAINER ...] [--env KEY=VALUE ...] [--entrypoint COMMAND]
  [-p CONTAINER_PORT:auto/tcp|udp ...] IMAGE [-- ARGS...]
```

By default, each TCP/UDP container port becomes a Macker mapping such as
`-p 8080:auto/tcp`; Macker allocates and persists a unique process port, injects
it as `MACKER_PORT_N`, and PF redirects traffic addressed to that Pod IP and
declared port. The image must honor `MACKER_PORT_N` (for example, the bundled
nginx image substitutes it in its configuration). To use a fixed direct port
instead, set the Pod-template annotation
`k8s-darwin.dev/disable-port-forward: "true"`; maclet then supplies the
container port directly as `MACKER_PORT_N` and does not install PF mappings.
The Kubernetes Pod status is updated with the allocated
Pod IP, host IP, phase,
conditions, and native container state. A terminating/deleted Pod stops and
removes its Macker container and releases its address alias. The long-running
join also uses the separate peer identity (when configured) to force-delete
native Pods that remain after a 5-second cleanup window; the
restricted `system:node` identity is never used for deletion. The first runtime
slice supports writable `hostPath` volumes through Macker's live symlink-backed
`-v` mounts. `Directory`, `DirectoryOrCreate`, `File`, and
`FileOrCreate` hostPath types are supported, as is a contained `subPath`. For
file content, use a `Directory` hostPath plus `subPath` and mount the file at
its full destination (for example, `/usr/share/nginx/html/index.html`), or use
a `File` hostPath without `subPath`. Macker cannot enforce read-only mounts, so `readOnly` and `subPathExpr` mounts
are rejected. Multiple containers, non-hostPath volume sources, `valueFrom`
environment entries, custom working directories, and `hostPort` mappings are
also rejected. With a recent Macker, maclet records the actual exit code and
termination timestamps in the Kubernetes container status; older Macker
binaries retain the previous status-only fallback. Supply `--macker-binary` to
`join` when Macker is not on `PATH`; image layouts must already be available in
Macker's image store. Add `--debug` to log each quoted Macker invocation and,
when a native process exits during startup, its captured Macker logs. Debug
output can include Pod environment values, so use it only in a suitable log.

When VXLAN and Macker are enabled, maclet also serves the kubelet HTTPS
endpoint on the Node IP and port 10250 using the K3s-issued serving and client
CA certificates. This makes the standard commands work for managed native
Pods:

```sh
kubectl --context home-dev -n default logs -f POD -c CONTAINER
kubectl --context home-dev -n default exec -it POD -c CONTAINER -- EXECUTABLE ARGS...
```

`EXECUTABLE` must exist in the native image; macOS Darwin images are not
required to contain `/bin/sh`. Logs are streamed from Macker's detached log
capture. Exec uses Kubernetes SPDY stream multiplexing and delegates each
command to `macker exec`, with the
Pod's environment and working directory configuration. Resource metrics are
served from the same endpoint at `/metrics/resource`: node CPU/memory comes
from macOS host statistics, while managed native Pods report the Macker
launcher/process-tree CPU time and resident memory. With the cluster's
metrics-server installed, the normal `kubectl top nodes` and `kubectl top pods`
commands include the Darwin node and its managed Pods. This is still a
trusted native process path: it does not provide a Linux namespace or PTY
isolation boundary, and unsupported Pods are rejected by the workload
reconciler.

maclet persists ownership records in `<state-dir>/workloads.json` before
starting a native workload. On startup it reconciles those records against
Pods and Macker, removing owned containers and Pod IP aliases whose Pods no
longer exist. On `SIGINT` or `SIGTERM`, maclet first cordons its Node with a
maclet-owned marker, drains its native workloads, and then removes the
networking state. A later join removes only that marker; an operator-applied
cordon is preserved.

### Stale Pod cleanup controller

maclet also includes an optional `cleanup-controller` for API objects that are
already terminating but remain after a failed node shutdown. It must run with
a separately scoped ServiceAccount; the restricted `system:node:maclet`
identity is intentionally not granted Pod deletion:

```sh
./maclet cleanup-controller \
  --node-name maclet \
  --namespace maclet-system \
  --interval 15s \
  --stale-after 45s
```

When run inside Kubernetes, the controller uses the mounted in-cluster
ServiceAccount token. Outside the cluster, pass an explicit cleanup kubeconfig
with `--kubeconfig` and optionally `--context`. The controller lists only
`k8s-darwin.dev/native=true` Pods assigned to the selected Node and force-deletes
those whose deletion timestamp is older than `--stale-after`. Install its
`get/list/delete` Pod Role only in the namespace containing trusted-native
workloads; do not reuse a broad administrator kubeconfig for an always-on
controller. The in-process join cleanup only needs delete permission for the
workload namespaces because the node identity supplies the assigned-Pod list.
`examples/maclet-cleanup-rbac.yaml` provides the namespace-scoped
ServiceAccount, Role, and RoleBinding for this example.

### Service and Ingress caveat

The example Service is `ClusterIP`, so it does not need ServiceLB. The existing
Traefik DaemonSet listens on ports 80 and 443 on the Linux nodes and can proxy
the Service to the maclet Pod IP through Flannel. cert-manager can also use the
existing `letsencrypt` ClusterIssuer, which is configured for Traefik HTTP-01
challenges.

This remains an experimental trusted-native ingress path, but maclet now
discovers all Linux Flannel peers and installs destination-specific PodCIDR
routes and VTEP mappings. Traffic through all four Linux ingress nodes has been
validated in the development cluster. `--vxlan-remote` remains the selected
fallback for ServiceCIDR traffic; it should point at a healthy Linux Flannel
node.

The hostname must resolve to a reachable Traefik node for the HTTP-01
certificate challenge. In the development environment, check this before
applying the manifest:

```sh
dig +short nginx.dev.initialed85.cc
kubectl --context home-dev get ingressclass traefik
kubectl --context home-dev get clusterissuer letsencrypt
```

If the hostname resolves only to a private or unrelated address, the Ingress
object will still be accepted but Let's Encrypt will not be able to complete
HTTP-01 validation. The TLS secret is created asynchronously by cert-manager;
inspect it with `kubectl --context home-dev describe certificate` or
`kubectl --context home-dev describe ingress nginx-native`.

## VXLAN network handoff

maclet can launch the existing `darwin-vxlan` process and complete the
multi-peer Flannel setup after K3s assigns the Node's PodCIDR:

```sh
./maclet join \
  --server https://192.168.1.111:6443 \
  --node-name maclet \
  --node-ip 192.168.137.111 \
  --vxlan-binary ../darwin-vxlan/target/release/darwin-vxlan \
  --vxlan-remote 192.168.1.111
```

The child receives VNI `1`, UDP port `8472`, MTU `1450`, and a bridge address
made from the allocated PodCIDR (for example `10.42.4.1/24`). maclet then:

- discovers the bridge's VtepMAC;
- publishes `flannel.alpha.coreos.com/backend-type`, `backend-data`,
  `public-ip`, and `kube-subnet-manager` annotations on its Node;
- causes the existing Linux Flannel instances to install the Darwin PodCIDR
  route and permanent FDB entry automatically;
- reads every eligible Linux Flannel peer's PodCIDR, public underlay IP, and
  `flannel.1` MAC from the peer kubeconfig; use `--vxlan-gateway-mac` only as a
  static single-peer override;
- installs one synthetic gateway and ARP entry per discovered peer, reserves
  those addresses from workload allocation, and adds a route for each remote
  PodCIDR; the selected `--vxlan-remote` gateway handles the configured
  `--cluster-cidr` (`10.42.0.0/16`) and `--service-cidr` (`10.43.0.0/16`) on
  macOS.

The Kubernetes Node's `ExternalIP` is informational; Flannel uses its own
`public-ip` annotation. Peer discovery uses `$KUBECONFIG` or `~/.kube/config`
by default, and `--peer-kubeconfig`/`--peer-context` can select a dedicated
read-only credential. `--vxlan-remote` should point at a healthy Linux Flannel
node because it remains the ServiceCIDR and unmapped fallback peer. Clean
shutdown removes the annotations, per-peer routes, ARP entries, and aliases
before stopping the child. An unclean kill can leave temporary host routes or
stale remote FDB state behind.

Do not start another process that owns the same local UDP endpoint. VXLAN and
route setup require root or equivalent macOS networking entitlements.

## Cluster DNS on macOS

When VXLAN is enabled, maclet discovers the `kube-dns` Service through the
read-only peer Kubernetes client and configures the native macOS resolver at
`/etc/resolver/cluster.local`. The resolver file points at the Service's
ClusterIP, so macOS applications can use the real CoreDNS implementation rather
than a flattened copy of its records:

```text
# Managed by maclet; do not edit.
# Cluster DNS domain: cluster.local
nameserver 10.43.0.10
```

This supports the full CoreDNS behavior, including Services, headless Service
records, EndpointSlices, SRV records, and TTLs. maclet does not watch the
CoreDNS ConfigMap for records: that ConfigMap contains the Corefile and static
`NodeHosts` data, while CoreDNS watches Kubernetes resources itself. The
resolver is reconciled during startup and on each heartbeat in case the
`kube-dns` ClusterIP changes.

The resolver integration is enabled by default for long-running VXLAN joins
when a peer API client is available. If peer discovery credentials are missing,
maclet logs a warning and continues without changing host DNS. Use
`--dns-resolver=false` to leave the host resolver untouched. maclet removes its
managed resolver file during a clean shutdown and refuses to overwrite an
existing `/etc/resolver/cluster.local` file that was not created by maclet.
Verify the active resolver and a cluster name with:

```sh
scutil --dns | grep -A4 -B2 cluster.local
dscacheutil -q host -a name kubernetes.default.svc.cluster.local
```

Fully qualified names such as
`my-service.my-namespace.svc.cluster.local` are the reliable form for native
workloads; macOS does not receive each Kubernetes Pod's namespace-specific
`resolv.conf` search list.

## Current boundaries and next steps

Not implemented yet:

- complete automatic cleanup after an unclean process kill (network routes,
  ARP state, or the VXLAN child may still require manual cleanup);
- a native Darwin CNI implementation;
- full kubelet/CRI Pod lifecycle semantics, including sidecars, projected
  volumes, image pulling, and CNI integration;
- an `/etc/hosts` fallback for runtimes that ignore macOS's `/etc/resolver`
  mechanism;
- service proxying, Pod host-port mapping, or network policy;
- workload event/watch output.

The current network manager consumes the Node's assigned PodCIDR, coordinates
multi-peer Darwin VXLAN and host routes, allocates direct IP aliases, and
reconciles explicitly opted-in Pods through Macker. The reported container
runtime is `macker://trusted-native`, reflecting that workloads are supervised
by Macker rather than a Linux container runtime. The macOS resolver integration
uses CoreDNS directly; it does not attempt to make native processes look like
isolated Linux Pods.

## Removing the experiment node

Stop the long-running maclet process first (normally with `Ctrl-C`) so its
native workloads, VXLAN child, routes, ARP entries, resolver configuration, and
Flannel annotations are cleaned up. Then the `leave` command removes the
cluster-side Node, Lease, and K3s node-password Secret and deletes maclet's
persisted credentials and state files:

```sh
./maclet leave --context home-dev
```

`leave` reads the server, Node name, and peer kubeconfig from the persisted
state. Use `--kubeconfig` to select a different kubeconfig, or
`--state-dir` to select a different state directory. The kubeconfig identity
must be allowed to patch/delete the Node and delete the Lease and Secret;
maclet's restricted `system:node` certificate is intentionally insufficient.
The operation is idempotent for resources that are already absent. It does not
delete Kubernetes Pods; let the daemon drain them before leaving, and use the
separate cleanup controller for stale terminating native Pods.

Deleting the node-password Secret is necessary if the name will be reused.
K3s stores only a hash of the per-node password, so deleting local state
without deleting that Secret will make the next join fail with a password-hash
mismatch.
