package runenv

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

func TestParseAgentEnvAdmitsTheCutoffNamesUnchanged(t *testing.T) {
	tests := []struct {
		name  string
		pairs []string
		want  map[string]string
	}{
		{name: "nil input", pairs: nil, want: nil},
		{name: "empty input", pairs: []string{}, want: nil},
		{name: "pip cutoff", pairs: []string{"PIP_UPLOADED_PRIOR_TO=2026-11-01"}, want: map[string]string{"PIP_UPLOADED_PRIOR_TO": "2026-11-01"}},
		{name: "uv relative duration with a space", pairs: []string{"UV_EXCLUDE_NEWER=7 days"}, want: map[string]string{"UV_EXCLUDE_NEWER": "7 days"}},
		{name: "npm RFC3339 timestamp", pairs: []string{"npm_config_before=2026-11-01T12:30:00Z"}, want: map[string]string{"npm_config_before": "2026-11-01T12:30:00Z"}},
		{name: "value keeps surrounding spaces and later equals signs", pairs: []string{"UV_EXCLUDE_NEWER= a=b "}, want: map[string]string{"UV_EXCLUDE_NEWER": " a=b "}},
		{
			name:  "all three names",
			pairs: []string{"npm_config_before=2026-11-01", "UV_EXCLUDE_NEWER=2026-11-01", "PIP_UPLOADED_PRIOR_TO=2026-11-01"},
			want: map[string]string{
				"PIP_UPLOADED_PRIOR_TO": "2026-11-01",
				"UV_EXCLUDE_NEWER":      "2026-11-01",
				"npm_config_before":     "2026-11-01",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAgentEnv(tc.pairs)
			if err != nil {
				t.Fatalf("ParseAgentEnv() error = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseAgentEnv() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseAgentEnvRefusesWhatItCannotAdmit(t *testing.T) {
	tests := []struct {
		name      string
		pairs     []string
		wantInErr []string
	}{
		{name: "entry without equals", pairs: []string{"UV_EXCLUDE_NEWER"}, wantInErr: []string{"UV_EXCLUDE_NEWER", "NAME=VALUE"}},
		{name: "name outside the list", pairs: []string{"FOO=bar"}, wantInErr: []string{`"FOO"`, "PIP_UPLOADED_PRIOR_TO, UV_EXCLUDE_NEWER, npm_config_before"}},
		{name: "empty name", pairs: []string{"=2026-11-01"}, wantInErr: []string{"is not accepted"}},
		{name: "name in the wrong case", pairs: []string{"NPM_CONFIG_BEFORE=2026-11-01"}, wantInErr: []string{"NPM_CONFIG_BEFORE", "is not accepted"}},
		{name: "empty value", pairs: []string{"UV_EXCLUDE_NEWER="}, wantInErr: []string{"UV_EXCLUDE_NEWER", "nonempty"}},
		{name: "newline in value", pairs: []string{"UV_EXCLUDE_NEWER=2026-11-01\nFOO=bar"}, wantInErr: []string{"UV_EXCLUDE_NEWER", "control"}},
		{name: "tab in value", pairs: []string{"npm_config_before=a\tb"}, wantInErr: []string{"npm_config_before", "control"}},
		{name: "NUL in value", pairs: []string{"npm_config_before=a\x00b"}, wantInErr: []string{"npm_config_before", "control"}},
		{name: "duplicate name", pairs: []string{"UV_EXCLUDE_NEWER=a", "UV_EXCLUDE_NEWER=b"}, wantInErr: []string{"UV_EXCLUDE_NEWER", "more than once"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAgentEnv(tc.pairs)
			if err == nil {
				t.Fatalf("ParseAgentEnv() = %#v, want an error", got)
			}
			for _, want := range tc.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
			if got != nil {
				t.Fatalf("ParseAgentEnv() = %#v on error, want nil", got)
			}
		})
	}
}

func TestValidateAgentEnvReadmitsAMapReceivedOverIPC(t *testing.T) {
	if err := ValidateAgentEnv(nil); err != nil {
		t.Fatalf("ValidateAgentEnv(nil) error = %v", err)
	}
	if err := ValidateAgentEnv(map[string]string{"UV_EXCLUDE_NEWER": "7 days"}); err != nil {
		t.Fatalf("ValidateAgentEnv() error = %v", err)
	}
	err := ValidateAgentEnv(map[string]string{"UV_EXCLUDE_NEWER": "7 days", "PATH": "/evil"})
	if err == nil || !strings.Contains(err.Error(), `"PATH"`) {
		t.Fatalf("ValidateAgentEnv() error = %v, want one naming PATH", err)
	}
}

func TestFormatAgentEnvSortsByName(t *testing.T) {
	got := FormatAgentEnv(map[string]string{
		"npm_config_before":     "b",
		"UV_EXCLUDE_NEWER":      "7 days",
		"PIP_UPLOADED_PRIOR_TO": "a",
	})
	want := []string{"PIP_UPLOADED_PRIOR_TO=a", "UV_EXCLUDE_NEWER=7 days", "npm_config_before=b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FormatAgentEnv() = %#v, want %#v", got, want)
	}
	if got := FormatAgentEnv(nil); len(got) != 0 {
		t.Fatalf("FormatAgentEnv(nil) = %#v, want empty", got)
	}
}

func TestOverlayWithSetAddsEntriesWithoutMutatingTheReceiver(t *testing.T) {
	forge := Overlay{Set: map[string]string{"GH_CONFIG_DIR": "/profiles/personal"}, Unset: []string{"GH_TOKEN"}}
	before := maps.Clone(forge.Set)

	got := forge.WithSet(map[string]string{"UV_EXCLUDE_NEWER": "7 days"})

	wantSet := map[string]string{"GH_CONFIG_DIR": "/profiles/personal", "UV_EXCLUDE_NEWER": "7 days"}
	if !reflect.DeepEqual(got.Set, wantSet) || !reflect.DeepEqual(got.Unset, []string{"GH_TOKEN"}) {
		t.Fatalf("WithSet() = %#v, want set %#v and unset [GH_TOKEN]", got, wantSet)
	}
	if !reflect.DeepEqual(forge.Set, before) {
		t.Fatalf("receiver mutated: %#v, want %#v", forge.Set, before)
	}

	fromEmpty := Overlay{}.WithSet(map[string]string{"npm_config_before": "x"})
	if !reflect.DeepEqual(fromEmpty.Set, map[string]string{"npm_config_before": "x"}) {
		t.Fatalf("WithSet() on an empty overlay = %#v", fromEmpty)
	}
	if !(Overlay{}).WithSet(nil).Empty() {
		t.Fatalf("WithSet(nil) on an empty overlay is not empty")
	}
}
