package maclet

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	caffeinatePath = "/usr/bin/caffeinate"
	pmsetPath      = "/usr/bin/pmset"
	// defaultKeepAwakeRestartDelay is how long to wait before re-arming the assertion when
	// the helper process dies while this Node is still joined.
	defaultKeepAwakeRestartDelay = 5 * time.Second
)

// keepAwake holds the macOS power assertion that stops this Mac from sleeping while it is a
// joined Kubernetes Node.
//
// The reason a Node has to ask for this is the tunnel. The API server reaches a Darwin
// Node's kubelet through a remotedialer connection the Node dials, and the server side closes
// it when no client pong arrives. A sleeping Mac pongs nothing, so the tunnel dies, the Node
// is NotReady within the cluster's monitor grace period, and short not-ready tolerations
// evict everything scheduled on it. Measured on the fleet Mac: 136 `Entering Sleep state`
// entries in the power log (five `Maintenance Sleep` entries inside three minutes) each
// followed by a maclet tunnel reconnect, while `pmset -g assertions` showed the system-wide
// PreventSystemSleep assertion held by nothing - the always-on user-space assertion
// (PreventUserIdleSystemSleep) does not cover the maintenance sleeps that caused the flaps.
// With caffeinate holding PreventSystemSleep, the same window recorded no sleep entry at all
// and the tunnel stayed up for hours.
//
// caffeinate is the supported way to hold that assertion, and its system-sleep half is
// documented to apply only on AC power, which is the right behaviour for a laptop: unplugged
// and carried away, the Mac sleeps and the Node simply goes NotReady.
type keepAwake struct {
	command *exec.Cmd

	mu       sync.Mutex
	stopping bool
	power    string
}

func startKeepAwake(ctx context.Context) (*keepAwake, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	if _, err := exec.LookPath(caffeinatePath); err != nil {
		return nil, fmt.Errorf("macOS sleep assertion needs %s: %w", caffeinatePath, err)
	}
	awake := &keepAwake{power: powerSource()}
	go awake.supervise(ctx)
	return awake, nil
}

// supervise keeps one caffeinate process alive for the lifetime of the join session.
func (k *keepAwake) supervise(ctx context.Context) {
	for {
		if k.stopped() {
			return
		}
		command := exec.Command(caffeinatePath, "-is")
		var stderr bytes.Buffer
		command.Stderr = &stderr
		if err := command.Start(); err != nil {
			log.Printf("warning: cannot hold macOS sleep assertion via %s: %v", caffeinatePath, err)
			return
		}
		k.mu.Lock()
		k.command = command
		k.mu.Unlock()

		// The assertion is only worth reporting once per arm: a Node that cannot hold it is
		// a Node that will flap, and the operator should see that at startup rather than
		// reconstruct it from sleep logs later.
		log.Printf("holding macOS idle and system sleep assertions (%s -is, %s) while joined; --keep-awake=false opts out", caffeinatePath, k.powerDescription())
		if held, found := systemSleepAssertionHeld(); found {
			if held {
				log.Printf("macOS system-sleep assertion is held system-wide")
			} else {
				log.Printf("warning: macOS system-sleep assertion is not held system-wide after arming %s; expect this Node to go NotReady whenever the Mac sleeps", caffeinatePath)
			}
		}

		err := command.Wait()
		k.mu.Lock()
		stopping := k.stopping
		k.command = nil
		k.mu.Unlock()
		if stopping {
			return
		}
		detail := strings.TrimSpace(stderr.String())
		if err != nil || detail != "" {
			log.Printf("warning: macOS sleep assertion stopped (%v) %s; re-arming in %s", err, detail, defaultKeepAwakeRestartDelay)
		} else {
			log.Printf("warning: macOS sleep assertion stopped unexpectedly; re-arming in %s", defaultKeepAwakeRestartDelay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(defaultKeepAwakeRestartDelay):
		}
	}
}

// Close releases the assertion. The child process is owned by this process, so it is stopped
// here rather than left behind for launchd to reap.
func (k *keepAwake) Close() {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.stopping = true
	command := k.command
	k.mu.Unlock()
	if command != nil && command.Process != nil {
		if err := command.Process.Kill(); err != nil {
			log.Printf("warning: stop macOS sleep assertion helper: %v", err)
		}
	}
	log.Printf("released macOS idle and system sleep assertions")
}

func (k *keepAwake) stopped() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.stopping
}

// systemSleepAssertionHeld reads the pmset assertion report and returns whether any process
// currently holds PreventSystemSleep. found is false when the report was unreadable, which is
// deliberately different from "not held".
func systemSleepAssertionHeld() (held, found bool) {
	output, err := exec.Command(pmsetPath, "-g", "assertions").Output()
	if err != nil {
		return false, false
	}
	return parseSystemSleepAssertion(string(output))
}

// parseSystemSleepAssertion reads the system-wide PreventSystemSleep counter out of
// `pmset -g assertions`. The counter is zero on a Mac where nothing holds the assertion, and
// that is the number a Node's tunnel flapping shows up in.
func parseSystemSleepAssertion(output string) (held, found bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "PreventSystemSleep") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return false, false
		}
		return fields[1] != "0", true
	}
	return false, false
}

// parsePowerSource reports what the Mac is drawing power from, for the startup log line. The
// system-sleep assertion is documented to apply only on AC power, so a Node running on
// battery should say so out loud.
func parsePowerSource(output string) string {
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(output), "\n", 2)[0])
	for _, source := range []string{"AC Power", "Battery Power"} {
		if strings.Contains(first, source) {
			return strings.ToLower(strings.ReplaceAll(source, " Power", " power"))
		}
	}
	return "power source unknown"
}

// powerSource reads `pmset -g batt`, best effort.
func powerSource() string {
	output, err := exec.Command(pmsetPath, "-g", "batt").Output()
	if err != nil {
		return "power source unknown"
	}
	return parsePowerSource(string(output))
}

func (k *keepAwake) powerDescription() string {
	if k == nil || k.power == "" {
		return "power source unknown"
	}
	return k.power
}
