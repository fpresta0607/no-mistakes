package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// withFastProviderProbe shortens the probe pacing and the deadline reserve so
// an outage wait fits a test's budget.
func withFastProviderProbe(t *testing.T) {
	t.Helper()
	interval, reserve := providerProbeInterval, providerOutageReserve
	providerProbeInterval, providerOutageReserve = 10*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { providerProbeInterval, providerOutageReserve = interval, reserve })
}

// probeResolver hands runWithRetry the same probe at every retry.
func probeResolver(probe providerProbe) func() providerProbe {
	return func() providerProbe { return probe }
}

// outageTimeline records the order of attempts and probes.
type outageTimeline struct {
	mu     sync.Mutex
	events []string
}

func (o *outageTimeline) add(event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *outageTimeline) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Join(o.events, ",")
}

// The issue #412 shape: the Test turn's first attempt ends on Claude Code's
// "Request timed out" after most of the live check budget. The retry used to be
// refused because the budget left was shorter than the failed attempt.
func TestRunWithRetry_TimedOutAttemptRetriesOnceTheProviderAnswersAndPasses(t *testing.T) {
	// Arrange
	defer withFastBackoff(t)()
	withFastProviderProbe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var timeline outageTimeline
	probe := func(context.Context) error {
		if strings.Count(timeline.String(), "probe") < 2 {
			timeline.add("probe:silent")
			return errors.New("dial tcp: i/o timeout")
		}
		timeline.add("probe:answers")
		return nil
	}
	calls := 0

	// Act
	res, err := runWithRetry(ctx, "claude", RunOpts{}, 3, claudeRetryClassifier, nil, probeResolver(probe), func() (*Result, error) {
		calls++
		timeline.add("attempt")
		if calls == 1 {
			time.Sleep(2200 * time.Millisecond)
			return nil, &claudeAPIError{Message: "Request timed out"}
		}
		return &Result{Text: "ok"}, nil
	})

	// Assert
	if err != nil || res == nil || res.Text != "ok" {
		t.Fatalf("res=%+v err=%v, want the retry to pass once the provider answers", res, err)
	}
	if got, want := timeline.String(), "attempt,probe:silent,probe:silent,probe:answers,attempt"; got != want {
		t.Fatalf("timeline = %s, want %s", got, want)
	}
}

func TestRunWithRetry_NoRetryStartsWhileTheProviderDoesNotAnswer(t *testing.T) {
	// Arrange
	defer withFastBackoff(t)()
	withFastProviderProbe(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	timedOut := &claudeAPIError{Message: "Request timed out"}
	probes := 0
	calls := 0
	var phases []string
	opts := RunOpts{OnLifecycle: func(event LifecycleEvent) { phases = append(phases, event.Phase) }}

	// Act
	_, err := runWithRetry(ctx, "claude", opts, 3, claudeRetryClassifier, nil, probeResolver(func(context.Context) error {
		probes++
		return errors.New("dial tcp: lookup api.anthropic.com: no such host")
	}), func() (*Result, error) {
		calls++
		return nil, timedOut
	})

	// Assert
	if calls != 1 {
		t.Fatalf("calls = %d, want 1: no retry may start while the provider does not answer", calls)
	}
	if probes < 2 {
		t.Fatalf("probes = %d, want the provider probed until the budget ran out", probes)
	}
	if !strings.Contains(err.Error(), "provider outage") || !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("err = %q, want it to name the provider outage and the last probe", err)
	}
	if !errors.Is(err, timedOut) || !IsTerminalProviderError(err) {
		t.Fatalf("err = %v, want a terminal error wrapping the failed attempt", err)
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, ctx = %v: the wait must give up before the step's own deadline cuts the turn", err, ctx.Err())
	}
	if got, want := strings.Join(phases, ","), LifecyclePhaseRetry+","+LifecyclePhaseProviderWait; got != want {
		t.Fatalf("lifecycle phases = %s, want %s: the wait is declared once, so the stall watcher can leave it to the deadline", got, want)
	}
}

