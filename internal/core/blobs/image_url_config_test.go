package blobs

import (
	"sync"
	"testing"
)

// The published configuration is process-wide, so every test here resets it
// first and on cleanup, and none of them may call t.Parallel.
func resetConfig(t *testing.T) {
	t.Helper()
	ResetImageURLConfigForTesting()
	t.Cleanup(ResetImageURLConfigForTesting)
}

// Before startup publishes anything, readers must see the proxy as disabled
// rather than some partially-initialized state. This is the value every view
// builder falls back to, and it is what makes URL generation degrade to direct
// PDS blob URLs — the configuration config.mediaProblems refuses to boot into,
// which is exactly why the default has to be predictable rather than accidental.
func TestGetImageURLConfig_BeforeSetReportsDisabled(t *testing.T) {
	resetConfig(t)

	got := GetImageURLConfig()

	if got.ProxyEnabled {
		t.Error("ProxyEnabled = true before SetImageURLConfig; the unpublished default must be disabled")
	}
	if got != (ImageURLConfig{}) {
		t.Errorf("GetImageURLConfig() = %+v before publication, want the zero value", got)
	}
}

func TestSetImageURLConfig_PublishesTheValue(t *testing.T) {
	resetConfig(t)

	want := ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.coves.social"}
	SetImageURLConfig(want)

	if got := GetImageURLConfig(); got != want {
		t.Errorf("GetImageURLConfig() = %+v, want %+v", got, want)
	}
}

// The configuration is read concurrently by request handlers, so swapping it
// mid-flight would hand different clients different URLs for the same blob. A
// second call carrying a different value is a wiring bug and must be ignored,
// not applied.
func TestSetImageURLConfig_FirstCallWins(t *testing.T) {
	resetConfig(t)

	first := ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.coves.social"}
	SetImageURLConfig(first)

	SetImageURLConfig(ImageURLConfig{ProxyEnabled: false, ProxyBaseURL: "https://other.example"})

	if got := GetImageURLConfig(); got != first {
		t.Errorf("GetImageURLConfig() = %+v after a conflicting second call, want the first value %+v", got, first)
	}
}

// A repeat call carrying the identical value is not a conflict — the latch just
// stays where it is.
func TestSetImageURLConfig_IdenticalRepeatIsHarmless(t *testing.T) {
	resetConfig(t)

	want := ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.coves.social"}
	SetImageURLConfig(want)
	SetImageURLConfig(want)

	if got := GetImageURLConfig(); got != want {
		t.Errorf("GetImageURLConfig() = %+v, want %+v", got, want)
	}
}

func TestResetImageURLConfigForTesting_ClearsTheLatch(t *testing.T) {
	resetConfig(t)

	SetImageURLConfig(ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.coves.social"})
	ResetImageURLConfigForTesting()

	if got := GetImageURLConfig(); got != (ImageURLConfig{}) {
		t.Errorf("GetImageURLConfig() = %+v after reset, want the zero value", got)
	}

	// And the latch is genuinely re-armed, not merely blanked.
	second := ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://second.example"}
	SetImageURLConfig(second)
	if got := GetImageURLConfig(); got != second {
		t.Errorf("GetImageURLConfig() = %+v, want %+v; reset must re-arm the write-once latch", got, second)
	}
}

// The RWMutex exists because handlers read this while startup may still be
// writing it. Run under -race, this is the test that justifies it.
func TestImageURLConfig_ConcurrentReadersAndWriters(t *testing.T) {
	resetConfig(t)

	const goroutines = 16
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				SetImageURLConfig(ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.coves.social"})
				return
			}
			_ = GetImageURLConfig()
		}(i)
	}

	close(start)
	wg.Wait()

	// Whichever writer got there first, the latch holds exactly one value and
	// every reader saw a coherent one.
	if got := GetImageURLConfig(); got.ProxyEnabled && got.ProxyBaseURL != "https://img.coves.social" {
		t.Errorf("GetImageURLConfig() = %+v, want a value one writer actually published", got)
	}
}
