package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestEnvironmentCRUD(t *testing.T) {
	s := newTestStore(t)
	u := &User{Username: "envowner", Email: "env@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Create.
	env := &TestEnvironment{
		OwnerID:    u.ID,
		Name:       "cpu-node-1",
		Host:       "192.168.1.10",
		Username:   "runner",
		PrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----",
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatalf("create: %v", err)
	}
	if env.ID == 0 {
		t.Fatal("expected ID set after create")
	}

	// List (site-wide: the pool is shared, the API layer decides who may edit).
	envs, err := s.ListAllEnvironments()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(envs) != 1 || envs[0].Name != "cpu-node-1" {
		t.Fatalf("unexpected list: %+v", envs)
	}

	// Another user's row shows up in the same list: the store does not filter
	// by owner, and the environment carries the owner the API layer checks.
	other := &User{Username: "other", Email: "other@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(other); err != nil {
		t.Fatalf("create other user: %v", err)
	}
	foreign := &TestEnvironment{
		OwnerID: other.ID, Name: "gpu-a100", Host: "h", Username: "u", PrivateKey: "k",
	}
	if err := s.CreateEnvironment(foreign); err != nil {
		t.Fatalf("create foreign: %v", err)
	}
	envs, err = s.ListAllEnvironments()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("expected both owners' environments, got %d", len(envs))
	}

	// Get.
	got, err := s.GetEnvironment(env.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Host != "192.168.1.10" || got.Username != "runner" || got.OwnerID != u.ID {
		t.Fatalf("unexpected env: %+v", got)
	}

	// Update.
	got.Name = "renamed-node"
	got.Host = "gpu.example.com:22"
	if err := s.UpdateEnvironment(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, err := s.GetEnvironment(env.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got2.Name != "renamed-node" || got2.Host != "gpu.example.com:22" {
		t.Fatalf("update not persisted: %+v", got2)
	}

	// Delete removes only the addressed row.
	if err := s.DeleteEnvironment(env.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetEnvironment(env.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
	if _, err := s.GetEnvironment(foreign.ID); err != nil {
		t.Fatalf("delete removed another environment: %v", err)
	}
}

// TestUsernamesByID covers the one-query owner-name lookup the environment
// list and the dashboard columns are labelled with.
func TestUsernamesByID(t *testing.T) {
	s := newTestStore(t)
	alice := &User{Username: "alice", Email: "a@example.com", PasswordHash: "hash"}
	bob := &User{Username: "bob", Email: "b@example.com", PasswordHash: "hash"}
	for _, u := range []*User{alice, bob} {
		if err := s.CreateUser(u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}

	names, err := s.UsernamesByID([]int64{alice.ID, bob.ID, 9999})
	if err != nil {
		t.Fatalf("usernames: %v", err)
	}
	if names[alice.ID] != "alice" || names[bob.ID] != "bob" {
		t.Fatalf("unexpected names: %+v", names)
	}
	if _, ok := names[9999]; ok {
		t.Fatalf("unknown id must be absent, got %+v", names)
	}

	// No ids: no query, empty result.
	empty, err := s.UsernamesByID(nil)
	if err != nil {
		t.Fatalf("empty usernames: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected empty map, got %+v", empty)
	}
}

func TestEnvironmentBeforeSaveRequiresOwner(t *testing.T) {
	s := newTestStore(t)
	env := &TestEnvironment{Name: "orphan", Host: "h", Username: "u", PrivateKey: "k"}
	if err := s.CreateEnvironment(env); err == nil {
		t.Fatal("expected error creating environment without owner")
	}
}

func TestSetEnvironmentEnabled(t *testing.T) {
	s := newTestStore(t)
	u := &User{Username: "toggleowner", Email: "toggle@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// New environments default to enabled.
	env := &TestEnvironment{OwnerID: u.ID, Name: "node", Host: "h", Username: "u", PrivateKey: "k"}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !env.Enabled {
		t.Fatal("expected new environment to default to enabled")
	}

	// Disable.
	got, err := s.SetEnvironmentEnabled(env.ID, false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got.Enabled {
		t.Fatal("expected disabled after toggle")
	}
	// Persisted.
	got, err = s.GetEnvironment(env.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Enabled {
		t.Fatal("expected disabled state to persist")
	}

	// Re-enable.
	got, err = s.SetEnvironmentEnabled(env.ID, true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !got.Enabled {
		t.Fatal("expected enabled after re-toggle")
	}

	// An unknown id is a lookup failure, not a silent no-op.
	if _, err := s.SetEnvironmentEnabled(env.ID+999, false); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not found for unknown environment, got %v", err)
	}
}
