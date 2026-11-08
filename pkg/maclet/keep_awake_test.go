package maclet

import (
	"context"
	"runtime"
	"testing"
)

// assertionsWithoutKeepAwake is `pmset -g assertions` on the fleet Mac while its Node was
// flapping: the always-on user assertion is held, and the assertion that covers maintenance
// sleeps is held by nobody.
const assertionsWithoutKeepAwake = `Assertion status system-wide:
   BackgroundTask                 1
   ApplePushServiceTask           0
   UserIsActive                   0
   PreventUserIdleDisplaySleep    0
   SoftwareUpdateTask             0
   PreventSystemSleep             0
   ExternalMedia                  0
   PreventUserIdleSystemSleep     1
   NetworkClientActive            0
Listed by owning process:
   pid 30261(sushi): [0x00066f99000196bd] 00:00:10 PreventUserIdleSystemSleep named: "sushi is generating"
`

// assertionsWithKeepAwake is the same report with maclet's helper armed. The system-wide
// counter is what changes, and it is the counter the sleep log follows.
const assertionsWithKeepAwake = `Assertion status system-wide:
   BackgroundTask                 0
   ApplePushServiceTask           0
   UserIsActive                   1
   PreventUserIdleDisplaySleep    0
   SoftwareUpdateTask             0
   PreventSystemSleep             1
   ExternalMedia                  0
   PreventUserIdleSystemSleep     1
   NetworkClientActive            0
Listed by owning process:
   pid 38019(maclet): [0x0006629d000793fa] 00:22:25 PreventSystemSleep named: "maclet is joined"
`

func TestParseSystemSleepAssertion(t *testing.T) {
	if held, found := parseSystemSleepAssertion(assertionsWithoutKeepAwake); !found || held {
		t.Fatalf("report without the assertion read held=%v found=%v, want held=false found=true", held, found)
	}
	if held, found := parseSystemSleepAssertion(assertionsWithKeepAwake); !found || !held {
		t.Fatalf("report with the assertion read held=%v found=%v, want held=true found=true", held, found)
	}
	// An unreadable or partial report must not read as "nothing holds it": that is how a
	// Node would be told its assertion worked when the helper never started.
	if held, found := parseSystemSleepAssertion("Assertion status system-wide:\n   PreventSystemSleep\n"); found || held {
		t.Fatalf("counter-less report read held=%v found=%v, want found=false", held, found)
	}
	if held, found := parseSystemSleepAssertion(""); found || held {
		t.Fatalf("empty report read held=%v found=%v, want found=false", held, found)
	}
}

func TestParsePowerSource(t *testing.T) {
	battery := `Now drawing from 'Battery Power'
 -InternalBattery-0 (id=22478947)	79%; discharging; 4:12 remaining present: true`
	if source := parsePowerSource(assertionsWithoutKeepAwake); source != "power source unknown" {
		t.Fatalf("non-power report = %q, want the unknown reading", source)
	}
	if source := parsePowerSource("Now drawing from 'AC Power'\n -InternalBattery-0 (id=22478947)	79%; AC attached; not charging present: true"); source != "ac power" {
		t.Fatalf("AC report = %q, want %q", source, "ac power")
	}
	if source := parsePowerSource(battery); source != "battery power" {
		t.Fatalf("battery report = %q, want %q", source, "battery power")
	}
}

// startKeepAwake must be a no-op off Darwin rather than an error: the same binary is built
// for Linux development and CI.
func TestStartKeepAwakeOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS host: the helper is expected to arm here")
	}
	awake, err := startKeepAwake(context.Background())
	if err != nil || awake != nil {
		t.Fatalf("startKeepAwake off Darwin = (%v, %v), want (nil, nil)", awake, err)
	}
	awake.Close()
}
