package store

import (
	"errors"
	"testing"
)

// TestErrTruncateRequiresConfirmation_SentinelExists is the PR4 RED-GREEN
// gate for the lock-in decision Q6 / REQ-011 / Risk #2: rewinding in
// truncate mode without an explicit ConfirmTruncate opt-in MUST be
// rejected with a dedicated sentinel error. The existence of the sentinel
// is the first half of the contract; per-REJECTION behavior is covered in
// rewind_test.go's truncate-mode tests.
func TestErrTruncateRequiresConfirmation_SentinelExists(t *testing.T) {
	if ErrTruncateRequiresConfirmation == nil {
		t.Fatal("ErrTruncateRequiresConfirmation sentinel is nil; expected a non-nil errors.New value")
	}
	if ErrTruncateRequiresConfirmation.Error() == "" {
		t.Fatal("ErrTruncateRequiresConfirmation has empty Error() string; expected a descriptive message")
	}
	// Sentinel MUST be unique vs. the other rewind sentinels so that
	// errors.Is can discriminate the opt-in rejection from a generic
	// "not implemented" / "invalid mode" error.
	others := []error{ErrInvalidRewindMode, ErrTargetTurnNotFound, ErrEmptySession, ErrCrossProjectFork, ErrProjectRequired}
	for _, other := range others {
		if errors.Is(ErrTruncateRequiresConfirmation, other) {
			t.Errorf("ErrTruncateRequiresConfirmation must be a distinct sentinel; it aliases %v", other)
		}
	}
}
