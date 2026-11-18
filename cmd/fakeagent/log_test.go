package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func readInvocations(t *testing.T, path string) []invocation {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer file.Close()
	var records []invocation
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record invocation
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("parse log line %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	return records
}

func unsetAgentEnv(t *testing.T) {
	t.Helper()
	for _, name := range agentEnvNames {
		if previous, ok := os.LookupEnv(name); ok {
			os.Unsetenv(name)
			t.Cleanup(func() { os.Setenv(name, previous) })
		}
	}
}

func TestLogInvocationRecordsOnlyPresentAgentEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want map[string]string
	}{
		{name: "none present omits the field", env: nil, want: nil},
		{
			name: "all three present",
			env: map[string]string{
				"PIP_UPLOADED_PRIOR_TO": "P7D",
				"UV_EXCLUDE_NEWER":      "7 days",
				"npm_config_before":     "2026-11-11T00:00:00Z",
			},
			want: map[string]string{
				"PIP_UPLOADED_PRIOR_TO": "P7D",
				"UV_EXCLUDE_NEWER":      "7 days",
				"npm_config_before":     "2026-11-11T00:00:00Z",
			},
		},
		{
			name: "a subset and an empty value are reported exactly",
			env:  map[string]string{"UV_EXCLUDE_NEWER": ""},
			want: map[string]string{"UV_EXCLUDE_NEWER": ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetAgentEnv(t)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			logPath := filepath.Join(t.TempDir(), "fakeagent.log")
			t.Setenv("FAKEAGENT_LOG", logPath)

			logInvocation("claude", "prompt", []string{"--print"})

			records := readInvocations(t, logPath)
			if len(records) != 1 {
				t.Fatalf("records = %d, want 1", len(records))
			}
			if !reflect.DeepEqual(records[0].Env, tt.want) {
				t.Errorf("Env = %#v, want %#v", records[0].Env, tt.want)
			}
			if records[0].Agent != "claude" || records[0].Prompt != "prompt" || !reflect.DeepEqual(records[0].Args, []string{"--print"}) {
				t.Errorf("existing fields changed: %+v", records[0])
			}
		})
	}
}

func TestLogInvocationIgnoresUnrelatedEnvironment(t *testing.T) {
	unsetAgentEnv(t)
	t.Setenv("FOO", "1")
	t.Setenv("UV_EXCLUDE_NEWER_EXTRA", "x")
	logPath := filepath.Join(t.TempDir(), "fakeagent.log")
	t.Setenv("FAKEAGENT_LOG", logPath)

	logInvocation("claude", "", nil)

	if records := readInvocations(t, logPath); len(records) != 1 || records[0].Env != nil {
		t.Fatalf("records = %+v, want one record without Env", records)
	}
}

func TestRunEnvProbeLogsItsArgsAndEnvironment(t *testing.T) {
	unsetAgentEnv(t)
	t.Setenv("npm_config_before", "2026-11-11T00:00:00Z")
	logPath := filepath.Join(t.TempDir(), "fakeagent.log")
	t.Setenv("FAKEAGENT_LOG", logPath)

	if code := run([]string{"/bin/nm-env-probe.exe", "test", "extra"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	records := readInvocations(t, logPath)
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	got := records[0]
	if got.Agent != "env-probe" || got.Prompt != "" || !reflect.DeepEqual(got.Args, []string{"test", "extra"}) {
		t.Errorf("record = %+v, want env-probe with args [test extra] and empty prompt", got)
	}
	if want := map[string]string{"npm_config_before": "2026-11-11T00:00:00Z"}; !reflect.DeepEqual(got.Env, want) {
		t.Errorf("Env = %#v, want %#v", got.Env, want)
	}
}
