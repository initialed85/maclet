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
TEST_PORT=${MACLET_TEST_PORT:-8080}
MAX_UNAVAILABLE=0
MAX_SURGE=1
# Port forwarding is the native default; this test intentionally leaves the
# disable annotation absent so overlapping generations use distinct ports.
RUN_ID=${MACLET_TEST_RUN_ID:-$(date +%s)-$$}
NAMESPACE=${MACLET_TEST_NAMESPACE:-maclet-lifecycle-${RUN_ID}}
APP=maclet-lifecycle
HOST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/maclet-lifecycle.XXXXXX")
K=(kubectl --context "$KUBECTL_CONTEXT")

cleanup() {
  set +e
  "${K[@]}" delete namespace "$NAMESPACE" --wait=true --timeout=180s >/dev/null 2>&1
  "${K[@]}" delete namespace "$NAMESPACE" --grace-period=0 --force --wait=false >/dev/null 2>&1
  for _ in $(seq 1 30); do
    "${K[@]}" get namespace "$NAMESPACE" >/dev/null 2>&1 || break
    sleep 2
  done
  rm -rf "$HOST_DIR"
}
trap cleanup EXIT

printf 'maclet lifecycle volume %s\n' "$RUN_ID" >"$HOST_DIR/index.html"

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

# Exercise the same discovery view used while diagnosing the original issue.
"${K[@]}" -n "$NAMESPACE" get -o wide pod,service

# Deployment teardown must not leave its native Pod in Terminating.
"${K[@]}" -n "$NAMESPACE" delete deployment "$APP" --wait=true --timeout=120s >/dev/null
for _ in $(seq 1 60); do
  if [[ "$(pod_count)" == 0 ]]; then
    echo "native workload lifecycle test passed ($NAMESPACE)"
    exit 0
  fi
  sleep 2
done
"${K[@]}" -n "$NAMESPACE" get pods -o wide || true
echo "deployment teardown left native Pods behind" >&2
exit 1
