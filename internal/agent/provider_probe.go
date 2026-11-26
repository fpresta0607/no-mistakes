package agent

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// providerProbe reports whether the agent's model provider answers at all. A
// nil error says the route to the provider works again; it says nothing about
// whether the next model request succeeds.
type providerProbe func(ctx context.Context) error

// providerUnreachableLabels are the transient labels of a failure in which the
// provider never answered. Such an attempt lasts as long as the agent CLI's own
// connection or request timeout, however short the outage, and a retry started
// while the provider still does not answer fails the same way (issue #412).
var providerUnreachableLabels = map[string]bool{
	"connection refused":    true,
	"connection reset":      true,
	"i/o timeout":           true,
	"request timeout":       true,
	"dns lookup failed":     true,
	"dns temporary failure": true,
	"tls handshake failure": true,
}

const (
	// providerProbeTimeout bounds one probe.
	providerProbeTimeout = 10 * time.Second
	// providerOutageMaxWait bounds the wait of an invocation with no deadline.
	providerOutageMaxWait = 30 * time.Minute
)

var (
	// providerProbeInterval paces the probes while the provider does not answer.
	providerProbeInterval = 15 * time.Second
	// providerOutageReserve is how long before the invocation's deadline the
	// wait gives up, so the step reads the outage and not its own budget cut.
	providerOutageReserve = 5 * time.Second
)

// awaitProvider waits until probe reports the provider answers, probing every
// providerProbeInterval. It gives up providerOutageReserve before ctx's
// deadline, or after providerOutageMaxWait without one, with a terminal error
// that names the provider outage, so the step ends on that cause rather than on
// a budget cut or a failing check. Cancelling ctx ends the wait with ctx's error.
func awaitProvider(ctx context.Context, opts RunOpts, name string, probe providerProbe, lastErr error) error {
	started := time.Now()
	giveUp := started.Add(providerOutageMaxWait)
	limit := fmt.Sprintf("its %s outage wait limit", providerOutageMaxWait)
	if deadline, ok := ctx.Deadline(); ok && deadline.Add(-providerOutageReserve).Before(giveUp) {
		giveUp = deadline.Add(-providerOutageReserve)
		limit = "the step budget ran out"
	}
	for probes := 1; ; probes++ {
		probeDeadline := time.Now().Add(providerProbeTimeout)
		if giveUp.Before(probeDeadline) {
			probeDeadline = giveUp
		}
		probeCtx, cancel := context.WithDeadline(ctx, probeDeadline)
		probeErr := probe(probeCtx)
		cancel()
		if probeErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if probes == 1 {
			emitAgentControl(opts, LifecycleEvent{
				Agent:   name,
				Phase:   LifecyclePhaseRetry,
				Message: fmt.Sprintf("%s waiting for its model provider to answer before retrying: %v", name, probeErr),
			})
		}
		pause := min(providerProbeInterval, time.Until(giveUp))
		if pause <= 0 {
			return &exhaustedTransientRetries{err: fmt.Errorf("%s retry skipped: provider outage: the model provider did not answer for %s before %s (last probe: %v): %w",
				name, time.Since(started).Round(time.Second), limit, probeErr, lastErr)}
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// providerProbeClient does not follow redirects: a redirect is already an
// answer.
var providerProbeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// httpEndpointAnswers reports whether endpoint answers an HTTP request. Any
// response below 500, even a 404, is an answer: the route to the provider
// works. A 5xx is a gateway answering for an upstream that does not.
func httpEndpointAnswers(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := providerProbeClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("%s answered %s", req.URL.Redacted(), resp.Status)
	}
	return nil
}
