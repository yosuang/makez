package telemetry

import (
	"testing"
	"time"
)

func TestMetricLabelsCannotContainTaskIdentity(t *testing.T) {
	m := New()
	m.Result("SUCCEEDED", time.Now().Add(-time.Second))
	m.HTTP.WithLabelValues("GET", "/v1/tasks/:id", "200").Observe(0.1)
	m.StreamEnds.WithLabelValues("terminal").Inc()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"result": true, "method": true, "route": true, "status": true, "reason": true, "state": true}
	for _, f := range families {
		for _, metric := range f.Metric {
			for _, label := range metric.Label {
				if !allowed[label.GetName()] {
					t.Fatalf("unbounded metric label: %s", label.GetName())
				}
			}
		}
	}
}
