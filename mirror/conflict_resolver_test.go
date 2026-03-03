package mirror

import "testing"

func TestResolveConflict_AINewerUSN(t *testing.T) {
	res := ResolveConflict(ConflictContext{
		AIStartUSN: 5,
		AIEndUSN:   7,
		DBUSN:      6,
	})
	if res != ResolutionApplyAI {
		t.Errorf("got %q, want %q", res, ResolutionApplyAI)
	}
}

func TestResolveConflict_AIEqualUSN(t *testing.T) {
	res := ResolveConflict(ConflictContext{
		AIStartUSN: 5,
		AIEndUSN:   6,
		DBUSN:      6,
	})
	if res != ResolutionApplyAI {
		t.Errorf("got %q, want %q", res, ResolutionApplyAI)
	}
}

func TestResolveConflict_UserNewerUSN(t *testing.T) {
	res := ResolveConflict(ConflictContext{
		AIStartUSN: 5,
		AIEndUSN:   5,
		DBUSN:      8,
	})
	if res != ResolutionKeepUser {
		t.Errorf("got %q, want %q", res, ResolutionKeepUser)
	}
}

func TestResolveConflict_SameUSNAINewerTimestamp(t *testing.T) {
	res := ResolveConflict(ConflictContext{
		AIStartUSN:  5,
		AIEndUSN:    5,
		DBUSN:       5,
		AITimestamp:  1709000002000,
		DBTimestamp:  1709000001000,
	})
	if res != ResolutionApplyAI {
		t.Errorf("got %q, want %q", res, ResolutionApplyAI)
	}
}

func TestResolveConflict_SameUSNUserNewerTimestamp(t *testing.T) {
	res := ResolveConflict(ConflictContext{
		AIStartUSN:  5,
		AIEndUSN:    5,
		DBUSN:       5,
		AITimestamp:  1709000001000,
		DBTimestamp:  1709000002000,
	})
	if res != ResolutionKeepUser {
		t.Errorf("got %q, want %q", res, ResolutionKeepUser)
	}
}

func TestResolveConflict_SameEverything(t *testing.T) {
	res := ResolveConflict(ConflictContext{
		AIStartUSN:  5,
		AIEndUSN:    5,
		DBUSN:       5,
		AITimestamp:  1709000001000,
		DBTimestamp:  1709000001000,
	})
	if res != ResolutionSkip {
		t.Errorf("got %q, want %q", res, ResolutionSkip)
	}
}

func TestShouldApplyImport_CreateAlwaysApply(t *testing.T) {
	res := ShouldApplyImport(ImportEntry{Action: ImportActionCreate}, 10, 5)
	if res != ResolutionApplyAI {
		t.Errorf("create should always apply, got %q", res)
	}
}

func TestShouldApplyImport_DeleteNoUserChange(t *testing.T) {
	res := ShouldApplyImport(ImportEntry{Action: ImportActionDelete}, 5, 5)
	if res != ResolutionApplyAI {
		t.Errorf("delete with no user change should apply, got %q", res)
	}
}

func TestShouldApplyImport_DeleteWithUserChange(t *testing.T) {
	res := ShouldApplyImport(ImportEntry{Action: ImportActionDelete}, 8, 5)
	if res != ResolutionKeepUser {
		t.Errorf("delete with user change should keep user, got %q", res)
	}
}

func TestShouldApplyImport_UpdateWithNoteUSN(t *testing.T) {
	res := ShouldApplyImport(ImportEntry{
		Action:     ImportActionUpdate,
		NoteMeta:   &NoteMeta{USN: 7},
	}, 6, 5)
	if res != ResolutionApplyAI {
		t.Errorf("update with newer note USN should apply, got %q", res)
	}
}

func TestShouldApplyImport_MoveWithOlderUSN(t *testing.T) {
	res := ShouldApplyImport(ImportEntry{
		Action:     ImportActionMove,
		NoteMeta:   &NoteMeta{USN: 5},
	}, 8, 5)
	if res != ResolutionKeepUser {
		t.Errorf("move with older USN should keep user, got %q", res)
	}
}
