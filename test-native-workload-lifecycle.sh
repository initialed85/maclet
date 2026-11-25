#!/usr/bin/env bash
set -euo pipefail

# Exercise the native Pod lifecycle through the Kubernetes API. This deliberately
# includes one invalid volume rollout: the reconciler must leave that Pod
# Pending rather than marking it terminal (which makes a ReplicaSet create an
# unbounded replacement stream), and a later valid rollout must remove it.

KUBECTL_CONTEXT=${KUBECTL_CONTEXT:-home-dev}
IMAGE=${MACLET_TEST_IMAGE:-docker.io/initialed85/nginx:latest}
# Local macOS loopback does not exercise PF rdr rules. Set this to a reachable
# Linux cluster node (for example edward@192.168.1.111) to validate the full
# PodIP:declared-port path through Flannel and PF.
MACLET_TEST_REMOTE=${MACLET_TEST_REMOTE:-}
# The read-only volume stage needs a Macker that can enforce :ro, so it is
# opt-in until a node is known to have one. Set it to true to assert that a
# readOnly mount really denies writes and still serves reads.
MACLET_TEST_READ_ONLY=${MACLET_TEST_READ_ONLY:-false}
# A hostPath volume is a live mapping into the node's own filesystem, so the directory
# this test mounts has to exist on the Darwin node -- not on whichever machine runs the
# script. Set this to a shell on the node (for example edwardbeech@192.168.137.111) to
# create, populate, and remove it there. Leaving it unset assumes this script is running
# on the node itself, which is the only other way the mount can resolve.
MACLET_TEST_NODE_SSH=${MACLET_TEST_NODE_SSH:-}
TEST_PORT=${MACLET_TEST_PORT:-8080}
MAX_UNAVAILABLE=0
MAX_SURGE=1
# Port forwarding is the native default; this test intentionally leaves the
# disable annotation absent so overlapping generations use distinct ports.
RUN_ID=${MACLET_TEST_RUN_ID:-$(date +%s)-$$}
NAMESPACE=${MACLET_TEST_NAMESPACE:-maclet-lifecycle-${RUN_ID}}
APP=maclet-lifecycle

node_sh() {
  ssh -o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=2 "$MACLET_TEST_NODE_SSH" "$@"
}

if [[ -n "$MACLET_TEST_NODE_SSH" ]]; then
  # /private/tmp rather than /tmp because a hostPath is resolved on the node's own
  # filesystem, and /tmp there is a symlink; the resolved path is what maclet validates.
  HOST_DIR="/private/tmp/maclet-lifecycle.${RUN_ID}"
  node_sh "mkdir -p '$HOST_DIR/run' && printf 'maclet lifecycle volume %s\n' '$RUN_ID' >'$HOST_DIR/index.html'"
else
  HOST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/maclet-lifecycle.XXXXXX")
fi
K=(kubectl --context "$KUBECTL_CONTEXT")

cleanup() {
  set +e
  "${K[@]}" delete namespace "$NAMESPACE" --wait=true --timeout=180s >/dev/null 2>&1
  "${K[@]}" delete namespace "$NAMESPACE" --grace-period=0 --force --wait=false >/dev/null 2>&1
  for _ in $(seq 1 30); do
    "${K[@]}" get namespace "$NAMESPACE" >/dev/null 2>&1 || break
    sleep 2
  done
  if [[ -n "$MACLET_TEST_NODE_SSH" ]]; then
    node_sh "rm -rf '$HOST_DIR'" >/dev/null 2>&1
  else
    rm -rf "$HOST_DIR"
  fi
}
trap cleanup EXIT

if [[ -z "$MACLET_TEST_NODE_SSH" ]]; then
  printf 'maclet lifecycle volume %s\n' "$RUN_ID" >"$HOST_DIR/index.html"
fi

