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
- run explicitly opted-in single-container Pods through Macker as trusted
  native processes on direct PodCIDR address aliases;
- print pods assigned to the node as JSON.

This is **not** a secure container runtime. It does not provide kubelet,
containerd, Linux namespaces, or CNI isolation. Macker supervises supported
Pods as ordinary host processes. The node is intentionally tainted so the
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
      containers:
        - name: nginx
          image: docker.io/initialed85/nginx:latest
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
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

Such a Pod is eligible for the initial native runtime when `join` has VXLAN
enabled and can find Macker. maclet advertises capacity for up to 110 Pods so
the Kubernetes scheduler can place workloads; this is a scheduling hint, not a
hard process or resource limit. maclet currently supports one container per
Pod, uses the PodCIDR's `.1` bridge address and `.2` synthetic gateway as
reserved addresses, and allocates workload aliases from `.3` upward. It
invokes:

```text
macker run --detach --net=external --interface <vxlan-bridge> --ip <pod-ip>
  --host-interface <vxlan-bridge> --host-ip <bridge-ip> --name <generated-name>
  [-v HOST:CONTAINER ...] [--env KEY=VALUE ...] [--entrypoint COMMAND] IMAGE [-- ARGS...]
```

Container ports become `MACKER_PORT_N` environment values for image
configuration templates; they do not create host PF publications. The
Kubernetes Pod status is updated with the allocated Pod IP, host IP, phase,
conditions, and native container state. A terminating/deleted Pod stops and
removes its Macker container and releases its address alias. The first runtime
slice supports writable `hostPath` volumes through Macker's live symlink-backed
`-v` mounts. `Directory`, `DirectoryOrCreate`, `File`, and
`FileOrCreate` hostPath types are supported, as is a contained `subPath`.
Macker cannot enforce read-only mounts, so `readOnly` and `subPathExpr` mounts
are rejected. Multiple containers, non-hostPath volume sources, `valueFrom`
environment entries, custom working directories, and `hostPort` mappings are
also rejected. Supply `--macker-binary` to `join` when Macker is not on
`PATH`; image layouts must already be available in Macker's image store.

maclet persists ownership records in `<state-dir>/workloads.json` before
starting a native workload. On startup it reconciles those records against
Pods and Macker, removing owned containers and Pod IP aliases whose Pods no
longer exist. On `SIGINT` or `SIGTERM`, maclet first cordons its Node with a
maclet-owned marker, drains its native workloads, and then removes the
networking state. A later join removes only that marker; an operator-applied
cordon is preserved.

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

## Current boundaries and next steps

Not implemented yet:

- automatic cleanup after an unclean process kill;
- a native Darwin CNI implementation;
- full kubelet-compatible Pod lifecycle semantics, including sidecars,
  volume projection, image pulling, and exit-code reporting;
- `/etc/hosts`/cluster DNS synchronization;
- service proxying, Pod host-port mapping, or network policy;
- workload event/watch output.

The current network manager consumes the Node's assigned PodCIDR, coordinates
single-peer Darwin VXLAN and host routes, allocates direct IP aliases, and
reconciles explicitly opted-in Pods through Macker. The reported container
runtime is `macker://trusted-native`, reflecting that workloads are supervised
by Macker rather than a Linux container runtime. `/etc/hosts` and richer native
workload behavior can then be added without pretending that macOS provides
Linux container isolation.

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
