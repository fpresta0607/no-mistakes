package ipc

import (
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
)

const MethodValidateAuthored = "validate_authored"

type ValidateAuthoredParams struct {
	Request   authoredvalidation.Request `json:"request"`
	CallerDir string                     `json:"caller_dir"`
}

type ValidateAuthoredResult struct {
	Receipt LaunchReceipt              `json:"receipt"`
	Family  *authoredvalidation.Family `json:"family"`
	Started bool                       `json:"started"`
}

// HasAuthoredValidation reports whether the run belongs to an authored
// validation family, including one whose provenance could not be read; no
// automatic gate resolver has standing consent for such a run.
func (r *RunInfo) HasAuthoredValidation() bool {
	return r.AuthoredValidation != nil || r.AuthoredValidationError != ""
}