render() {
  local mode=$1 version=$2 volume_yaml='' mount_yaml=''
  case "$mode" in
    bad)
      volume_yaml=$(cat <<EOF
      volumes:
        - name: index-html
          hostPath:
            path: $HOST_DIR/index.html
            type: File
EOF
)
      mount_yaml=$(cat <<'EOF'
          volumeMounts:
            - name: index-html
              mountPath: /usr/share/nginx/html/index.html
              subPath: index.html
EOF
)
      ;;
    good)
      volume_yaml=$(cat <<EOF
      volumes:
        - name: index-html
          hostPath:
            path: $HOST_DIR
            type: Directory
EOF
)
      mount_yaml=$(cat <<'EOF'
          volumeMounts:
            - name: index-html
              mountPath: /usr/share/nginx/html/index.html
              subPath: index.html
EOF
)
      ;;
    # A writable mount at the directory the workload must write. This is the
    # control for the readOnly stage below: the same Pod, one field apart.
    writable-run)
      volume_yaml=$(cat <<EOF
      volumes:
        - name: run-dir
          hostPath:
            path: $HOST_DIR/run
            type: DirectoryOrCreate
EOF
)
      mount_yaml=$(cat <<'EOF'
          volumeMounts:
            - name: run-dir
              mountPath: /var/run
EOF
)
      ;;
    # nginx writes its PID file into /var/run, so a read-only mount there is a
    # write test that needs no shell in the image.
    ro-write)
      volume_yaml=$(cat <<EOF
      volumes:
        - name: run-dir
          hostPath:
            path: $HOST_DIR/run
            type: DirectoryOrCreate
EOF
)
      mount_yaml=$(cat <<'EOF'
          volumeMounts:
            - name: run-dir
              mountPath: /var/run
              readOnly: true
EOF
)
      ;;
    # readOnly must not mean unusable: serving content out of a read-only
    # document root is the expected success case.
    ro-read)
      volume_yaml=$(cat <<EOF
      volumes:
        - name: html-dir
          hostPath:
            path: $HOST_DIR
            type: Directory
EOF
)
      mount_yaml=$(cat <<'EOF'
          volumeMounts:
            - name: html-dir
              mountPath: /usr/share/nginx/html
              readOnly: true
EOF
)
      ;;
  esac
  cat <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $APP
  namespace: $NAMESPACE
  labels:
    app: $APP
spec:
  replicas: 1
  strategy:
    type: RollingUpdate
    rollingUpdate:
      # Native process generations overlap safely because the default PF
      # port remapping gives each process a distinct internal port.
      maxUnavailable: $MAX_UNAVAILABLE
      maxSurge: $MAX_SURGE
  selector:
    matchLabels:
      app: $APP
  template:
    metadata:
      labels:
        app: $APP
        k8s-darwin.dev/native: "true"
    spec:
      nodeSelector:
        k8s-darwin.dev/native: "true"
      tolerations:
        - key: k8s-darwin.dev/native
          operator: Equal
          value: "true"
          effect: NoSchedule
$volume_yaml
      containers:
        - name: nginx
          image: $IMAGE
          env:
            - name: MACLET_TEST_VERSION
              value: "$version"
          ports:
            - name: http
              containerPort: $TEST_PORT
              protocol: TCP
$mount_yaml
---
apiVersion: v1
kind: Service
metadata:
  name: $APP
  namespace: $NAMESPACE
spec:
  selector:
    app: $APP
  ports:
    - name: http
      port: 80
      targetPort: http
      protocol: TCP
EOF
}

pod_count() {
  "${K[@]}" -n "$NAMESPACE" get pods -l "app=$APP" --no-headers 2>/dev/null | awk 'NF { count++ } END { print count+0 }'
}

wait_rollout() {
  "${K[@]}" -n "$NAMESPACE" rollout status deployment/"$APP" --timeout=120s
}

wait_single_ready() {
  local deadline=$((SECONDS + 120)) count ready
  while (( SECONDS < deadline )); do
    count=$(pod_count)
    ready=$("${K[@]}" -n "$NAMESPACE" get pods -l "app=$APP" \
      -o jsonpath='{range .items[*]}{.status.phase}{"/"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
      2>/dev/null | awk '$0 == "Running/True" { count++ } END { print count+0 }')
    if [[ "$count" == 1 && "$ready" == 1 ]]; then
      return 0
    fi
    sleep 2
  done
  "${K[@]}" -n "$NAMESPACE" get pods -o wide || true
  return 1
}