func TestRunWithRetry_CancellationEndsTheProviderWait(t *testing.T) {
	// Arrange
	defer withFastBackoff(t)()
	withFastProviderProbe(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probes := 0

	// Act
	_, err := runWithRetry(ctx, "claude", RunOpts{}, 3, claudeRetryClassifier, nil, probeResolver(func(context.Context) error {
		probes++
		if probes == 3 {
			cancel()
		}
		return errors.New("connection refused")
	}), func() (*Result, error) {
		return nil, &claudeAPIError{Message: "API Error: Can't reach the API server (ECONNREFUSED)"}
	})

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
}

func TestRunWithRetry_ProviderAnsweringFailuresKeepTheBudgetRule(t *testing.T) {
	// Arrange
	defer withFastBackoff(t)()
	withFastProviderProbe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	probes := 0
	calls := 0

	// Act
	_, err := runWithRetry(ctx, "claude", RunOpts{}, 3, classifyTransient, nil, probeResolver(func(context.Context) error {
		probes++
		return nil
	}), func() (*Result, error) {
		calls++
		time.Sleep(400 * time.Millisecond)
		return nil, errors.New("API Error: overloaded_error: please retry")
	})

	// Assert
	if probes != 0 || calls != 1 || !strings.Contains(err.Error(), "retry skipped") {
		t.Fatalf("probes=%d calls=%d err=%v, want an overloaded provider kept on the step budget rule", probes, calls, err)
	}
}

// A settings source that routes the CLI elsewhere resolves to no probe at
// retry time, and the request timeout then keeps the plain budget rule.
func TestRunWithRetry_NoProbeAtRetryTimeKeepsTheBudgetRule(t *testing.T) {
	// Arrange
	defer withFastBackoff(t)()
	withFastProviderProbe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	resolved := 0
	calls := 0

	// Act
	_, err := runWithRetry(ctx, "claude", RunOpts{}, 3, claudeRetryClassifier, nil, func() providerProbe {
		resolved++
		return nil
	}, func() (*Result, error) {
		calls++
		time.Sleep(400 * time.Millisecond)
		return nil, &claudeAPIError{Message: "Request timed out"}
	})

	// Assert
	if resolved != 1 || calls != 1 || err == nil || !strings.Contains(err.Error(), "less than the") {
		t.Fatalf("resolved=%d calls=%d err=%v, want the probe resolved after the failure and the budget rule applied", resolved, calls, err)
	}
}

func TestClaudeProviderProbeAbsentWithoutAProber(t *testing.T) {
	if probe := (&claudeAgent{}).providerProbe(RunOpts{CWD: t.TempDir()}); probe != nil {
		t.Fatal("an agent without a prober must keep the plain retry rule")
	}
}

func TestClaudeAgent_RequestTimeoutRetriesOnceTheProviderAnswers(t *testing.T) {
	// Arrange
	defer withFastBackoff(t)()
	withFastProviderProbe(t)
	settings := newClaudeSettingsFixture(t)
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "transient-then-succeed")
	t.Setenv("NM_CLAUDE_STDIN_COUNT_FILE", filepath.Join(t.TempDir(), "attempts"))
	t.Setenv("NM_CLAUDE_STDIN_TRANSIENT_REMAIN", "1")
	t.Setenv("NM_CLAUDE_STDIN_BANNER", `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","api_error_status":null,"result":"Request timed out"}`+"\n")
	a := newClaudeStdinHelperAgent(t)
	a.settingsHost = settings.host()
	var probed []string
	a.providerAnswers = func(_ context.Context, endpoint string) error {
		probed = append(probed, endpoint)
		if len(probed) == 1 {
			return errors.New("dial tcp: i/o timeout")
		}
		return nil
	}
	attempts := 0

	// Act
	res, err := a.Run(context.Background(), RunOpts{
		Prompt:    "review",
		CWD:       t.TempDir(),
		OnAttempt: func(Attempt) { attempts++ },
	})

	// Assert
	if err != nil || res == nil || string(res.Output) != `{"ok":true}` {
		t.Fatalf("Run = %+v, %v, want the retry's structured output", res, err)
	}
	if attempts != 2 || len(probed) != 2 || probed[0] != claudeDefaultAPIBaseURL {
		t.Fatalf("attempts=%d probed=%v, want two attempts around two probes of %s", attempts, probed, claudeDefaultAPIBaseURL)
	}
}

func TestHTTPEndpointAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "not found is an answer", status: http.StatusNotFound},
		{name: "unauthorized is an answer", status: http.StatusUnauthorized},
		{name: "gateway without its upstream", status: http.StatusBadGateway, wantErr: true},
		{name: "unavailable", status: http.StatusServiceUnavailable, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			// Act
			err := httpEndpointAnswers(context.Background(), server.URL)

			// Assert
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestHTTPEndpointAnswersFailsWhenNothingListens(t *testing.T) {
	// Arrange
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL
	server.Close()

	// Act
	err := httpEndpointAnswers(context.Background(), endpoint)

	// Assert
	if err == nil {
		t.Fatal("a closed endpoint must not count as answering")
	}
}
