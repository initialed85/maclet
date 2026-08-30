# maclet

`maclet` is an experimental Darwin/Apple Silicon Kubernetes node agent. Its
purpose is to let a macOS host participate in a K3s cluster before there is a
full Darwin runtime or container implementation.

The first milestone is deliberately small:

- authenticate to K3s with the cluster join token;
- obtain a node client certificate from K3s;
- create and maintain a Kubernetes `Node` object;
- advertise `darwin`/`arm64` and report `Ready` heartbeats and Leases;
- retain the controller-assigned PodCIDR;
- optionally participate in the K3s Flannel VXLAN network;
- publish the node's InternalIP, ExternalIP, and `macker://trusted-native`
  runtime identity;
- print pods assigned to the node as JSON.

This is **not** a secure container runtime. It does not run kubelet,
containerd, CNI, or workloads yet. The node is intentionally tainted so the
scheduler will not place ordinary workloads on it:

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

The project uses only the Go standard library for its Kubernetes API and TLS
surface. When automatic VXLAN peer discovery is enabled, maclet invokes the
locally installed `kubectl` only to load the selected kubeconfig as JSON; a
static `--vxlan-gateway-mac` override avoids that helper dependency.

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

`--node-ip` is the address advertised as the node's `InternalIP`. If omitted,
maclet chooses the local address used to reach the API server. `--external-ip`
controls the Kubernetes `ExternalIP` address and defaults to `--vxlan-local` or
`--node-ip`; it is also useful for making the node's externally reachable
underlay address visible in `kubectl get nodes -o wide`. The Flannel
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
6. updates the Node status and creates the matching `kube-node-lease` Lease.

The client key, certificate, CA, node password, and state metadata are stored
under the state directory with restrictive permissions. The join token is not
stored after bootstrap. The peer kubeconfig is not copied into state; only its path and optional
context are persisted. When VXLAN is enabled, maclet uses `kubectl config view`
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
only `darwin-vxlan`, `arp`, and `route` operations with `sudo -n`. Otherwise,
rerun the complete `join` command via `sudo`. The state remains under the
invoking user's `~/.maclet` in either case.

## Inspect scheduled workloads

The `workloads` command uses the persisted client certificate to query pods
whose `spec.nodeName` is this node and emits a small JSON snapshot:

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
runtime and select the node. A future test manifest will look like:

```yaml
spec:
  nodeSelector:
    k8s-darwin.dev/native: "true"
  tolerations:
    - key: k8s-darwin.dev/native
      operator: Equal
      value: "true"
      effect: NoSchedule
```

Such a pod will currently remain a desired workload for maclet to report; no
runtime is implemented to execute it yet.

## VXLAN network handoff

maclet can launch the existing `darwin-vxlan` process and complete the current
single-peer Flannel setup after K3s assigns the Node's PodCIDR:

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
- reads the selected remote Linux node's `flannel.1` MAC (the `VtepMAC`) from
  the peer kubeconfig; use `--vxlan-gateway-mac` only as a static override;
- installs a synthetic gateway (the second usable address in the Darwin
  PodCIDR, for example `10.42.4.2`) and routes for the configured
  `--cluster-cidr` (`10.42.0.0/16`) and `--service-cidr` (`10.43.0.0/16`) on
  macOS.

The Kubernetes Node's `ExternalIP` is informational; Flannel uses its own
`public-ip` annotation. Peer discovery uses `$KUBECONFIG` or `~/.kube/config`
by default, and `--peer-kubeconfig`/`--peer-context` can select a dedicated
read-only credential. The remote selection is currently single-peer, so
`--vxlan-remote` should point at a healthy Linux Flannel node. Clean shutdown
removes the annotations, routes, and ARP entry before stopping the child. An
unclean kill can leave temporary host routes or stale remote FDB state behind.

Do not start another process that owns the same local UDP endpoint. VXLAN and
route setup require root or equivalent macOS networking entitlements.

## Current boundaries and next steps

Not implemented yet:

- multi-peer VXLAN fan-out and failover;
- automatic cleanup after an unclean process kill;
- a native Darwin CNI implementation;
- kubelet-compatible Pod lifecycle or container execution;
- `/etc/hosts`/cluster DNS synchronization;
- service proxying, port publishing, or network policy;
- workload event/watch output.

The current network manager consumes the Node's assigned PodCIDR, coordinates
single-peer Darwin VXLAN and host routes, and watches for explicitly opted-in
pods. The reported container runtime is `macker://trusted-native`, reflecting
that future workloads are expected to be supervised by Macker rather than a
Linux container runtime. `/etc/hosts` and native workload execution can then be
added without pretending that macOS provides Linux container isolation.

## Removing the experiment node

There is not yet a built-in `maclet leave` command. To fully unregister the
experimental node, stop maclet first (normally with `Ctrl-C`) so its VXLAN
child, routes, ARP entry, and Flannel annotations are cleaned up, then run:

```sh
kubectl --context home-dev delete node maclet --ignore-not-found
kubectl --context home-dev -n kube-node-lease delete lease maclet --ignore-not-found
kubectl --context home-dev -n kube-system delete secret maclet.node-password.k3s --ignore-not-found
rm -rf ~/.maclet
```

Deleting the node-password Secret is necessary if the name will be reused.
K3s stores only a hash of the per-node password, so deleting `~/.maclet`
without deleting that Secret will make the next join fail with a password-hash
mismatch. Do not remove the Secret while maclet is still running.
