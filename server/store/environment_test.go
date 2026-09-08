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

	// List (owner-scoped).
	envs, err := s.ListEnvironments(u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(envs) != 1 || envs[0].Name != "cpu-node-1" {
		t.Fatalf("unexpected list: %+v", envs)
	}

	// Another user sees nothing.
	other := &User{Username: "other", Email: "other@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(other); err != nil {
		t.Fatalf("create other user: %v", err)
	}
	envs, err = s.ListEnvironments(other.ID)
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(envs) != 0 {
		t.Fatalf("expected empty list for other owner, got %d", len(envs))
	}

	// Get (own + foreign).
	got, err := s.GetEnvironment(u.ID, env.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Host != "192.168.1.10" || got.Username != "runner" {
		t.Fatalf("unexpected env: %+v", got)
	}
	if _, err := s.GetEnvironment(other.ID, env.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not found for foreign owner, got %v", err)
	}

	// Update.
	got.Name = "gpu-a100"
	got.Host = "gpu.example.com:22"
	if err := s.UpdateEnvironment(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, err := s.GetEnvironment(u.ID, env.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got2.Name != "gpu-a100" || got2.Host != "gpu.example.com:22" {
		t.Fatalf("update not persisted: %+v", got2)
	}

	// Delete.
	if err := s.DeleteEnvironment(u.ID, env.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetEnvironment(u.ID, env.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
	// Foreign delete is a no-op that reports no error, but record remains for owner.
}

func TestEnvironmentBeforeSaveRequiresOwner(t *testing.T) {
	s := newTestStore(t)
	env := &TestEnvironment{Name: "orphan", Host: "h", Username: "u", PrivateKey: "k"}
	if err := s.CreateEnvironment(env); err == nil {
		t.Fatal("expected error creating environment without owner")
	}
}
