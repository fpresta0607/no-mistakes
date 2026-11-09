package steps

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// publicPRIntent is the publication gate for ordinary, fallback, and template
// bodies and their drafting prompts. Only trusted pr.publish_intent: true
// permits publication; a caller's persisted omission can only reduce it.
// Withholding intent from drafting also prevents paraphrasing the private brief
// into public prose. Validation prompts and stored intent remain unchanged.
func publicPRIntent(sctx *pipeline.StepContext) string {
	source := publicPRIntentSource(sctx)
	if source == "" {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(sctx.Config.PR.Intent.Format), "brief") {
		return strings.TrimSpace(sctx.PublicIntentBrief)
	}
	return source
}

func publicPRIntentSource(sctx *pipeline.StepContext) string {
	if !shouldPublishPRIntent(sctx) {
		return ""
	}
	return cleanedUserIntent(sctx)
}

func shouldPublishPRIntent(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Config != nil && sctx.Config.PR.PublishesIntent() && !runOmitsIntent(sctx)
}

// runOmitsIntent reports whether this run was started with the caller-side,
// tighten-only request to keep the generated Intent section out of the PR
// body. The decision is stamped on the run row at start, so it survives
// daemon restarts and reruns.
func runOmitsIntent(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Run != nil && sctx.Run.OmitIntent
}

// prDraftIntentPromptSection is the intent section for the PR-drafting turns
// (ordinary narrative, title-only fallback, and repository-template
// narrative). It obeys the same publication gate as the generated section, read
// from the source because a brief does not exist until these turns draft it.
// Every other step prompt keeps userIntentPromptSection unchanged.
func prDraftIntentPromptSection(sctx *pipeline.StepContext) string {
	if publicPRIntentSource(sctx) == "" {
		return ""
	}
	return userIntentPromptSection(sctx)
}
