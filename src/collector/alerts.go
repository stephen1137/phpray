package main

import (
	"fmt"
	"time"
)

// AlertRule defines a threshold-based alert condition
type AlertRule struct {
	Name            string  `json:"name"`
	Type            string  `json:"type"`    // threshold, rate, anomaly
	Metric          string  `json:"metric"`  // avg_duration, error_rate, p95_duration, n1_rate, query_count
	Operator        string  `json:"operator"` // gt, lt
	Value           float64 `json:"value"`
	WindowMinutes   int     `json:"window_minutes"`
	DomainFilter    string  `json:"domain_filter,omitempty"` // empty = global
	CooldownMinutes int     `json:"cooldown_minutes"`
	Severity        string  `json:"severity"` // critical, warning, info
	Enabled         bool    `json:"enabled"`
}

// AlertEvent represents a fired alert
type AlertEvent struct {
	ID             int64   `json:"id"`
	RuleName       string  `json:"rule_name"`
	Severity       string  `json:"severity"`
	Metric         string  `json:"metric"`
	CurrentValue   float64 `json:"current_value"`
	ThresholdValue float64 `json:"threshold_value"`
	Domain         string  `json:"domain"`
	Message        string  `json:"message"`
	CreatedAt      int64   `json:"created_at"`
	Acknowledged   bool    `json:"acknowledged"`
	AckAt          *int64  `json:"ack_at,omitempty"`
}

// DefaultAlertRules returns the built-in always-active alert rules
func DefaultAlertRules() []AlertRule {
	return []AlertRule{
		{
			Name:            "p95_warning",
			Type:            "threshold",
			Metric:          "p95_duration",
			Operator:        "gt",
			Value:           3000,
			WindowMinutes:   5,
			CooldownMinutes: 15,
			Severity:        "warning",
			Enabled:         true,
		},
		{
			Name:            "p95_critical",
			Type:            "threshold",
			Metric:          "p95_duration",
			Operator:        "gt",
			Value:           10000,
			WindowMinutes:   5,
			CooldownMinutes: 15,
			Severity:        "critical",
			Enabled:         true,
		},
		{
			Name:            "error_rate_warning",
			Type:            "rate",
			Metric:          "error_rate",
			Operator:        "gt",
			Value:           5,
			WindowMinutes:   5,
			CooldownMinutes: 15,
			Severity:        "warning",
			Enabled:         true,
		},
		{
			Name:            "error_rate_critical",
			Type:            "rate",
			Metric:          "error_rate",
			Operator:        "gt",
			Value:           20,
			WindowMinutes:   5,
			CooldownMinutes: 15,
			Severity:        "critical",
			Enabled:         true,
		},
		{
			Name:            "n1_rate_warning",
			Type:            "rate",
			Metric:          "n1_rate",
			Operator:        "gt",
			Value:           30,
			WindowMinutes:   5,
			CooldownMinutes: 30,
			Severity:        "warning",
			Enabled:         true,
		},
		{
			Name:            "avg_duration_warning",
			Type:            "threshold",
			Metric:          "avg_duration",
			Operator:        "gt",
			Value:           5000,
			WindowMinutes:   5,
			CooldownMinutes: 15,
			Severity:        "warning",
			Enabled:         true,
		},
	}
}

// alertMetrics holds computed metrics for alert evaluation
type alertMetrics struct {
	avgDuration float64
	p95Duration float64
	errorRate   float64
	n1Rate      float64
	queryCount  int64
	totalReqs   int64
	domain      string
}