wait_one_pending() {
  local deadline=$((SECONDS + 45)) count pending
  while (( SECONDS < deadline )); do
    count=$(pod_count)
    pending=$("${K[@]}" -n "$NAMESPACE" get pods -l "app=$APP" \
      -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null | awk '$0 == "Pending" { count++ } END { print count+0 }')
    # The old Pod may remain in its normal 30-second deletion grace period;
    # the invariant is one bad Pending Pod and no replacement storm.
    if (( count <= 2 )) && [[ "$pending" == 1 ]]; then
      return 0
    fi
    sleep 2
  done
  "${K[@]}" -n "$NAMESPACE" get pods -o wide || true
  return 1
}

# wait_never_ready is the readOnly invariant, and it is asserted about the Pod that
# itself asks for readOnly -- not about the Deployment. The earlier form ("no Pod with
# this app label is Ready") was wrong on a node that refuses :ro mounts: with
# maxUnavailable=0 and one surge Pod, the *previous* generation stays Ready, and that
# generation is legitimately writable, so a correct refusal read as "a workload denied
# write access became Ready". Both node classes satisfy one rule: a Pod whose own spec
# requests readOnly never becomes Ready. Where the OS enforces it the process starts and
# its writes fail with EROFS; where it cannot, maclet refuses the Pod at admission and it
# never runs writable. The phase itself is whatever the runtime reports for a process that
# cannot write its own state, so it is not asserted.
wait_never_ready() {
  local deadline=$((SECONDS + 60)) rows seen_ro=false seen_refusal=false
  sleep 20
  if (( $(pod_count) > 2 )); then
    echo "read-only volume rollout created a replacement storm" >&2
    "${K[@]}" -n "$NAMESPACE" get pods -o wide || true
    return 1
  fi
  while (( SECONDS < deadline )); do
    rows=$("${K[@]}" -n "$NAMESPACE" get pods -l "app=$APP" \
      -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.phase}{" "}{.status.reason}{" "}{.spec.containers[0].volumeMounts[*].readOnly}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
      2>/dev/null)
    while read -r name phase reason ro ready; do
      [[ -z "$name" || "$ro" != *true* ]] && continue
      seen_ro=true
      [[ "$reason" == "MacletUnsupportedPod" ]] && seen_refusal=true
      if [[ "$ready" == "True" ]]; then
        echo "Pod $name asked for a readOnly mount and became Ready; readOnly is not enforced on this node" >&2
        "${K[@]}" -n "$NAMESPACE" get pods -o wide || true
        return 1
      fi
    done <<<"$rows"
    sleep 3
  done
  if [[ "$seen_ro" != "true" ]]; then
    echo "no Pod carrying the readOnly mount reached the node; the manifest did not apply" >&2
    "${K[@]}" -n "$NAMESPACE" get pods -o wide || true
    return 1
  fi
  if [[ "$seen_refusal" == "true" ]]; then
    echo "readOnly Pod refused at admission, which is correct for a node whose OS cannot enforce it"
  fi
  return 0
}

"${K[@]}" create namespace "$NAMESPACE" >/dev/null

# Initial create and rollout.
render base one | "${K[@]}" apply -f - >/dev/null
wait_rollout
wait_single_ready

# A normal template mutation must roll out exactly one replacement.
render base two | "${K[@]}" apply -f - >/dev/null
wait_rollout
wait_single_ready

# Deliberately invalid File+subPath: one Pending Pod, no replacement storm.
render bad invalid | "${K[@]}" apply -f - >/dev/null
wait_one_pending
sleep 20
if (( $(pod_count) > 2 )); then
  echo "invalid volume rollout created a replacement storm" >&2
  exit 1
fi

# Correct Directory+subPath mapping and verify the host file is served.
render good volume | "${K[@]}" apply -f - >/dev/null
wait_rollout
wait_single_ready
POD_IP=$("${K[@]}" -n "$NAMESPACE" get pods -l "app=$APP" \
  -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.status.podIP}{"\n"}{end}' | head -n 1)
if [[ -z "$POD_IP" ]]; then
  echo "no running Pod available for volume check" >&2
  exit 1
