package steps

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// newTestDesktop creates the hidden desktop the test step's agents run on.
// It returns nil, and the agents run as they always have, where no such
// desktop exists.
var newTestDesktop = winproc.NewHiddenDesktop

// testDesktopGuidance tells the test step's agents that they and every
// program they start run on desktop, a hidden desktop nobody sees, and how to
// keep it that way. The step stands the product up to drive it, and on the
// user's own machine a program started on the user's desktop opens its
// window over whatever the user is doing. A process passes its desktop on to
// every process it starts, so only a start handed to a process outside the
// agent's own tree can leave it, and the rules name those. It is empty where
// there is no hidden desktop.
func testDesktopGuidance(desktop string) string {
	if desktop == "" {
		return ""
	}
	return "\n- You run on a hidden desktop of your own that nobody sees, and every program you start runs there too, because a process passes its desktop on to the processes it starts. A window one of them opens does not show on the user's screen, and no-mistakes names every such window in this step's report. Never start a program in a way that leaves this desktop: not through Explorer, `start`, `Invoke-Item` or `Start-Process` given a document or URL instead of a program, which hands it to a program already running on the user's desktop; not through Task Scheduler, WMI (`Win32_Process` Create), a service or a COM server; and never switch desktops or name another desktop for a process you start. To picture a window, capture it where it is with PrintWindow or the program's own capture, or render its page headless; never move it onto the user's screen."
}

// reportTestWindows names every window a program the step's agents started
// opened on their hidden desktop: in the step log, and as informational
// findings in outcome, which do not change whether the step passes.
func reportTestWindows(sctx *pipeline.StepContext, outcome *pipeline.StepOutcome, err error, opened []winproc.Window, more int) (*pipeline.StepOutcome, error) {
	if len(opened) == 0 && more == 0 {
		return outcome, err
	}
	items := make([]Finding, 0, len(opened)+1)
	for _, window := range opened {
		items = append(items, Finding{
			Severity:    types.FindingSeverityInfo,
			Action:      types.ActionNoOp,
			Description: "a program the test agent started opened a window on the step's hidden desktop, off the user's screen: " + window.String(),
		})
	}
	if more > 0 {
		items = append(items, Finding{
			Severity:    types.FindingSeverityInfo,
			Action:      types.ActionNoOp,
			Description: fmt.Sprintf("programs the test agent started opened %d more windows on the step's hidden desktop than it names one by one", more),
		})
	}
	for _, item := range items {
		sctx.Log(item.Description)
	}
	if outcome == nil {
		return outcome, err
	}
	findings, parseErr := types.ParseFindingsJSON(outcome.Findings)
	if parseErr != nil {
		return nil, errors.Join(err, fmt.Errorf("name the windows the test agent opened in the step's findings: %w", parseErr))
	}
	findings.Items = append(findings.Items, items...)
	findingsJSON, marshalErr := json.Marshal(findings)
	if marshalErr != nil {
		return nil, errors.Join(err, fmt.Errorf("name the windows the test agent opened in the step's findings: %w", marshalErr))
	}
	outcome.Findings = string(findingsJSON)
	return outcome, err
}
