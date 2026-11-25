package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	nativeWorkloadLabelKey             = "k8s-darwin.dev/native"
	nativeWorkloadLabelValue           = "true"
	nativeDisablePortForwardAnnotation = "k8s-darwin.dev/disable-port-forward"
	workloadRetryDelay                 = 10 * time.Second
	defaultNativeLogRetention          = 24 * time.Hour
	maxRetainedNativeLogBytes          = 1 << 20
)

type nfsMountRecord struct {
	Target string       `json:"target"`
	Spec   nfsMountSpec `json:"spec"`
}

type managedWorkload struct {
	UID              string
	Namespace        string
	Name             string
	PodContainerName string
	ContainerName    string
	IP               string
	RestartCount     int32
	VolumePaths      []string
	HostNetwork      bool
	Retained         bool
	LogFile          string
	LogExpiresAt     time.Time
	NFSMounts        []nfsMountRecord
	RetryAfter       time.Time
}

type mackerInspection struct {
	Status            string     `json:"status"`
	PID               int        `json:"pid"`
	WorkloadPID       int        `json:"workload_pid"`
	ExitCode          *int32     `json:"exit_code"`
	StartedAt         *time.Time `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	TerminationSignal string     `json:"termination_signal"`
	TerminationReason string     `json:"termination_reason"`
}

type workloadJournalRecord struct {
	UID              string           `json:"uid"`
	Namespace        string           `json:"namespace,omitempty"`
	Name             string           `json:"name,omitempty"`
	ContainerName    string           `json:"containerName"`
	PodContainerName string           `json:"podContainerName,omitempty"`
	IP               string           `json:"ip,omitempty"`
	RestartCount     int32            `json:"restartCount,omitempty"`
	VolumePaths      []string         `json:"volumePaths,omitempty"`
	HostNetwork      bool             `json:"hostNetwork,omitempty"`
	Retained         bool             `json:"retained,omitempty"`
	LogFile          string           `json:"logFile,omitempty"`
	LogExpiresAt     string           `json:"logExpiresAt,omitempty"`
	NFSMounts        []nfsMountRecord `json:"nfsMounts,omitempty"`
}

type workloadJournal struct {
	Version   int                     `json:"version"`
	Workloads []workloadJournalRecord `json:"workloads"`
}

type workloadManager struct {
	network      *DarwinNetworkHandle
	mackerBinary string
	nodeIP       string
	apiClient    *APIClient
	journalPath  string
	volumeRoot   string
	workloads    map[string]*managedWorkload
	retained     map[string]*managedWorkload
	logsRoot     string
	nfsRoot      string
	logTTL       time.Duration
	useSudo      bool
	mountNFS     func(context.Context, bool, nfsMountSpec, string) error
	unmountNFS   func(bool, string) error
	isMountpoint func(string, nfsMountSpec) (bool, error)
	debug        bool
	mu           sync.RWMutex
	// readOnlyVolumes caches what the installed Macker binary can enforce, so a
	// Pod asking for readOnly does not re-probe on every reconciliation pass.
	readOnlyVolumes         *bool
	readOnlyVolumesProbedAt time.Time
	// readOnlyVolumesReason is why the cached answer is what it is, in the words of
	// whoever measured it -- Macker's `volumes` report where that answered, maclet's
	// own platform reading otherwise. It goes into the refusal an operator reads, so
	// the cause is never something they have to re-derive from a log.
	readOnlyVolumesReason string
	// readOnlyGuidanceLogged holds the last Macker `guidance` paragraph maclet wrote to
	// the node log, so the same checklist is not repeated on every capability re-probe.
	readOnlyGuidanceLogged string
	// readOnlyFalsifiedLogged holds the last reason maclet published while knowing one
	// of its claims to be false, so the warning about it is written once per distinct
	// reason rather than on every five-minute re-probe.
	readOnlyFalsifiedLogged string
	// readOnlyVolumesNodeRejected records that Macker advertised the read-only
	// volume marker but a launch still failed on the mount itself. It is sticky
	// until the Macker binary changes, so maclet refuses readOnly volumes at
	// admission instead of starting a Pod that is certain to fail.
	readOnlyVolumesNodeRejected bool
	readOnlyRejectedBinary      string
	// readOnlyPlatform caches whether this OS can make a read-only bind mount at
	// all. nil means use the running system; tests replace it.
	readOnlyPlatform *readOnlyPlatformProbe
	capabilityMu     sync.Mutex
	// podEvents remembers the Event maclet last filed per Pod, so a reconcile loop
	// that rewrites the same status every few seconds says it once. Tests replace
	// emitEvent to observe what would have been written.
	podEvents map[string]podEventMemory
	eventMu   sync.Mutex
	emitEvent func(context.Context, *APIClient, *Event, bool) error
}

// mackerReadOnlyVolumeRetry is how long a negative capability answer is trusted.
// It exists so a Macker upgrade is picked up without restarting maclet.
const mackerReadOnlyVolumeRetry = 5 * time.Minute

// mackerHelpOutput returns the combined output of a Macker usage command. Usage
// commands exit non-zero by design, so their error is not interesting here.
func (m *workloadManager) mackerHelpOutput(args ...string) []byte {
	command, err := m.mackerCommand(args...)
	if err != nil {
		return nil
	}
	output, _ := command.CombinedOutput()
	return output
}

// supportsReadOnlyVolumes reports whether a read-only -v mount can actually be
// enforced here. Two things have to be true: the installed Macker must accept the
// third `ro` field (its usage marker), and this node must be able to make a
// read-only bind mount at all. The answer is cached; a negative answer is retried
// after mackerReadOnlyVolumeRetry so upgrading Macker or the OS heals an
// already-joined node.
func (m *workloadManager) supportsReadOnlyVolumes() bool {
	m.capabilityMu.Lock()
	defer m.capabilityMu.Unlock()
	if m.readOnlyVolumesNodeRejected {
		if m.mackerBinaryFingerprint() == m.readOnlyRejectedBinary {
			return false
		}
		// A different Macker is installed, so its mount path may work now.
		m.readOnlyVolumesNodeRejected = false
		m.readOnlyRejectedBinary = ""
		m.readOnlyVolumes = nil
	}
	if m.readOnlyVolumes != nil && (*m.readOnlyVolumes || time.Since(m.readOnlyVolumesProbedAt) < mackerReadOnlyVolumeRetry) {
		return *m.readOnlyVolumes
	}
	supported, decidedByRuntime, reason, answeredBy := m.readOnlyVolumeCapability()
	// The evidence is logged alongside the answer, not just behind --debug, because the two
	// negative paths look identical from outside and mean different things: a Macker too old
	// to probe and a Macker that probed and was refused by the kernel both leave Pods Pending
	// with a read-only refusal, and only one of them is fixed by upgrading Macker.
	log.Printf("macker read-only volume capability for this node: supported=%t, answered by %s", supported, answeredBy)
	m.logFalsifiedReadOnlyReason(reason)
	if m.debug {
		log.Printf("debug: Macker read-only volume support detected: %t (from runtime probe: %t)", supported, decidedByRuntime)
	}
	m.readOnlyVolumes = &supported
	m.readOnlyVolumesReason = reason
	m.readOnlyVolumesProbedAt = time.Now()
	return supported
}

// readOnlyVolumesRefusalDetail returns the node-specific cause behind a negative
// capability answer, for embedding in the refusal. It probes if nothing has been
// cached yet, because the refusal is only read when the answer is already no.
func (m *workloadManager) readOnlyVolumesRefusalDetail() string {
	if !m.supportsReadOnlyVolumes() {
		m.capabilityMu.Lock()
		defer m.capabilityMu.Unlock()
		return m.readOnlyVolumesReason
	}
	return ""
}

// readOnlyVolumeCapability asks whether a read-only -v mount can actually be
// enforced here, strongest evidence first.
//
//  1. `macker volumes --probe --format json` (Macker f61c2bb+) attempts a real
//     temporary read-only mount and a write inside it, and answers with a
//     machine-readable `probe_status`. This is the only answer that cannot be produced
//     by a mount helper merely being present, so maclet branches on it.
//  2. `macker volumes --format json` (4527b97+) is the runtime's unproven verdict from
//     what it can resolve on the node. Used where the probe does not exist, and where
//     the probe reported that nobody was allowed to mount -- a privilege answer says
//     nothing about the kernel, and reading it as "unsupported" would make a node that
//     could enforce refuse Pods it has no business refusing.
//  3. The `run --help` marker and then maclet's own look at the platform, for a Macker
//     too old to answer at all. The marker is argument grammar, not capability, which
//     is why it sits last.
func (m *workloadManager) readOnlyVolumeCapability() (bool, bool, string, string) {
	if verdict, ok := parseMackerReadOnlyVerdict(m.mackerHelpOutput("volumes", "--probe", "--format", "json")); ok {
		m.logMackerReadOnlyGuidance(verdict.Guidance)
		m.logMackerProbeUnmountFailure(verdict)
		switch verdict.probeStatus() {
		case mackerProbeEnforced:
			reason := strings.TrimSpace(verdict.Reason)
			if reason == "" {
				reason = fmt.Sprintf("Macker mounted a temporary read-only volume via %q and the kernel refused a write inside it", verdict.Mechanism)
			}
			return true, true, reason, fmt.Sprintf("its probe, which mounted a temporary read-only volume (probe_status %q)", mackerProbeEnforced)
		case mackerProbePrivilege, mackerProbeNotRequest, "":
			// No mount was attempted, so the probe holds no verdict. Ask the unproven
			// question rather than treating an absent answer as a refusal.
			if plain, plainOK := parseMackerReadOnlyVerdict(m.mackerHelpOutput("volumes", "--format", "json")); plainOK {
				return m.readOnlyVerdictDecision(plain, "the read-only probe was not attempted on this node",
					fmt.Sprintf("its unproven `volumes` verdict, because its probe reported probe_status %q without attempting a mount", verdict.probeStatus()))
			}
		default:
			// mount_refused, write_accepted, helper_unavailable, not_attempted_os,
			// unmount_failed: the mount path does not hold here. `write_accepted` is the
			// case where a mount appeared to work but stayed writable, and
			// `unmount_failed` is a mount whose lifecycle the runtime could not complete;
			// maclet refuses both, because promising a Pod a read-only volume it cannot
			// guarantee is the failure this gate exists to prevent.
			//
			// Anything maclet does not recognise lands here too, and that is the contract
			// macker and maclet agreed before the code existed: `enforced` means a mount
			// existed, was read-only node-wide for its lifetime, and was released. A
			// per-process sandbox (`policy_enforced_per_process`, if seatbelt is ever
			// adopted) dies with the process and so cannot make that promise, which is why
			// it is container hardening and not `:ro` capability -- it must keep arriving
			// here, refusing, whatever its `read_only_supported` field says.
			return false, true, m.readOnlyProbeRefusal(verdict), fmt.Sprintf("its probe (probe_status %q)", verdict.probeStatus())
		}
	}
	if verdict, ok := parseMackerReadOnlyVerdict(m.mackerHelpOutput("volumes", "--format", "json")); ok {
		return m.readOnlyVerdictDecision(verdict, "", "its unproven `volumes` verdict, which this Macker build answers but whose probe does not answer here")
	}
	if m.readOnlyVolumesNodeRejected {
		return false, false, "the installed Macker failed a read-only volume mount on this node, so maclet stopped forwarding :ro until the binary changes", "a read-only mount this very Macker binary already failed on this node"
	}
	supported := mackerHelpAdvertisesReadOnlyVolumes(string(m.mackerHelpOutput("run", "--help")))
	if !supported {
		return false, false, fmt.Sprintf("the installed Macker does not accept the %q volume form, so it cannot be asked for a read-only mount; upgrade Macker (the mount lifecycle needs 363dc0c, and the capability probe that would have named the cause here needs 4527b97)", mackerReadOnlyVolumeMarker), "its `run --help`, which does not accept the :ro volume form at all"
	}
	if !m.readOnlyPlatformSupportsMounts() {
		// A Macker that advertises `:ro` on a macOS that cannot reach nullfs still accepts
		// the flag and still fails the mount, so the platform has to be checked before
		// promising a Pod its volume will be read-only.
		return false, false, "this operating system provides no usable read-only bind mount for a hostPath: no mount helper and no null filesystem bundle for the userland route. This kernel does build nullfs, and mount(2) for the registered type name answers EPERM even as root, because nullfs_mount() requires the private entitlement com.apple.private.nullfs_allow with no superuser fallback -- so neither root nor a vendored helper can create the read-only view a readOnly volume asks for.", "maclet's own reading of this operating system, because this Macker build can be asked for a verdict and cannot produce one"
	}
	return supported, false, "", "its `run --help` marker, which is argument grammar and not a capability answer"
}

// readOnlyVerdictDecision turns an unproven `macker volumes` answer into maclet's
// decision. caveat, when set, is appended so an operator reading a Pending Pod knows the
// answer was never tested against the kernel. answeredBy describes how maclet came to know
// the verdict, which is for the node log and never reaches a Pod.
func (m *workloadManager) readOnlyVerdictDecision(verdict mackerReadOnlyVerdict, caveat, answeredBy string) (bool, bool, string, string) {
	m.logMackerReadOnlyGuidance(verdict.Guidance)
	reason := strings.TrimSpace(verdict.Reason)
	if verdict.ReadOnlySupported {
		if reason == "" {
			reason = fmt.Sprintf("Macker reports it can enforce read-only volumes here via %q", verdict.Mechanism)
		}
		return true, true, reason, answeredBy
	}
	if caveat != "" && reason != "" {
		reason = reason + " (" + caveat + ")"
	}
	return false, true, reason, answeredBy
}

// logMackerReadOnlyGuidance writes Macker's node-operator checklist to the node log,
// once per distinct paragraph. It is deliberately not put in a Pod's status: guidance is
// a checklist for whoever administers the node (what to look for, which command answers
// the question, which override exists for a future OS), and a Pod cannot act on any of
// it. It is logged without the debug flag because it is the only place a node operator
// can read the runtime's reasoning for a refusal maclet has already filed, and it is
// deduplicated because a negative capability answer is re-probed every few minutes.
func (m *workloadManager) logMackerReadOnlyGuidance(guidance string) {
	guidance = strings.TrimSpace(guidance)
	if guidance == "" || guidance == m.readOnlyGuidanceLogged {
		return
	}
	m.readOnlyGuidanceLogged = guidance
	log.Printf("macker read-only volume guidance for this node: %s", guidance)
	// The same honesty check applies to the operator checklist: Macker builds 35 and 36
	// (7468fd2, da1138e) published "errno 19 on Darwin is ENOTSUP" here, and maclet copies
	// that text to the node log verbatim.
	m.logFalsifiedReadOnlyReason(guidance)
}

// mackerReadOnlyFalsifiedClaims are fragments maclet has published inside a Pod's status on
// the strength of a Macker `reason`, and has since falsified by measurement or by Apple's own
// source. maclet copies `reason` verbatim because the runtime is the authority on its own mount
// path, and that contract means maclet also inherits its accuracy -- so a node running an old
// Macker tells operators something untrue that maclet's own parity document has corrected.
//
// The refusal is still correct: on this OS no Macker build can enforce a read-only hostPath
// mount, whichever sentence names the reason. So maclet does not rewrite the runtime's text --
// silently editing a runtime's explanation would make the Pod's status uncheckable against any
// binary -- it publishes the refusal and says plainly on the node, where an operator looking at
// the runtime looks for it, that the mechanism named in the message is wrong and what replaced
// it. Nothing here ever enters a Pod's status or an Event.
var mackerReadOnlyFalsifiedClaims = []struct {
	fragment string
	instead  string
}{
	{
		fragment: "does not register the null filesystem",
		instead:  "the kernel does register it, under the type name nullfs: mount(2) for nullfs returns EPERM even as root because xnu's nullfs_mount() requires the private entitlement com.apple.private.nullfs_allow and has no superuser fallback",
	},
	{
		fragment: "registers no nullfs",
		instead:  "nullfs is registered and refuses on entitlement (EPERM at the mount entry point), which is why no helper can mount it rather than nothing being there to mount",
	},
	{
		fragment: "removed nullfs",
		instead:  "nullfs was not removed; it is present in this kernel and gated behind com.apple.private.nullfs_allow",
	},
	{
		fragment: "errno 19 on Darwin is ENOTSUP",
		instead:  "errno 19 is ENODEV on both Darwin and Linux (Apple's bsd/sys/errno.h comments it \"operation not supported by device\"); ENOTSUP is 45 on Darwin and 95 on Linux",
	},
	{
		// Macker build-40 (2250a51) named bindfs in guidance and got the closure wrong: it
		// read Apple's bind_vfsops.c, found the same entitlement gate, and published that
		// bindfs "is gated the same way" as nullfs. macker re-measured the mount(2) answer on
		// this node at build-41 (7837864) and the truth is stronger: the type name bindfs is
		// not registered at all here, drawing ENODEV (19) in every mount-data shape exactly
		// like a made-up name. Reading the source tree cannot tell you which name a shipping
		// kernel exposes, which is the trap that sentence exists to warn about.
		fragment: "bindfs is the other bind-mount VFS Apple still ships",
		instead:  "bindfs is closed at a stronger level than an entitlement refusal: on macOS 26.6.2 the type name bindfs is not registered at all and draws ENODEV (19) in all three mount-data shapes, identically to a made-up name, so there is no bindfs mount entry point on this OS for anyone to be refused by (measured by Macker at 7837864, which corrected 2250a51)",
	},
}

// logFalsifiedReadOnlyReason names any claim in the reason about to be published that maclet
// knows to be wrong. It logs once per distinct reason, because a negative capability answer is
// re-probed every five minutes so that a Macker upgrade heals the node without a restart.
func (m *workloadManager) logFalsifiedReadOnlyReason(reason string) {
	if reason == "" || reason == m.readOnlyFalsifiedLogged {
		return
	}
	for _, claim := range mackerReadOnlyFalsifiedClaims {
		if !strings.Contains(reason, claim.fragment) {
			continue
		}
		m.readOnlyFalsifiedLogged = reason
		log.Printf("the read-only refusal maclet is publishing on this node repeats a claim the runtime's reason states that maclet has since falsified: %q is wrong -- %s. The refusal itself stands; upgrade Macker so the Pod's status names the real mechanism (macker ff7b159 onward, and this node's own message is checked against macker's guidance text)",
			claim.fragment, claim.instead)
	}
}

// readOnlyProbeRefusal words a probe answer that says no. Macker's own reason is the
// node-specific cause and is already decision-shaped, so it is carried verbatim; the
// status is named alongside it because a reader needs to know whether the kernel
// refused or the OS never offered a path.
func (m *workloadManager) readOnlyProbeRefusal(verdict mackerReadOnlyVerdict) string {
	reason := strings.TrimSpace(verdict.Reason)
	if reason == "" {
		reason = fmt.Sprintf("Macker reported probe_status %q without a reason", verdict.ProbeStatus)
	}
	return reason
}

// readOnlyPlatformSupportsMounts reports whether the running OS provides nullfs,
// the only mechanism that turns a hostPath mapping into a read-only view. The
// default probe answers unconditionally off Darwin, so an injected probe is honoured
// wherever the tests run.
func (m *workloadManager) readOnlyPlatformSupportsMounts() bool {
	probe := m.readOnlyPlatform
	if probe == nil {
		defaultProbe := defaultReadOnlyPlatformProbe()
		probe = &defaultProbe
	}
	return probe.hasNullfs()
}

// noteReadOnlyVolumeNodeRejection downgrades the cached capability answer when
// Macker reports that it could not make a read-only mount, so the next Pod with a
// readOnly volume is refused at admission with maclet's own reason instead of
// being launched into the same runtime failure.
func (m *workloadManager) noteReadOnlyVolumeNodeRejection(detail string) {
	if !mackerReportsReadOnlyMountFailure(detail) {
		return
	}
	m.capabilityMu.Lock()
	defer m.capabilityMu.Unlock()
	if m.readOnlyVolumesNodeRejected {
		return
	}
	m.readOnlyVolumesNodeRejected = true
	m.readOnlyRejectedBinary = m.mackerBinaryFingerprint()
	m.readOnlyVolumesReason = strings.TrimSpace(detail)
	log.Printf("maclet: this node cannot enforce read-only volume mounts; refusing readOnly volumes rather than launching workloads that cannot mount them")
}

// mackerBinaryFingerprint identifies the installed Macker well enough to notice a
// roll, which is the event that should clear a learned node rejection.
func (m *workloadManager) mackerBinaryFingerprint() string {
	info, err := os.Stat(m.mackerBinary)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%s", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
}

func newWorkloadManager(network *DarwinNetworkHandle, mackerBinary, nodeIP string) *workloadManager {
	return newWorkloadManagerWithState(network, mackerBinary, nodeIP, "")
}

func newWorkloadManagerWithState(network *DarwinNetworkHandle, mackerBinary, nodeIP, stateDir string) *workloadManager {
	journalPath := ""
	if stateDir != "" {
		journalPath = filepath.Join(stateDir, "workloads.json")
	}
	return &workloadManager{
		network:      network,
		mackerBinary: mackerBinary,
		nodeIP:       nodeIP,
		journalPath:  journalPath,
		workloads:    make(map[string]*managedWorkload),
		retained:     make(map[string]*managedWorkload),
		volumeRoot:   filepath.Join(stateDir, "volumes"),
		logsRoot:     filepath.Join(stateDir, "logs"),
		nfsRoot:      filepath.Join(stateDir, "nfs"),
		logTTL:       defaultNativeLogRetention,
		podEvents:    make(map[string]podEventMemory),
	}
}

func (m *workloadManager) loadJournal() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadJournalLocked()
}

func (m *workloadManager) loadJournalLocked() error {
	if m.journalPath == "" {
		return nil
	}
	body, err := os.ReadFile(m.journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read workload journal: %w", err)
	}
	var journal workloadJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		return fmt.Errorf("decode workload journal: %w", err)
	}
	if journal.Version != 1 {
		return fmt.Errorf("unsupported workload journal version %d", journal.Version)
	}
	for _, record := range journal.Workloads {
		if record.UID == "" || record.ContainerName == "" {
			return errors.New("workload journal contains an incomplete record")
		}
		workload := &managedWorkload{
			UID:              record.UID,
			Namespace:        record.Namespace,
			Name:             record.Name,
			PodContainerName: record.PodContainerName,
			ContainerName:    record.ContainerName,
			IP:               record.IP,
			RestartCount:     record.RestartCount,
			VolumePaths:      append([]string(nil), record.VolumePaths...),
			HostNetwork:      record.HostNetwork,
			Retained:         record.Retained,
			LogFile:          record.LogFile,
			NFSMounts:        append([]nfsMountRecord(nil), record.NFSMounts...),
		}
		if record.LogExpiresAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339Nano, record.LogExpiresAt); parseErr == nil {
				workload.LogExpiresAt = parsed
			}
		}
		if workload.Retained {
			m.retained[record.UID] = workload
		} else {
			m.workloads[record.UID] = workload
		}
	}
	if m.pruneRetainedLogsLocked(time.Now()) {
		return m.persistJournalLocked()
	}
	return nil
}

func (m *workloadManager) persistJournal() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.persistJournalLocked()
}

func (m *workloadManager) persistJournalLocked() error {
	if m.journalPath == "" {
		return nil
	}
	records := make([]workloadJournalRecord, 0, len(m.workloads)+len(m.retained))
	appendRecord := func(workload *managedWorkload) {
		record := workloadJournalRecord{
			UID: workload.UID, Namespace: workload.Namespace, Name: workload.Name,
			PodContainerName: workload.PodContainerName, ContainerName: workload.ContainerName,
			IP: workload.IP, RestartCount: workload.RestartCount,
			VolumePaths: append([]string(nil), workload.VolumePaths...),
			HostNetwork: workload.HostNetwork, Retained: workload.Retained,
			LogFile:   workload.LogFile,
			NFSMounts: append([]nfsMountRecord(nil), workload.NFSMounts...),
		}
		if !workload.LogExpiresAt.IsZero() {
			record.LogExpiresAt = workload.LogExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		records = append(records, record)
	}
	for _, workload := range m.workloads {
		appendRecord(workload)
	}
	for _, workload := range m.retained {
		appendRecord(workload)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].UID < records[j].UID })
	body, err := json.MarshalIndent(workloadJournal{Version: 1, Workloads: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workload journal: %w", err)
	}
	if err := writePrivateFile(m.journalPath, append(body, '\n'), 0600); err != nil {
		return fmt.Errorf("write workload journal: %w", err)
	}
	return nil
}

func formatCommandArgs(args []string) string {
	quoted := make([]string, len(args))
	for index, arg := range args {
		quoted[index] = strconv.Quote(arg)
	}
	return strings.Join(quoted, " ")
}

func (m *workloadManager) mackerCommand(args ...string) (*exec.Cmd, error) {
	binary := m.mackerBinary
	if binary == "" {
		var err error
		binary, err = exec.LookPath("macker")
		if err != nil {
			return nil, errors.New("macker executable was not found; set --macker-binary or add macker to PATH")
		}
	}
	return exec.Command(binary, args...), nil
}

func (m *workloadManager) mackerOutput(args ...string) ([]byte, error) {
	command, err := m.mackerCommand(args...)
	if err != nil {
		return nil, err
	}
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return nil, fmt.Errorf("macker %s: %w", strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("macker %s: %w (%s)", strings.Join(args, " "), err, message)
	}
	return output, nil
}

func mackerImageMissing(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "image is not present in local storage") ||
		strings.Contains(message, "pull or build it first")
}

// startMackerWorkload starts a native workload and lazily pulls its Darwin
// image when the local Macker store does not contain it. Macker deliberately
// keeps pulling out of its run command, so a fresh maclet node needs this
// small orchestration layer to make an assigned Pod self-sufficient.
func (m *workloadManager) startMackerWorkload(args []string, image string) error {
	if _, err := m.mackerOutput(args...); err == nil {
		return nil
	} else if !mackerImageMissing(err) {
		return err
	} else {
		if image == "" {
			return fmt.Errorf("Macker reported a missing image but the Pod image is empty: %w", err)
		}
		if m.debug {
			log.Printf("debug: Macker image %s is not local; pulling it before retrying workload", image)
		}
		if _, pullErr := m.mackerOutput("pull", image); pullErr != nil {
			return fmt.Errorf("pull Macker image %s: %w (initial run: %v)", image, pullErr, err)
		}
		if _, retryErr := m.mackerOutput(args...); retryErr != nil {
			return fmt.Errorf("retry Macker workload after pulling image %s: %w", image, retryErr)
		}
		return nil
	}
}

func mackerContainerStatus(output, name string) (status string, found bool) {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 3 && fields[0] == name {
			return fields[2], true
		}
	}
	return "", false
}

func (m *workloadManager) containerStatus(name string) (string, bool, error) {
	output, err := m.mackerOutput("ps", "--all")
	if err != nil {
		return "", false, err
	}
	status, found := mackerContainerStatus(string(output), name)
	return status, found, nil
}

func (m *workloadManager) waitForMackerRunning(name string) error {
	// Macker's launcher can remain alive briefly after the native workload has
	// already exited. Prefer inspect's workload lifecycle status over `ps` so
	// startup failures (for example EADDRINUSE) are not reported as Ready.
	deadline := time.Now().Add(5 * time.Second)
	for {
		inspection, inspectionAvailable, err := m.containerInspection(name)
		if err != nil {
			return err
		}
		if inspectionAvailable {
			if inspection.Status != "running" {
				message := fmt.Sprintf("Macker workload exited during startup (status=%s)", inspection.Status)
				if output, logsErr := m.mackerOutput("logs", name); logsErr == nil && strings.TrimSpace(string(output)) != "" {
					message += ": " + strings.TrimSpace(string(output))
				}
				return errors.New(message)
			}
			// A launcher can report running while its child is still in a
			// startup retry loop. Hold the readiness decision until the
			// startup window has elapsed, then inspect once more.
			if time.Now().After(deadline) {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		status, found, err := m.containerStatus(name)
		if err != nil {
			return err
		}
		if found && status == "running" {
			return nil
		}
		if found && status != "running" {
			return fmt.Errorf("Macker workload exited during startup (status=%s)", status)
		}
		if time.Now().After(deadline) {
			return errors.New("Macker workload did not become running within 5 seconds")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *workloadManager) findContainer(namespace, podName, containerName string) (*managedWorkload, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, workload := range m.workloads {
		if workload.Namespace != namespace || workload.Name != podName {
			continue
		}
		if workload.PodContainerName != "" && workload.PodContainerName != containerName {
			return nil, fmt.Errorf("container %q is not managed by maclet", containerName)
		}
		copy := *workload
		return &copy, nil
	}
	for _, workload := range m.retained {
		if workload.Namespace != namespace || workload.Name != podName {
			continue
		}
		if workload.PodContainerName != "" && workload.PodContainerName != containerName {
			return nil, fmt.Errorf("container %q is not managed by maclet", containerName)
		}
		copy := *workload
		return &copy, nil
	}
	return nil, errNotFound
}

func (m *workloadManager) containerInspection(name string) (*mackerInspection, bool, error) {
	output, err := m.mackerOutput("inspect", "--format", "json", name)
	if err != nil {
		// Keep compatibility with Macker versions predating inspect. The
		// lifecycle reconciler can still use ps status, but cannot report an
		// exit code until the newer Macker binary is installed.
		return nil, false, nil
	}
	var inspection mackerInspection
	if err := json.Unmarshal(output, &inspection); err != nil {
		return nil, true, fmt.Errorf("decode Macker inspect output for %s: %w", name, err)
	}
	return &inspection, true, nil
}
