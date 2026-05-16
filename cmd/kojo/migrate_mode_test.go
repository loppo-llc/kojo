package main

import "testing"

// TestClassifyStartupMode locks in the flag → mode mapping that
// applyStartupGate dispatches on. Invariants:
//
//   - At most one primary mode flag may be set; multiple = Invalid
//     (this includes the migrate + migrateRestart combo — they are
//     siblings, NOT a layered "restart wins" relationship).
//   - rollbackExternal-only → RollbackExternal.
//   - migrate-only → Migrate.
//   - migrateRestart-only → MigrateRestart.
//   - fresh-only → Fresh.
//   - No primary flags → Normal.
//
// Each case sets exactly one primary flag; the Invalid cases pin
// the multi-set behavior.
func TestClassifyStartupMode(t *testing.T) {
	tests := []struct {
		name  string
		flags migrationFlags
		want  startupMode
	}{
		{name: "no flags", flags: migrationFlags{}, want: startupModeNormal},
		{name: "migrate only", flags: migrationFlags{migrate: true}, want: startupModeMigrate},
		{name: "migrateRestart only", flags: migrationFlags{migrateRestart: true}, want: startupModeMigrateRestart},
		{name: "fresh only", flags: migrationFlags{fresh: true}, want: startupModeFresh},
		{name: "rollbackExternal only", flags: migrationFlags{rollbackExternal: true}, want: startupModeRollbackExternal},
		{
			name:  "migrate + fresh = invalid",
			flags: migrationFlags{migrate: true, fresh: true},
			want:  startupModeInvalid,
		},
		{
			name:  "migrate + rollbackExternal = invalid",
			flags: migrationFlags{migrate: true, rollbackExternal: true},
			want:  startupModeInvalid,
		},
		{
			name:  "migrateRestart + fresh = invalid",
			flags: migrationFlags{migrateRestart: true, fresh: true},
			want:  startupModeInvalid,
		},
		{
			// Codex-flagged case: migrate + migrateRestart are siblings
			// in primaryModeCount, NOT a layered relationship where
			// migrateRestart overrides migrate. Both true counts as 2
			// primary flags → Invalid.
			name:  "migrate + migrateRestart = invalid",
			flags: migrationFlags{migrate: true, migrateRestart: true},
			want:  startupModeInvalid,
		},
		{
			name:  "all four = invalid",
			flags: migrationFlags{migrate: true, migrateRestart: true, fresh: true, rollbackExternal: true},
			want:  startupModeInvalid,
		},
		{
			// migrateExternalCLI / migrateBackup / migrateForceRecentMtime are
			// NOT primary modes — they're modifiers on top of --migrate.
			// classifyStartupMode must ignore them when deciding the mode.
			name:  "non-primary modifiers do not change classification",
			flags: migrationFlags{migrateExternalCLI: true, migrateBackup: "/tmp/x", migrateForceRecentMtime: true},
			want:  startupModeNormal,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStartupMode(tc.flags)
			if got != tc.want {
				t.Errorf("classifyStartupMode = %d, want %d", got, tc.want)
			}
		})
	}
}
