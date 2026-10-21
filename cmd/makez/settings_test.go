package main

import "testing"

func TestSettingsBudgetsAndOperationsProtection(t *testing.T) {
	for _, name := range []string{"MAKEZ_DATABASE_MAX_CONNS", "MAKEZ_WORKER_ACTIVITIES", "MAKEZ_WORKER_WORKFLOWS", "MAKEZ_OPERATIONS_KEY", "MAKEZ_ENV"} {
		t.Setenv(name, "")
	}
	c, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseConnections != 16 || c.ActivityConcurrency != 8 || c.WorkflowConcurrency != 32 || c.Development || c.OperationsKey != "" {
		t.Fatalf("unsafe defaults: %+v", c)
	}
	for _, tc := range []struct{ name, value string }{{"MAKEZ_DATABASE_MAX_CONNS", "1"}, {"MAKEZ_DATABASE_MAX_CONNS", "257"}, {"MAKEZ_WORKER_ACTIVITIES", "0"}, {"MAKEZ_WORKER_WORKFLOWS", "huge"}, {"MAKEZ_OPERATIONS_KEY", "short"}} {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := loadSettings(); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
	t.Setenv("MAKEZ_ENV", "development")
	t.Setenv("MAKEZ_DATABASE_MAX_CONNS", "8")
	c, err = loadSettings()
	if err != nil || !c.Development || c.DatabaseConnections != 8 {
		t.Fatal("valid overrides rejected", err)
	}
}