// EvaluateAlerts checks all rules against recent data and returns new alert events
func EvaluateAlerts(store *Storage, rules []AlertRule) []AlertEvent {
	var events []AlertEvent

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}

		// Get metrics per domain (plus global)
		metricsList := computeAlertMetrics(store, rule.WindowMinutes, rule.DomainFilter)

		for _, m := range metricsList {
			if m.totalReqs < 5 {
				// Skip evaluation for very low traffic to avoid false positives
				continue
			}

			var currentValue float64
			switch rule.Metric {
			case "avg_duration":
				currentValue = m.avgDuration
			case "p95_duration":
				currentValue = m.p95Duration
			case "error_rate":
				currentValue = m.errorRate
			case "n1_rate":
				currentValue = m.n1Rate
			case "query_count":
				currentValue = float64(m.queryCount)
			default:
				continue
			}

			triggered := false
			switch rule.Operator {
			case "gt":
				triggered = currentValue > rule.Value
			case "lt":
				triggered = currentValue < rule.Value
			}

			if !triggered {
				continue
			}

			// Check cooldown: skip if same rule+domain fired recently
			lastFired, err := store.GetLastAlertTime(rule.Name, m.domain)
			if err == nil && lastFired > 0 {
				cooldownSec := int64(rule.CooldownMinutes) * 60
				if time.Now().Unix()-lastFired < cooldownSec {
					continue
				}
			}

			events = append(events, AlertEvent{
				RuleName:       rule.Name,
				Severity:       rule.Severity,
				Metric:         rule.Metric,
				CurrentValue:   currentValue,
				ThresholdValue: rule.Value,
				Domain:         m.domain,
				Message:        formatAlertMessage(rule, currentValue, m.domain),
				CreatedAt:      time.Now().Unix(),
			})
		}
	}

	return events
}

func computeAlertMetrics(store *Storage, windowMin int, domainFilter string) []alertMetrics {
	cutoff := time.Now().Unix() - int64(windowMin)*60

	query := `SELECT
		COALESCE(host, '') as domain,
		COUNT(*) as total_reqs,
		COALESCE(AVG(duration_ms), 0) as avg_dur,
		SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END) as err_count,
		SUM(CASE WHEN n_plus_one = 1 THEN 1 ELSE 0 END) as n1_count,
		SUM(db_count) as total_queries
		FROM traces WHERE timestamp >= ?`
	args := []interface{}{cutoff}

	if domainFilter != "" {
		query += " AND host LIKE ?"
		args = append(args, "%"+domainFilter+"%")
	}

	query += " GROUP BY host"

	rows, err := store.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var results []alertMetrics
	for rows.Next() {
		var m alertMetrics
		var errCount, n1Count int64
		if err := rows.Scan(&m.domain, &m.totalReqs, &m.avgDuration, &errCount, &n1Count, &m.queryCount); err != nil {
			continue
		}
		if m.totalReqs > 0 {
			m.errorRate = float64(errCount) / float64(m.totalReqs) * 100
			m.n1Rate = float64(n1Count) / float64(m.totalReqs) * 100
		}
		results = append(results, m)
	}

	// Compute P95 per domain using OFFSET (avoids loading all durations into memory)
	for i, m := range results {
		if m.totalReqs == 0 {
			continue
		}
		p95Offset := int(float64(m.totalReqs) * 0.95)
		if p95Offset >= int(m.totalReqs) {
			p95Offset = int(m.totalReqs) - 1
		}
		var p95 float64
		err := store.db.QueryRow(`SELECT duration_ms FROM traces
			WHERE timestamp >= ? AND host = ?
			ORDER BY duration_ms ASC LIMIT 1 OFFSET ?`,
			cutoff, m.domain, p95Offset).Scan(&p95)
		if err == nil {
			results[i].p95Duration = p95
		}
	}

	return results
}

func formatAlertMessage(rule AlertRule, currentValue float64, domain string) string {
	domainStr := "globally"
	if domain != "" {
		domainStr = fmt.Sprintf("on %s", domain)
	}

	var metricLabel string
	var valueStr string
	switch rule.Metric {
	case "avg_duration":
		metricLabel = "Average response time"
		valueStr = fmt.Sprintf("%.0fms (threshold: %.0fms)", currentValue, rule.Value)
	case "p95_duration":
		metricLabel = "P95 response time"
		valueStr = fmt.Sprintf("%.0fms (threshold: %.0fms)", currentValue, rule.Value)
	case "error_rate":
		metricLabel = "Error rate"
		valueStr = fmt.Sprintf("%.1f%% (threshold: %.0f%%)", currentValue, rule.Value)
	case "n1_rate":
		metricLabel = "N+1 query rate"
		valueStr = fmt.Sprintf("%.1f%% (threshold: %.0f%%)", currentValue, rule.Value)
	case "query_count":
		metricLabel = "Query count"
		valueStr = fmt.Sprintf("%.0f (threshold: %.0f)", currentValue, rule.Value)
	default:
		metricLabel = rule.Metric
		valueStr = fmt.Sprintf("%.2f (threshold: %.2f)", currentValue, rule.Value)
	}

	return fmt.Sprintf("%s is %s %s", metricLabel, valueStr, domainStr)
}