fi
if [[ -n "$MACLET_TEST_REMOTE" ]]; then
  HTTP_OUTPUT=$(ssh -o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=2 "$MACLET_TEST_REMOTE" \
    "curl --fail --location --silent --show-error --max-time 10 http://${POD_IP}:${TEST_PORT}/index.html")
  if ! grep -F "maclet lifecycle volume $RUN_ID" <<<"$HTTP_OUTPUT" >/dev/null; then
    echo "hostPath volume content was not served" >&2
    exit 1
  fi
else
  echo "skipping remote PF/volume HTTP validation; set MACLET_TEST_REMOTE=user@linux-node" >&2
fi

# Remove the volume and roll out again.
render base three | "${K[@]}" apply -f - >/dev/null
wait_rollout
wait_single_ready

# Read-only volume semantics. maclet asks the runtime what it can enforce (see
# KUBERNETES-PARITY.md), so this stage passes on either answer: enforcement, or a refusal
# at admission. What it must never see is a :ro mount started writable.
if [[ "$MACLET_TEST_READ_ONLY" == "true" ]]; then
  render writable-run rw-control | "${K[@]}" apply -f - >/dev/null
  wait_rollout
  wait_single_ready

  render ro-write ro-denied | "${K[@]}" apply -f - >/dev/null
  wait_never_ready
  "${K[@]}" -n "$NAMESPACE" delete deployment "$APP" --wait=true >/dev/null 2>&1 || true

  render ro-read ro-served | "${K[@]}" apply -f - >/dev/null
  # Two legitimate outcomes, and the difference between them is the node's OS.
  # Where nullfs exists the mount is enforced and the Pod serves its content
  # read-only. Where it cannot (macOS 26 gates nullfs behind a private entitlement with no
  # superuser fallback, so mount(2) for the registered type `nullfs` answers EPERM even as
  # root, while the Linux name `null` answers ENODEV the way a bogus name does) maclet
  # refuses the Pod at admission rather than run that mount writable. Either way maclet
  # must never start a :ro mount it cannot enforce.
  RO_REFUSED=false
  RO_REFUSAL_MESSAGE=""
  for _ in $(seq 1 60); do
    if "${K[@]}" -n "$NAMESPACE" get deployment "$APP" -o jsonpath='{.status.readyReplicas}' 2>/dev/null | grep -qE '^[1-9]$'; then
      break
    fi
    if "${K[@]}" -n "$NAMESPACE" get pods -o jsonpath='{.items[*].status.reason}' 2>/dev/null | grep -q MacletUnsupportedPod; then
      RO_REFUSED=true
      RO_REFUSAL_MESSAGE=$("${K[@]}" -n "$NAMESPACE" get pods -o jsonpath='{range .items[*]}{.status.message}{"\n"}{end}' 2>/dev/null | grep -i readOnly | head -n 1)
      break
    fi
    sleep 2
  done
  if [[ "$RO_REFUSED" == "true" ]]; then
    # Two outcomes can produce this Pending Pod, and they are not the same thing: the
    # runtime declined because a read-only view cannot be held here, or the mount happened
    # and stayed writable. On this node both look identical as a refusal unless the reason
    # text is read -- macker asked for exactly that distinction, and maclet copies the
    # runtime's own reason into .status.message, so it is available here.
    if grep -qiE 'writ(e|ing|es).*succe|stayed writable|mounted .* writable|remained writable' <<<"$RO_REFUSAL_MESSAGE"; then
      echo "macker FAILED to enforce read-only (a mount accepted writes) rather than refusing it: $RO_REFUSAL_MESSAGE" >&2
      exit 1
    fi
    if ! grep -qiE 'no null|not register|not attempted|refus|entitlement|helper|nullfs' <<<"$RO_REFUSAL_MESSAGE"; then
      echo "refusal reason names no mechanism, so an honest refusal is indistinguishable from a bug here: $RO_REFUSAL_MESSAGE" >&2
      exit 1
    fi
    # The check above is a positive match, and a positive match alone is not enough: a reason
    # that names a mechanism which does not exist passes it. macker build-33 (dee408c) does
    # exactly that today, publishing "this kernel does not register the null filesystem at
    # all" into every refused :ro Pod's status -- nullfs IS registered, and answers EPERM at
    # an entitlement gate root cannot open. So a green stage would have been reporting an
    # honest node that was telling operators something maclet's own parity document
    # contradicts. Fail on the known-false claims instead, which makes this stage evidence
    # about truth rather than only about shape. It is expected to fail on a node running
    # macker before ff7b159 and to pass on build-38 (866c98f) onward: that is the defect, not
    # a flake, and the refusal it grades is itself correct in both cases.
    if grep -qiE 'does not register|registers no|nullfs (is |was )?removed|removed nullfs|no nullfs (vfs|filesystem)( |,|$)|errno 19 on Darwin is ENOTSUP' <<<"$RO_REFUSAL_MESSAGE"; then
      echo "refusal reason asserts a mechanism maclet has falsified, so this node is publishing an untrue cause to Pods (upgrade macker to ff7b159 or later): $RO_REFUSAL_MESSAGE" >&2
      exit 1
    fi
    echo "node cannot enforce read-only hostPath mounts: :ro Pods refused at admission, as designed"
    echo "  refusal reason: $(tr -d '\n' <<<"$RO_REFUSAL_MESSAGE" | cut -c1-170)"
  else
    wait_rollout
    wait_single_ready
    RO_POD_IP=$("${K[@]}" -n "$NAMESPACE" get pods -l "app=$APP" \
      -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.status.podIP}{"\n"}{end}' | head -n 1)
    if [[ -n "$MACLET_TEST_REMOTE" && -n "$RO_POD_IP" ]]; then
      RO_OUTPUT=$(ssh -o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=2 "$MACLET_TEST_REMOTE" \
        "curl --fail --location --silent --show-error --max-time 10 http://${RO_POD_IP}:${TEST_PORT}/index.html")
      if ! grep -F "maclet lifecycle volume $RUN_ID" <<<"$RO_OUTPUT" >/dev/null; then
        echo "a read-only mounted document root did not serve its content" >&2
        exit 1
      fi
    fi
  fi
  "${K[@]}" -n "$NAMESPACE" delete deployment "$APP" --wait=true >/dev/null 2>&1 || true
