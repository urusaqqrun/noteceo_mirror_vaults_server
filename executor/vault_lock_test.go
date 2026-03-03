package executor

import "testing"

func TestVaultLock_LockUnlock(t *testing.T) {
	l := NewVaultLock()

	if !l.Lock("user1", "task-1") {
		t.Error("first lock should succeed")
	}
	if l.Lock("user1", "task-2") {
		t.Error("second lock by different task should fail")
	}
	if !l.IsLocked("user1") {
		t.Error("user1 should be locked")
	}
	if l.GetLockingTask("user1") != "task-1" {
		t.Errorf("locking task: got %q, want %q", l.GetLockingTask("user1"), "task-1")
	}

	l.Unlock("user1", "task-1")
	if l.IsLocked("user1") {
		t.Error("user1 should be unlocked after unlock")
	}
}

func TestVaultLock_SameTaskRelock(t *testing.T) {
	l := NewVaultLock()
	l.Lock("user1", "task-1")
	if !l.Lock("user1", "task-1") {
		t.Error("same task should be able to relock")
	}
}

func TestVaultLock_UnlockWrongTask(t *testing.T) {
	l := NewVaultLock()
	l.Lock("user1", "task-1")
	l.Unlock("user1", "task-wrong")

	if !l.IsLocked("user1") {
		t.Error("wrong task unlock should not release the lock")
	}
}

func TestVaultLock_MultiUser(t *testing.T) {
	l := NewVaultLock()
	l.Lock("user1", "task-1")
	l.Lock("user2", "task-2")

	if !l.IsLocked("user1") || !l.IsLocked("user2") {
		t.Error("both users should be locked")
	}
	if l.IsLocked("user3") {
		t.Error("user3 should not be locked")
	}
}
