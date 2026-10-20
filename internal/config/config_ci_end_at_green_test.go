package config

import "testing"

func TestLoadGlobal_CIEndAtGreen(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("ci_end_at_green: true\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.CIEndAtGreen {
		t.Fatal("CIEndAtGreen = false, want it on")
	}
	if merged := Merge(cfg, &RepoConfig{}); !merged.CIEndAtGreen {
		t.Fatal("merged CIEndAtGreen = false, want the machine's setting")
	}
}

// Absent means off: the run watches the pull request until it is merged or
// closed, as it always has.
func TestLoadGlobal_CIEndAtGreenDefaultsOff(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent: claude\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.CIEndAtGreen || DefaultGlobalConfig().CIEndAtGreen || MergeForRemote(cfg, &RepoConfig{}, "https://github.com/acme/widget").CIEndAtGreen {
		t.Fatal("ci_end_at_green must be off unless the machine's config sets it")
	}
}

// The machine can switch it on, or off, for one repository.
func TestMergeForRemote_CIEndAtGreenPerRepository(t *testing.T) {
	onForWidget := "repository_overrides:\n  https://github.com/acme/widget.git:\n    ci:\n      end_at_green: true\n"
	cases := []struct {
		name   string
		yaml   string
		remote string
		want   bool
	}{
		{"on for the repository the entry names", onForWidget, "https://github.com/acme/widget", true},
		{"off for any other repository", onForWidget, "https://github.com/acme/gadget", false},
		{"on everywhere from the global value", "ci_end_at_green: true\n", "https://github.com/acme/widget", true},
		{"off for one repository under a global on", "ci_end_at_green: true\nrepository_overrides:\n  https://github.com/acme/widget:\n    ci:\n      end_at_green: false\n", "https://github.com/acme/widget", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := MergeForRemote(global, &RepoConfig{}, tc.remote).CIEndAtGreen; got != tc.want {
				t.Fatalf("CIEndAtGreen for %s = %v, want %v", tc.remote, got, tc.want)
			}
		})
	}
}

// Whether the gate keeps watching a pull request is the machine's decision:
// no repository file, trusted or pushed, can switch it on.
func TestLoadRepo_CIEndAtGreenIsNotARepoSetting(t *testing.T) {
	for _, data := range []string{"ci_end_at_green: true\n", "ci:\n  end_at_green: true\n"} {
		repo, err := LoadRepoFromBytes([]byte(data))
		if err != nil {
			continue
		}
		if merged := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(repo, repo, true)); merged.CIEndAtGreen {
			t.Fatalf("the repository file %q switched ci_end_at_green on", data)
		}
	}
}
