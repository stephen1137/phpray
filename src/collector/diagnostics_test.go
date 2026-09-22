package main

import (
	"testing"
)

func TestDiagEngineHealthy(t *testing.T) {
	ctx := &DiagContext{
		Domain:    "healthy.example.com",
		TotalReqs: 100,
		Traces: func() []TraceRecord {
			var traces []TraceRecord
			for i := 0; i < 100; i++ {
				traces = append(traces, TraceRecord{
					ID:         int64(i),
					URI:        "/index.php",
					Status:     200,
					DurationMs: 50.0,
					DBCount:    3,
					DBMs:       5.0,
					MemoryMB:   10.0,
				})
			}
			return traces
		}(),
		AvgDuration: 50.0,
		MaxDuration: 100.0,
	}

	engine := NewDiagEngine()
	report := engine.Analyze(ctx)

	if report.HealthScore < 90 {
		t.Errorf("Expected high health score for healthy traces, got %d", report.HealthScore)
	}
	if len(report.Findings) > 0 {
		t.Errorf("Expected no findings for healthy traces, got %d: %v", len(report.Findings), report.Findings)
	}
}

func TestDiagEngineN1Detection(t *testing.T) {
	ctx := &DiagContext{
		Domain:    "shop.example.com",
		TotalReqs: 20,
		N1Count:   15,
		Traces: func() []TraceRecord {
			var traces []TraceRecord
			for i := 0; i < 20; i++ {
				n1 := 0
				if i < 15 {
					n1 = 1
				}
				traces = append(traces, TraceRecord{
					ID:         int64(i),
					URI:        "/products",
					Status:     200,
					DurationMs: 500.0,
					DBCount:    30,
					DBMs:       200.0,
					N1:         n1,
					MemoryMB:   20.0,
				})
			}
			return traces
		}(),
		AvgDuration: 500.0,
		MaxDuration: 800.0,
	}

	engine := NewDiagEngine()
	report := engine.Analyze(ctx)

	found := false
	for _, f := range report.Findings {
		if f.Rule == "n1_queries" {
			found = true
			if f.Severity != SevCritical {
				t.Errorf("Expected critical severity for 75%% N+1 rate, got %s", f.Severity)
			}
		}
	}
	if !found {
		t.Error("Expected N+1 finding but none found")
	}

	if report.HealthScore >= 100 {
		t.Errorf("Expected reduced health score, got %d", report.HealthScore)
	}
}

func TestDiagEngineErrorRate(t *testing.T) {
	ctx := &DiagContext{
		Domain:     "broken.example.com",
		TotalReqs:  50,
		ErrorCount: 25,
		Traces: func() []TraceRecord {
			var traces []TraceRecord
			for i := 0; i < 50; i++ {
				status := 200
				if i < 25 {
					status = 500
				}
				traces = append(traces, TraceRecord{
					ID:         int64(i),
					URI:        "/checkout",
					Status:     status,
					DurationMs: 100.0,
					MemoryMB:   10.0,
				})
			}
			return traces
		}(),
		AvgDuration: 100.0,
		MaxDuration: 200.0,
	}

	engine := NewDiagEngine()
	report := engine.Analyze(ctx)

	found := false
	for _, f := range report.Findings {
		if f.Rule == "high_error_rate" {
			found = true
			if f.Severity != SevCritical {
				t.Errorf("Expected critical severity for 50%% error rate, got %s", f.Severity)
			}
		}
	}
	if !found {
		t.Error("Expected error rate finding but none found")
	}
}

func TestDiagEngineHighMemory(t *testing.T) {
	ctx := &DiagContext{
		Domain:    "heavy.example.com",
		TotalReqs: 10,
		Traces: func() []TraceRecord {
			var traces []TraceRecord
			for i := 0; i < 10; i++ {
				traces = append(traces, TraceRecord{
					ID:         int64(i),
					URI:        "/heavy-page",
					Status:     200,
					DurationMs: 200.0,
					MemoryMB:   96.0, // 96MB — over 64MB threshold
				})
			}
			return traces
		}(),
		AvgDuration: 200.0,
		MaxDuration: 300.0,
	}

	engine := NewDiagEngine()
	report := engine.Analyze(ctx)

	found := false
	for _, f := range report.Findings {
		if f.Rule == "high_memory" {
			found = true
		}
	}
	if !found {
		t.Error("Expected high memory finding but none found")
	}
}

func TestDiagEngineSlowResponses(t *testing.T) {
	ctx := &DiagContext{
		Domain:      "slow.example.com",
		TotalReqs:   20,
		AvgDuration: 4000.0,
		MaxDuration: 8000.0,
		P95Duration: 7000.0,
		Traces: func() []TraceRecord {
			var traces []TraceRecord
			for i := 0; i < 20; i++ {
				traces = append(traces, TraceRecord{
					ID:         int64(i),
					URI:        "/slow-page",
					Status:     200,
					DurationMs: 4000.0,
					DBMs:       100.0,
					HTTPMs:     100.0,
					MemoryMB:   10.0,
				})
			}
			return traces
		}(),
	}

	engine := NewDiagEngine()
	report := engine.Analyze(ctx)

	found := false
	for _, f := range report.Findings {
		if f.Rule == "slow_responses" {
			found = true
		}
	}
	if !found {
		t.Error("Expected slow responses finding but none found")
	}
}

func TestDiagEngineSummary(t *testing.T) {
	ctx := &DiagContext{
		Domain:     "test.example.com",
		TotalReqs:  10,
		ErrorCount: 5,
		N1Count:    3,
		Traces: func() []TraceRecord {
			var traces []TraceRecord
			for i := 0; i < 10; i++ {
				traces = append(traces, TraceRecord{
					ID:         int64(i),
					URI:        "/test",
					Status:     200,
					DurationMs: 50.0,
					N1:         0,
					MemoryMB:   10.0,
				})
			}
			// Set status for error traces
			for i := 0; i < 5; i++ {
				traces[i].Status = 500
			}
			for i := 5; i < 8; i++ {
				traces[i].N1 = 1
			}
			return traces
		}(),
		AvgDuration: 50.0,
		MaxDuration: 100.0,
	}

	engine := NewDiagEngine()
	report := engine.Analyze(ctx)

	if report.Summary == "" {
		t.Error("Expected non-empty summary")
	}
	if report.Domain != "test.example.com" {
		t.Errorf("Expected domain test.example.com, got %s", report.Domain)
	}
}

func TestExtractHost(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://api.stripe.com/v1/charges", "api.stripe.com"},
		{"http://example.com/path?q=1", "example.com"},
		{"https://cdn.example.com", "cdn.example.com"},
	}

	for _, tt := range tests {
		got := extractHost(tt.input)
		if got != tt.expected {
			t.Errorf("extractHost(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
