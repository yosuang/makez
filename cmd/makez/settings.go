package main

import (
	"fmt"
	"os"
	"strconv"
)

type settings struct {
	DatabaseConnections, ActivityConcurrency, WorkflowConcurrency int
	OperationsKey                                                 string
	Development                                                   bool
}

func loadSettings() (settings, error) {
	// One process gets 16 connections; Observe activities spend most of their
	// lifetime waiting on sandbox I/O. Start with 8 activity and 32 workflow slots.
	// Deployment budgets must sum these limits across all API and worker replicas.
	c := settings{OperationsKey: os.Getenv("MAKEZ_OPERATIONS_KEY"), Development: os.Getenv("MAKEZ_ENV") == "development"}
	var err error
	c.DatabaseConnections, err = boundedInt("MAKEZ_DATABASE_MAX_CONNS", 16, 2, 256)
	if err != nil {
		return c, err
	}
	c.ActivityConcurrency, err = boundedInt("MAKEZ_WORKER_ACTIVITIES", 8, 1, 1024)
	if err != nil {
		return c, err
	}
	c.WorkflowConcurrency, err = boundedInt("MAKEZ_WORKER_WORKFLOWS", 32, 1, 1024)
	if err != nil {
		return c, err
	}
	if c.OperationsKey != "" && len(c.OperationsKey) < 16 {
		return c, fmt.Errorf("MAKEZ_OPERATIONS_KEY must contain at least 16 characters")
	}
	return c, nil
}
func boundedInt(name string, fallback, minimum, maximum int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < minimum || n > maximum {
		return 0, fmt.Errorf("%s must be %d–%d", name, minimum, maximum)
	}
	return n, nil
}
