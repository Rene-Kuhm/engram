package main

import (
	"strings"
	"testing"
)

// TestSessionUsage_HelpText_SafetyContract is a snapshot-style test that
// asserts `engram session --help` includes the safety contract for
// truncate + the recover pointer. The user has to see the warning
// before they type `engram session rewind --mode truncate`; if the
// help text drops the line, a future contributor can ship a silent
// destructive CLI change without anyone noticing.
func TestSessionUsage_HelpText_SafetyContract(t *testing.T) {
	stubExitWithPanic(t)
	stdout, stderr, _ := captureOutputAndRecover(t, func() { printSessionUsage() })
	combined := stdout + stderr

	mustContain := []string{
		"session recover", // the recovery pointer
		"--confirm",       // the opt-in flag spelling
		"truncate",        // explicit mention of the destructive mode
		"destructive",     // the safety-contract wording
		"recover",         // pointer to recover as the recovery path
	}
	for _, want := range mustContain {
		if !strings.Contains(strings.ToLower(combined), want) {
			t.Errorf("printSessionUsage missing %q; got:\n%s", want, combined)
		}
	}
}

// TestSessionUsage_HelpText_ListsRecoverSubcommand covers the
// dispatcher contract: `engram session <unknown>` must list 'recover'
// in the subcommand inventory so users can discover it from --help.
func TestSessionUsage_HelpText_ListsRecoverSubcommand(t *testing.T) {
	stubExitWithPanic(t)
	stdout, stderr, _ := captureOutputAndRecover(t, func() { printSessionUsage() })
	combined := strings.ToLower(stdout + stderr)
	if !strings.Contains(combined, "recover") {
		t.Errorf("printSessionUsage missing 'recover' in subcommand inventory; got:\n%s", combined)
	}
}

// TestSessionUsage_HelpText_NoStaleReserveNote covers that the
// PR2-era "truncate is reserved for PR4" note is GONE now that PR4
// has shipped. A stale note would mislead users into thinking
// truncate is still unsupported.
func TestSessionUsage_HelpText_NoStaleReserveNote(t *testing.T) {
	stubExitWithPanic(t)
	stdout, stderr, _ := captureOutputAndRecover(t, func() { printSessionUsage() })
	combined := strings.ToLower(stdout + stderr)
	if strings.Contains(combined, "reserved for pr4") {
		t.Errorf("printSessionUsage still contains stale 'reserved for PR4' note; got:\n%s", combined)
	}
}