fi

# Exercise the same discovery view used while diagnosing the original issue.
"${K[@]}" -n "$NAMESPACE" get -o wide pod,service

# Deployment teardown must not leave its native Pod behind. Stated as two bounded
# facts rather than "the namespace is empty right now", because the API cascade and
# maclet's acknowledgement are separate clocks: a Pod can exist for a moment after
# `delete deployment --wait` returns, still without a deletion timestamp, while the
# ReplicaSet cascade catches up, and that is not a maclet failure. What is one: a Pod
# that never gets a deletion timestamp, or one that has it and is not removed, because
# on a darwin node only maclet can finish that deletion.
# The read-only block above may already have deleted the Deployment, and a delete of a
# missing object is not a teardown failure -- the assertion is what happens to the Pods.
"${K[@]}" -n "$NAMESPACE" delete deployment "$APP" --wait=true --timeout=120s >/dev/null 2>&1 || true
deadline=$((SECONDS + 180))
acked_seen=false
last_unacked=""
while (( SECONDS < deadline )); do  rows=$("${K[@]}" -n "$NAMESPACE" get pods -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.metadata.deletionTimestamp}{"\n"}{end}' 2>/dev/null)
  if [[ -z "${rows//[$'\n']/}" ]]; then
    echo "native workload lifecycle test passed ($NAMESPACE)"
    exit 0
  fi
  unacked=""
  while IFS='|' read -r name deleted_at; do
    [[ -z "$name" ]] && continue
    if [[ -n "$deleted_at" ]]; then
      acked_seen=true
    else
      unacked="$unacked $name"
    fi
  done <<<"$rows"
  last_unacked="$unacked"
  sleep 3
done
"${K[@]}" -n "$NAMESPACE" get pods -o wide || true
if [[ "$acked_seen" == "true" ]]; then
  echo "teardown left a Pod in Terminating: maclet did not acknowledge its deletion within 180s" >&2
else
  echo "teardown left Pod(s) without any deletion timestamp:${last_unacked}" >&2
fi
exit 1
