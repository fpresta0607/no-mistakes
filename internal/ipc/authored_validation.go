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
