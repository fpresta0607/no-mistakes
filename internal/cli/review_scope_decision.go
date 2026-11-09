package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func readReviewScopeDecision(path string) (*types.ReviewScopeDecision, error) {
	data, err := readAxiIntentFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scope decision: %w", err)
	}
	if len(data) > 32*1024 {
		return nil, fmt.Errorf("scope decision exceeds 32 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decision types.ReviewScopeDecision
	if err := decoder.Decode(&decision); err != nil {
		return nil, fmt.Errorf("decode scope decision: %w", err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("scope decision must contain exactly one JSON object")
	}
	if err := decision.Validate(); err != nil {
		return nil, err
	}
	return &decision, nil
}
