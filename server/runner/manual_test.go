package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"md-builder/server/store"
)

// TestDispatchManual checks the manual dispatch path: one graph per
// environment, manual trigger flag, the recorded commit and the per-stage
// command snapshots (an empty build command falls back to the default
// cmake recipe; empty stages are omitted).
func TestDispatchManual(t *testing.T) {
	s, err := store.Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	u := &store.User{Username: "manual", Email: "m@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	envs := make([]*store.TestEnvironment, 0, 2)
	for i, name := range []string{"cpu-man", "gpu-man"} {
		tags := "gpu"
		if i == 0 {
			tags = "cpu"
		}
		env := &store.TestEnvironment{
			OwnerID: u.ID, Name: name, Host: "h", Username: "u", PrivateKey: "k",
			Tags: tags, Enabled: true,
		}
		if err := s.CreateEnvironment(env); err != nil {
			t.Fatal(err)
		}
		envs = append(envs, env)
	}
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.example.com/group/code"}); err != nil {
		t.Fatal(err)
	}

	svc := &Service{Store: s}
	svc.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) {
		if ref != "v1.2" {
			t.Errorf("resolve ref: want v1.2, got %q", ref)
		}
		return "abcdef1234567890abcdef1234567890abcdef12", nil
	}

	roots, err := svc.DispatchManual(ManualDispatch{
		Ref:            "v1.2",
		UnitCommand:    "ctest -L unit",
		EnvironmentIDs: []int64{envs[0].ID, envs[1].ID},
		Username:       "manual",
	})
	if err != nil {
		t.Fatalf("dispatch manual: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("want 2 roots, got %d", len(roots))
	}

	// The commit is recorded under the normalized repo path.
	var commits []store.Commit
	if err := s.DB.Find(&commits).Error; err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || commits[0].SHA != "abcdef1234567890abcdef1234567890abcdef12" ||
		commits[0].Repo != "group/code" || commits[0].Ref != "v1.2" {
		t.Fatalf("unexpected commits: %+v", commits)
	}

	for _, root := range roots {
		if root.Trigger != store.TaskTriggerManual {
			t.Errorf("root %d: trigger %d, want manual", root.ID, root.Trigger)
		}
		if root.CommitID != commits[0].ID {
			t.Errorf("root %d: commit %d, want %d", root.ID, root.CommitID, commits[0].ID)
		}
		subs, err := s.ListSubTasks(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		// clone + build (default command) + unit; no regression.
		if len(subs) != 3 ||
			subs[0].Kind != store.TaskKindClone || subs[1].Kind != store.TaskKindBuild {
			t.Fatalf("root %d: unexpected first sub-tasks (%d nodes)", root.ID, len(subs))
		}
		var kinds []string
		for i := range subs {
			kinds = append(kinds, subs[i].Kind)
		}
		if !contains(kinds, store.TaskKindUnit) || contains(kinds, store.TaskKindRegression) {
			t.Fatalf("root %d: kinds %v", root.ID, kinds)
		}

		// The build snapshot carries the default cmake command; the unit
		// snapshot carries the given command.
		var rootCfg RootConfig
		if err := json.Unmarshal([]byte(root.Config), &rootCfg); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rootCfg.Entry.Build.Command, "cmake") {
			t.Errorf("root %d: build command %q, want the cmake default", root.ID, rootCfg.Entry.Build.Command)
		}
		for i := range subs {
			switch subs[i].Kind {
			case store.TaskKindBuild:
				var bc BuildStageConfig
				if err := json.Unmarshal([]byte(subs[i].Config), &bc); err != nil {
					t.Fatal(err)
				}
				if bc.Command != rootCfg.Entry.Build.Command {
					t.Errorf("root %d: build snapshot command %q", root.ID, bc.Command)
				}
			case store.TaskKindUnit:
				var sc StageConfig
				if err := json.Unmarshal([]byte(subs[i].Config), &sc); err != nil {
					t.Fatal(err)
				}
				if sc.Command != "ctest -L unit" {
					t.Errorf("root %d: unit command %q", root.ID, sc.Command)
				}
			}
		}
	}

	// Re-dispatching the same ref inserts a fresh commit row and builds a
	// new root for it (each manual attempt is its own matrix row); the old
	// root is left in place.
	again, err := svc.DispatchManual(ManualDispatch{
		Ref:               "v1.2",
		RegressionCommand: "python3 run.py",
		EnvironmentIDs:    []int64{envs[0].ID},
		Username:          "manual",
	})
	if err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if len(again) != 1 || again[0].ID == roots[0].ID {
		t.Fatalf("re-dispatch returned %+v, want a new root distinct from %d", again, roots[0].ID)
	}
	if again[0].Trigger != store.TaskTriggerManual {
		t.Errorf("re-dispatched root trigger %d, want manual", again[0].Trigger)
	}

	// Two commit rows share the (repo, sha) but belong to separate
	// attempts, each with its own root.
	var commits2 []store.Commit
	if err := s.DB.Find(&commits2).Error; err != nil {
		t.Fatal(err)
	}
	if len(commits2) != 2 {
		t.Fatalf("want 2 commit rows after re-dispatch, got %d", len(commits2))
	}
	if commits2[0].Repo != commits2[1].Repo || commits2[0].SHA != commits2[1].SHA {
		t.Fatalf("commit rows diverged: %+v", commits2)
	}
	if again[0].CommitID == roots[0].CommitID {
		t.Fatalf("re-dispatch reused commit %d, want a fresh row", again[0].CommitID)
	}

	subs, err := s.ListSubTasks(again[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range subs {
		if subs[i].Kind == store.TaskKindRegression {
			return // the fresh graph has the requested stage
		}
	}
	t.Fatal("re-dispatched root has no regression stage")
}

// TestResolveRefParsing covers the ls-remote output parsing (exact ref
// preferred, HEAD fallback, full-SHA short circuit) without git.
func TestResolveRefParsing(t *testing.T) {
	out := "abc111abc111abc111abc111abc111abc111abc1\trefs/heads/main\n" +
		"def222def222def222def222def222def222def222def2\trefs/heads/v1\n"
	sha, err := firstLSRemoteSHA(out, "refs/heads/v1")
	if err != nil || sha != "def222def222def222def222def222def222def222def2" {
		t.Fatalf("exact ref: %q %v", sha, err)
	}
	sha, err = firstLSRemoteSHA(out, "")
	if err != nil || sha != "abc111abc111abc111abc111abc111abc111abc1" {
		t.Fatalf("first line fallback: %q %v", sha, err)
	}
	if _, err := firstLSRemoteSHA("", "refs/heads/nope"); err == nil {
		t.Fatal("empty output should fail")
	}
	if !isFullSHA("ABCDEF0123456789abcdef0123456789ABCDEF01") {
		t.Fatal("40-hex should parse as a full SHA")
	}
	if isFullSHA("abc123") || isFullSHA("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz") {
		t.Fatal("non-40-hex must not parse as a full SHA")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
