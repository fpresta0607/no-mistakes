package agentcfg

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Selection describes explicit native launch knobs, never served-model,
// credential, subscription or billing evidence. Claude has no service-tier
// launch knob, so its selections leave ServiceTier empty.
type Selection struct {
	Harness     types.AgentName `json:"harness"`
	Model       string          `json:"model"`
	Effort      Effort          `json:"effort"`
	ServiceTier string          `json:"service_tier,omitempty"`
}

func (s Selection) Validate() error {
	switch s.Harness {
	case types.AgentCodex:
		if !isSelectionToken(s.Model) || !isSelectionToken(s.ServiceTier) {
			return fmt.Errorf("launch assertion requires explicit Codex model and service-tier identifiers")
		}
	case types.AgentClaude:
		if !isSelectionToken(s.Model) || s.ServiceTier != "" {
			return fmt.Errorf("launch assertion requires an explicit Claude model and no service tier")
		}
	default:
		return fmt.Errorf("launch assertions verify only Codex and Claude selection mechanisms")
	}
	effort, err := ParseEffort(string(s.Effort))
	if err != nil || effort != s.Effort || effort == "" {
		return fmt.Errorf("launch assertion requires an explicit native effort")
	}
	return nil
}

// EffectiveSelection follows the same NativeArgs precedence as NewWithOptions.
// User-home profiles and implicit defaults cannot prove an opt-in assertion;
// refuse selectors we cannot interpret rather than reporting requested knobs.
func EffectiveSelection(name types.AgentName, profile Profile, rawArgs []string) (Selection, error) {
	if name != types.AgentCodex && name != types.AgentClaude {
		return Selection{}, fmt.Errorf("agent selection has no verified launch-assertion mechanism")
	}
	if err := Validate(name, profile); err != nil {
		return Selection{}, err
	}
	args := append(append([]string(nil), rawArgs...), NativeArgs(name, profile, rawArgs)...)
	var selection Selection
	var err error
	if name == types.AgentCodex {
		selection, err = codexSelection(args)
	} else {
		selection, err = claudeSelection(args)
	}
	if err != nil {
		return Selection{}, err
	}
	return selection, selection.Validate()
}

func codexSelection(args []string) (Selection, error) {
	selection := Selection{Harness: types.AgentCodex}
	flagModel := ""
	configModel := ""
	for index := 0; index < len(args); index++ {
		argument := args[index]
		flag, value, hasInlineValue := strings.Cut(argument, "=")
		switch flag {
		case "-m", "--model", "-c", "--config":
			if !hasInlineValue {
				index++
				if index >= len(args) {
					return Selection{}, fmt.Errorf("incomplete native selection argument")
				}
				value = args[index]
			}
			if flag == "-m" || flag == "--model" {
				if flagModel != "" || !isSelectionToken(value) {
					return Selection{}, fmt.Errorf("ambiguous native model selection")
				}
				flagModel = value
				continue
			}
			key, nativeValue, hasAssignment := strings.Cut(value, "=")
			key = strings.TrimSpace(key)
			if !hasAssignment {
				return Selection{}, fmt.Errorf("invalid native config assignment")
			}
			switch key {
			case "model", "model_reasoning_effort", "service_tier":
				resolved, err := selectionConfigString(nativeValue)
				if err != nil {
					return Selection{}, err
				}
				switch key {
				case "model":
					configModel = resolved
				case "model_reasoning_effort":
					selection.Effort = Effort(resolved)
				case "service_tier":
					selection.ServiceTier = resolved
				}
			case "project_doc_max_bytes":
				// This suppression knob does not select a model or profile.
			default:
				return Selection{}, fmt.Errorf("native config override is not verifiable by launch assertions")
			}
		case "--ignore-rules", "--full-auto", "--dangerously-bypass-approvals-and-sandbox":
			if hasInlineValue {
				return Selection{}, fmt.Errorf("invalid native execution argument")
			}
		default:
			return Selection{}, fmt.Errorf("native argument is not verifiable by launch assertions")
		}
	}
	if flagModel != "" && configModel != "" && flagModel != configModel {
		return Selection{}, fmt.Errorf("conflicting native model selectors")
	}
	selection.Model = configModel
	if flagModel != "" {
		selection.Model = flagModel
	}
	return selection, nil
}

// claudeSelection reads claude's --model and --effort flags, which the CLI
// documents as overriding ANTHROPIC_MODEL and the model, effortLevel and
// modelSettings settings. A repeated selector is refused rather than resolved
// by guessing claude's own last-wins rule.
func claudeSelection(args []string) (Selection, error) {
	selection := Selection{Harness: types.AgentClaude}
	for index := 0; index < len(args); index++ {
		flag, value, hasInlineValue := strings.Cut(args[index], "=")
		switch flag {
		case "--model", "--effort", "--setting-sources", "--permission-mode":
			if !hasInlineValue {
				index++
				if index >= len(args) {
					return Selection{}, fmt.Errorf("incomplete native selection argument")
				}
				value = args[index]
			}
			switch flag {
			case "--model":
				if selection.Model != "" || !isSelectionToken(value) {
					return Selection{}, fmt.Errorf("ambiguous native model selection")
				}
				selection.Model = value
			case "--effort":
				if selection.Effort != "" {
					return Selection{}, fmt.Errorf("ambiguous native effort selection")
				}
				selection.Effort = Effort(value)
			}
			// --setting-sources and --permission-mode choose which settings
			// files load and the tool policy; the flags above override any
			// model or effort those settings carry.
		case "--dangerously-skip-permissions":
			if hasInlineValue {
				return Selection{}, fmt.Errorf("invalid native execution argument")
			}
		default:
			return Selection{}, fmt.Errorf("native argument is not verifiable by launch assertions")
		}
	}
	return selection, nil
}

func selectionConfigString(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || (value[0] != '"' && value[0] != '\'') || value[len(value)-1] != value[0] {
		return "", fmt.Errorf("native selection config must contain a literal string")
	}
	value = value[1 : len(value)-1]
	if !isSelectionToken(value) {
		return "", fmt.Errorf("native selection config must contain an exact identifier")
	}
	return value, nil
}

func isSelectionToken(value string) bool {
	if value == "" || len(value) > 256 || value[0] == '-' {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._-/", character)) {
			return false
		}
	}
	return true
}
