package main

import "testing"

// TestClassifyStartupMode locks in the flag → mode mapping that
// applyStartupGate dispatches on. Invariants:
//
//   - At most one primary mode flag may be set; multiple = Invalid.
//   - rollbackExternal wins over the others when only it is set.
//   - migrateRestart wins over migrate when only those two are set
//     (operator override semantics: --migrate-restart is the "force
//     redo" variant).
//   - migrate-only → Migrate.
//   - fresh-only → Fresh.
//   - No flags → Normal.
//
// The migrateRestart-and-nothing-else case explicitly does NOT also
// set migrate=true; we only document what the gate does when given
// a sane primary-mode bitset.
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
