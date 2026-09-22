package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ─── Diagnostic Rule Engine ───
// Analyzes traces and produces actionable insights per domain.

// Severity levels for diagnostic findings
type Severity string

const (
	SevCritical Severity = "critical" // Immediate action needed
	SevWarning  Severity = "warning"  // Should investigate
	SevInfo     Severity = "info"     // Optimization opportunity
)

// DiagFinding represents a single diagnostic finding
type DiagFinding struct {
	Rule        string   `json:"rule"`
	Severity    Severity `json:"severity"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Impact      string   `json:"impact"`
	Fix         string   `json:"fix"`
	Evidence    []string `json:"evidence,omitempty"`
	Score       float64  `json:"score"` // 0-100, higher = worse
}

// DiagReport is the full diagnostic report for a domain
type DiagReport struct {
	Domain      string        `json:"domain"`
	WindowMin   int           `json:"window_minutes"`
	TraceCount  int           `json:"trace_count"`
	Findings    []DiagFinding `json:"findings"`
	HealthScore int           `json:"health_score"` // 0-100, higher = better
	Summary     string        `json:"summary"`
}

// DiagRule is the interface for diagnostic rules
type DiagRule interface {
	Name() string
	Evaluate(ctx *DiagContext) []DiagFinding
}

// DiagContext holds all data needed for diagnosis
type DiagContext struct {
	Domain string
	Traces []TraceRecord

	// Pre-computed stats
	AvgDuration float64
	MaxDuration float64
	P95Duration float64
	ErrorCount  int
	N1Count     int
	TotalDB     int
	TotalHTTP   int
	TotalReqs   int
}

// TraceRecord is a simplified trace for diagnostics
type TraceRecord struct {
	ID         int64
	PhprayID   string
	Host       string
	URI        string
	Method     string
	Status     int
	DurationMs float64
	DBCount    int
	DBMs       float64
	HTTPCount  int
	HTTPMs     float64
	FileCount  int
	FileMs     float64
	RedisCount int
	RedisMs    float64
	MemoryMB   float64
	WP         int
	N1         int
	Level      string
	ErrorCount int
	PhpVer     string
	Queries    []QueryRecord
	HTTPCalls  []HTTPRecord
	Errors     []ErrorRecord
}

type QueryRecord struct {
	SQL         string
	Fingerprint string
	DurationMs  float64
	OffsetMs    float64
}

type HTTPRecord struct {
	URL        string
	DurationMs float64
	Status     int
}

type ErrorRecord struct {
	Type    string
	Message string
}

// ─── Diagnostic Engine ───

type DiagEngine struct {
	rules []DiagRule
}

func NewDiagEngine() *DiagEngine {
	return &DiagEngine{
		rules: []DiagRule{
			&RuleN1Queries{},
			&RuleSlowQueries{},
			&RuleExternalAPICalls{},
			&RuleHighMemory{},
			&RuleHighErrorRate{},
			&RuleSlowResponses{},
			&RuleWPAutoload{},
			&RuleMissingOpcache{},
			&RulePhpVersion{},
		},
	}
}

func (e *DiagEngine) Analyze(ctx *DiagContext) *DiagReport {
	report := &DiagReport{
		Domain:     ctx.Domain,
		TraceCount: ctx.TotalReqs,
	}

	for _, rule := range e.rules {
		findings := rule.Evaluate(ctx)
		report.Findings = append(report.Findings, findings...)
	}

	// Sort findings by score (worst first)
	sort.Slice(report.Findings, func(i, j int) bool {
		return report.Findings[i].Score > report.Findings[j].Score
	})

	// Calculate health score (100 = perfect, 0 = terrible)
	// Use diminishing returns: each finding's penalty is reduced by how many we already have
	totalPenalty := 0.0
	for i, f := range report.Findings {
		// Diminishing penalty: first finding at full weight, subsequent at 70%, 50%, etc.
		weight := 1.0
		if i > 0 {
			weight = 1.0 / (1.0 + float64(i)*0.5)
		}
		totalPenalty += f.Score * weight
	}
	report.HealthScore = max(0, min(100, 100-int(totalPenalty)))

	// Generate summary
	report.Summary = e.generateSummary(report)

	return report
}

func (e *DiagEngine) generateSummary(report *DiagReport) string {
	if len(report.Findings) == 0 {
		return fmt.Sprintf("%s looks healthy! No issues detected in %d requests.",
			report.Domain, report.TraceCount)
	}

	critCount := 0
	warnCount := 0
	for _, f := range report.Findings {
		switch f.Severity {
		case SevCritical:
			critCount++
		case SevWarning:
			warnCount++
		}
	}

	var parts []string
	if critCount > 0 {
		parts = append(parts, fmt.Sprintf("%d critical issue(s)", critCount))
	}
	if warnCount > 0 {
		parts = append(parts, fmt.Sprintf("%d warning(s)", warnCount))
	}
	infoCount := len(report.Findings) - critCount - warnCount
	if infoCount > 0 {
		parts = append(parts, fmt.Sprintf("%d optimization(s)", infoCount))
	}

	domain := report.Domain
	if domain == "" {
		domain = "All domains"
	}
	return fmt.Sprintf("%s: %s found in %d requests. Health score: %d/100.",
		domain, strings.Join(parts, ", "), report.TraceCount, report.HealthScore)
}

// ─── Rule Implementations ───

// Rule: N+1 Query Detection
type RuleN1Queries struct{}

func (r *RuleN1Queries) Name() string { return "n1_queries" }

func (r *RuleN1Queries) Evaluate(ctx *DiagContext) []DiagFinding {
	if ctx.N1Count == 0 {
		return nil
	}

	pct := float64(ctx.N1Count) / float64(ctx.TotalReqs) * 100
	var sev Severity
	var score float64

	switch {
	case pct > 30:
		sev = SevCritical
		score = 35
	case pct > 10:
		sev = SevWarning
		score = 20
	default:
		sev = SevInfo
		score = 10
	}

	// Find worst N+1 by HOST + URI
	type hostURI struct{ host, uri string }
	n1Map := make(map[hostURI]int)
	for _, t := range ctx.Traces {
		if t.N1 == 1 {
			n1Map[hostURI{t.Host, t.URI}]++
		}
	}
	var evidence []string
	type kv struct {
		k string
		v int
	}
	var sorted []kv
	for hu, v := range n1Map {
		sorted = append(sorted, kv{fmt.Sprintf("%s%s", hu.host, hu.uri), v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })
	for i, s := range sorted {
		if i >= 5 {
			break
		}
		evidence = append(evidence, fmt.Sprintf("%s: %d N+1 requests", s.k, s.v))
	}

	return []DiagFinding{{
		Rule:     r.Name(),
		Severity: sev,
		Title:    "N+1 Query Pattern Detected",
		Description: fmt.Sprintf(
			"%d out of %d requests (%.1f%%) have N+1 query patterns. "+
				"This means the same query is executed many times with different IDs, "+
				"typically in a loop instead of a single batch query.",
			ctx.N1Count, ctx.TotalReqs, pct),
		Impact: "Each N+1 pattern adds 5-50ms of unnecessary database round-trips. " +
			"Fixing the top offenders could reduce response times by 30-60%.",
		Fix: "Replace individual queries in loops with batch queries using IN() clauses. " +
			"For WordPress, consider using wp_cache_get_multiple() or pre-fetching with get_posts().",
		Evidence: evidence,
		Score:    score,
	}}
}

// Rule: Slow Database Queries
type RuleSlowQueries struct{}

func (r *RuleSlowQueries) Name() string { return "slow_queries" }

func (r *RuleSlowQueries) Evaluate(ctx *DiagContext) []DiagFinding {
	// Analyze queries across all traces
	type qStats struct {
		fingerprint string
		sample      string
		totalMs     float64
		maxMs       float64
		count       int
	}
	qMap := make(map[string]*qStats)

	for _, t := range ctx.Traces {
		for _, q := range t.Queries {
			fp := q.Fingerprint
			if fp == "" {
				fp = q.SQL
			}
			if s, ok := qMap[fp]; ok {
				s.totalMs += q.DurationMs
				s.count++
				if q.DurationMs > s.maxMs {
					s.maxMs = q.DurationMs
				}
			} else {
				qMap[fp] = &qStats{
					fingerprint: fp,
					sample:      q.SQL,
					totalMs:     q.DurationMs,
					maxMs:       q.DurationMs,
					count:       1,
				}
			}
		}
	}

	// Find queries > 100ms average
	var slowQ []qStats
	for _, s := range qMap {
		avg := s.totalMs / float64(s.count)
		if avg > 100 || s.maxMs > 500 {
			slowQ = append(slowQ, *s)
		}
	}

	if len(slowQ) == 0 {
		return nil
	}

	sort.Slice(slowQ, func(i, j int) bool {
		return slowQ[i].totalMs/float64(slowQ[i].count) > slowQ[j].totalMs/float64(slowQ[j].count)
	})

	var evidence []string
	for i, q := range slowQ {
		if i >= 5 {
			break
		}
		avg := q.totalMs / float64(q.count)
		sqlPreview := q.sample
		if len(sqlPreview) > 80 {
			sqlPreview = sqlPreview[:77] + "..."
		}
		evidence = append(evidence, fmt.Sprintf("%.0fms avg, %.0fms max, %d× — %s", avg, q.maxMs, q.count, sqlPreview))
	}

	sev := SevWarning
	score := 15.0
	if slowQ[0].maxMs > 1000 {
		sev = SevCritical
		score = 25
	}

	return []DiagFinding{{
		Rule:     r.Name(),
		Severity: sev,
		Title:    fmt.Sprintf("%d Slow Query Pattern(s) Detected", len(slowQ)),
		Description: fmt.Sprintf(
			"Found %d query fingerprints with average execution time >100ms or max >500ms.",
			len(slowQ)),
		Impact:   "Slow queries are often the #1 cause of slow page loads. Adding proper indexes can reduce query time by 10-100×.",
		Fix:      "Run EXPLAIN on the slowest queries. Check for missing indexes, full table scans, or unnecessary joins. Consider query caching for repeated slow queries.",
		Evidence: evidence,
		Score:    score,
	}}
}

// Rule: External API Calls
type RuleExternalAPICalls struct{}

func (r *RuleExternalAPICalls) Name() string { return "slow_external_api" }

func (r *RuleExternalAPICalls) Evaluate(ctx *DiagContext) []DiagFinding {
	// Find traces where HTTP calls dominate request time
	type httpStats struct {
		url     string
		totalMs float64
		maxMs   float64
		count   int
	}
	urlMap := make(map[string]*httpStats)
	slowHTTPTraces := 0

	for _, t := range ctx.Traces {
		if t.HTTPMs > 0 && t.DurationMs > 0 {
			if t.HTTPMs/t.DurationMs > 0.5 && t.HTTPMs > 200 {
				slowHTTPTraces++
			}
		}
		for _, h := range t.HTTPCalls {
			host := extractHost(h.URL)
			if s, ok := urlMap[host]; ok {
				s.totalMs += h.DurationMs
				s.count++
				if h.DurationMs > s.maxMs {
					s.maxMs = h.DurationMs
				}
			} else {
				urlMap[host] = &httpStats{
					url:     host,
					totalMs: h.DurationMs,
					maxMs:   h.DurationMs,
					count:   1,
				}
			}
		}
	}

	if slowHTTPTraces == 0 && len(urlMap) == 0 {
		return nil
	}

	var findings []DiagFinding

	if slowHTTPTraces > 0 {
		pct := float64(slowHTTPTraces) / float64(ctx.TotalReqs) * 100
		var sev Severity
		var score float64
		switch {
		case pct > 20:
			sev = SevCritical
			score = 25
		case pct > 5:
			sev = SevWarning
			score = 15
		default:
			sev = SevInfo
			score = 8
		}

		var evidence []string
		var sorted []httpStats
		for _, s := range urlMap {
			sorted = append(sorted, *s)
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].totalMs > sorted[j].totalMs })
		for i, s := range sorted {
			if i >= 3 {
				break
			}
			evidence = append(evidence, fmt.Sprintf("%s: %d calls, avg %.0fms, max %.0fms",
				s.url, s.count, s.totalMs/float64(s.count), s.maxMs))
		}

		findings = append(findings, DiagFinding{
			Rule:     r.Name(),
			Severity: sev,
			Title:    "Slow External API Calls",
			Description: fmt.Sprintf(
				"%d requests (%.1f%%) are bottlenecked by external HTTP calls. "+
					"External APIs are adding >50%% of total request time.",
				slowHTTPTraces, pct),
			Impact: "External API calls can't be optimized on your end but can be mitigated with caching, async processing, or timeouts.",
			Fix:    "Add transient caching (30s-5min) for API responses. Use curl_multi for parallel requests. Set reasonable timeouts (5s). Consider async processing with wp_cron for non-critical calls.",
			Evidence: evidence,
			Score:    score,
		})
	}

	return findings
}

// Rule: High Memory Usage
type RuleHighMemory struct{}

func (r *RuleHighMemory) Name() string { return "high_memory" }

func (r *RuleHighMemory) Evaluate(ctx *DiagContext) []DiagFinding {
	highMemTraces := 0
	var maxMem float64
	var worstURI string

	for _, t := range ctx.Traces {
		if t.MemoryMB > 64 {
			highMemTraces++
		}
		if t.MemoryMB > maxMem {
			maxMem = t.MemoryMB
			worstURI = t.URI
		}
	}

	if highMemTraces == 0 && maxMem < 48 {
		return nil
	}

	if highMemTraces > 0 {
		pct := float64(highMemTraces) / float64(ctx.TotalReqs) * 100
		sev := SevWarning
		score := 15.0
		if pct > 20 || maxMem > 128 {
			sev = SevCritical
			score = 25
		}

		return []DiagFinding{{
			Rule:     r.Name(),
			Severity: sev,
			Title:    "High Memory Usage Detected",
			Description: fmt.Sprintf(
				"%d requests (%.1f%%) use >64MB of memory. Peak: %.0fMB on %s",
				highMemTraces, pct, maxMem, worstURI),
			Impact:   "High memory usage increases the risk of hitting PHP memory_limit, causing fatal errors. It also reduces the number of concurrent requests the server can handle.",
			Fix:      "Check for large database result sets (use LIMIT). Disable unnecessary WordPress plugins. Avoid loading entire files into memory (use streaming). Check for memory leaks in loops.",
			Evidence: []string{fmt.Sprintf("Peak memory: %.0fMB on %s", maxMem, worstURI)},
			Score:    score,
		}}
	}

	// Just a note about approaching limits
	if maxMem > 48 {
		return []DiagFinding{{
			Rule:     r.Name(),
			Severity: SevInfo,
			Title:    "Memory Usage Approaching Limits",
			Description: fmt.Sprintf(
				"Peak memory usage is %.0fMB (on %s). Default PHP memory_limit is 128MB.",
				maxMem, worstURI),
			Impact:   "Currently safe, but spikes could trigger memory_limit errors.",
			Fix:      "Monitor and optimize if trending upward.",
			Evidence: []string{fmt.Sprintf("Peak: %.0fMB on %s", maxMem, worstURI)},
			Score:    5,
		}}
	}

	return nil
}

// Rule: High Error Rate
type RuleHighErrorRate struct{}

func (r *RuleHighErrorRate) Name() string { return "high_error_rate" }

func (r *RuleHighErrorRate) Evaluate(ctx *DiagContext) []DiagFinding {
	if ctx.ErrorCount == 0 {
		return nil
	}

	pct := float64(ctx.ErrorCount) / float64(ctx.TotalReqs) * 100

	var sev Severity
	var score float64
	switch {
	case pct > 10:
		sev = SevCritical
		score = 30
	case pct > 2:
		sev = SevWarning
		score = 18
	default:
		sev = SevInfo
		score = 8
	}

	// Count error types
	errTypes := make(map[int]int)
	for _, t := range ctx.Traces {
		if t.Status >= 500 {
			errTypes[t.Status]++
		}
	}

	var evidence []string
	for status, count := range errTypes {
		evidence = append(evidence, fmt.Sprintf("HTTP %d: %d requests", status, count))
	}

	// Find most erroring URIs (with host)
	errURIs := make(map[string]int)
	for _, t := range ctx.Traces {
		if t.Status >= 500 {
			errURIs[t.Host+t.URI]++
		}
	}
	type kv struct {
		k string
		v int
	}
	var sorted []kv
	for k, v := range errURIs {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })
	for i, s := range sorted {
		if i >= 3 {
			break
		}
		evidence = append(evidence, fmt.Sprintf("URI %s: %d errors", s.k, s.v))
	}

	return []DiagFinding{{
		Rule:     r.Name(),
		Severity: sev,
		Title:    "High Error Rate",
		Description: fmt.Sprintf(
			"%d out of %d requests (%.1f%%) returned 5xx errors.",
			ctx.ErrorCount, ctx.TotalReqs, pct),
		Impact:   "5xx errors mean broken user experience. Search engines may deindex pages with persistent errors.",
		Fix:      "Check PHP error logs for the specific error. Common causes: database connection limits, memory exhaustion, plugin conflicts, uncaught exceptions.",
		Evidence: evidence,
		Score:    score,
	}}
}

// Rule: Slow Response Times
type RuleSlowResponses struct{}

func (r *RuleSlowResponses) Name() string { return "slow_responses" }

func (r *RuleSlowResponses) Evaluate(ctx *DiagContext) []DiagFinding {
	slowCount := 0 // > 1s
	verySlowCount := 0 // > 3s
	for _, t := range ctx.Traces {
		if t.DurationMs > 3000 {
			verySlowCount++
			slowCount++
		} else if t.DurationMs > 1000 {
			slowCount++
		}
	}

	if slowCount == 0 {
		return nil
	}

	var findings []DiagFinding

	if verySlowCount > 0 {
		pct := float64(verySlowCount) / float64(ctx.TotalReqs) * 100
		sev := SevWarning
		score := 15.0
		if pct > 5 {
			sev = SevCritical
			score = 25
		}

		// Find slowest URIs
		type uriDur struct {
			uri    string
			maxMs  float64
			count  int
		}
		uriMap := make(map[string]*uriDur)
		for _, t := range ctx.Traces {
			if t.DurationMs > 3000 {
				key := t.Host + t.URI
				if u, ok := uriMap[key]; ok {
					u.count++
					if t.DurationMs > u.maxMs {
						u.maxMs = t.DurationMs
					}
				} else {
					uriMap[key] = &uriDur{key, t.DurationMs, 1}
				}
			}
		}
		var evidence []string
		var sorted []*uriDur
		for _, u := range uriMap {
			sorted = append(sorted, u)
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].maxMs > sorted[j].maxMs })
		for i, u := range sorted {
			if i >= 3 {
				break
			}
			evidence = append(evidence, fmt.Sprintf("%s: %d× very slow (max %.0fms)", u.uri, u.count, u.maxMs))
		}

		findings = append(findings, DiagFinding{
			Rule:     r.Name(),
			Severity: sev,
			Title:    "Very Slow Responses (>3s)",
			Description: fmt.Sprintf(
				"%d requests (%.1f%%) took over 3 seconds. P95: %.0fms, Max: %.0fms.",
				verySlowCount, pct, ctx.P95Duration, ctx.MaxDuration),
			Impact:   "Requests >3s cause poor user experience and timeouts. Google considers page speed a ranking factor.",
			Fix:      "Check the trace detail for these URIs to identify the bottleneck (DB, HTTP, PHP). Use caching (page cache, object cache) for repeated slow requests.",
			Evidence: evidence,
			Score:    score,
		})
	}

	return findings
}

// Rule: WordPress wp_options autoload bloat
type RuleWPAutoload struct{}

func (r *RuleWPAutoload) Name() string { return "wp_autoload_bloat" }

func (r *RuleWPAutoload) Evaluate(ctx *DiagContext) []DiagFinding {
	// Check if any traces are WordPress
	wpCount := 0
	for _, t := range ctx.Traces {
		if t.WP == 1 {
			wpCount++
		}
	}
	if wpCount == 0 {
		return nil
	}

	// Look for wp_options autoload queries
	autoloadQueries := 0
	var autoloadDurations []float64

	for _, t := range ctx.Traces {
		for _, q := range t.Queries {
			sql := strings.ToLower(q.SQL)
			if strings.Contains(sql, "wp_options") && strings.Contains(sql, "autoload") {
				autoloadQueries++
				autoloadDurations = append(autoloadDurations, q.DurationMs)
			}
		}
	}

	if autoloadQueries == 0 {
		return nil
	}

	// Calculate average autoload query time
	var totalMs float64
	var maxMs float64
	for _, d := range autoloadDurations {
		totalMs += d
		if d > maxMs {
			maxMs = d
		}
	}
	avgMs := totalMs / float64(len(autoloadDurations))

	// Only flag if autoload is actually slow
	if avgMs < 10 && maxMs < 50 {
		return nil
	}

	sev := SevWarning
	score := 15.0
	if avgMs > 50 || maxMs > 200 {
		sev = SevCritical
		score = 25
	}

	return []DiagFinding{{
		Rule:     r.Name(),
		Severity: sev,
		Title:    "WordPress wp_options Autoload Bloat",
		Description: fmt.Sprintf(
			"The wp_options autoload query takes %.1fms on average (max %.1fms). "+
				"This runs on EVERY page load and loads all autoloaded options into memory.",
			avgMs, maxMs),
		Impact:   "Autoload bloat slows down every single page load. Each unnecessary autoloaded option adds memory usage and query time.",
		Fix:      "Run: SELECT LENGTH(option_value) as size, option_name FROM wp_options WHERE autoload='yes' ORDER BY size DESC LIMIT 20. " +
			"Remove or disable autoload for large transients and unused plugin options. Use: UPDATE wp_options SET autoload='no' WHERE option_name='...'",
		Evidence: []string{
			fmt.Sprintf("Autoload query: avg %.1fms, max %.1fms, %d× observed", avgMs, maxMs, autoloadQueries),
		},
		Score: score,
	}}
}

// Rule: Missing OPcache indicators
type RuleMissingOpcache struct{}

func (r *RuleMissingOpcache) Name() string { return "missing_opcache" }

func (r *RuleMissingOpcache) Evaluate(ctx *DiagContext) []DiagFinding {
	// Heuristic: if PHP processing time is high but DB/HTTP are low,
	// OPcache might be disabled or misconfigured.
	// Look for requests where PHP time (total - db - http - file) is >80% and duration > 500ms
	phpBottleneckCount := 0
	for _, t := range ctx.Traces {
		if t.DurationMs < 500 {
			continue
		}
		phpMs := t.DurationMs - t.DBMs - t.HTTPMs - t.FileMs - t.RedisMs
		if phpMs > 0 && phpMs/t.DurationMs > 0.8 {
			phpBottleneckCount++
		}
	}

	if phpBottleneckCount < 3 {
		return nil
	}

	pct := float64(phpBottleneckCount) / float64(ctx.TotalReqs) * 100
	if pct < 5 {
		return nil
	}

	return []DiagFinding{{
		Rule:     r.Name(),
		Severity: SevInfo,
		Title:    "PHP Processing Bottleneck — Check OPcache",
		Description: fmt.Sprintf(
			"%d requests (%.1f%%) are slow (>500ms) with >80%% time in PHP processing (not DB/HTTP/file). "+
				"This could indicate OPcache is disabled or needs tuning.",
			phpBottleneckCount, pct),
		Impact:   "Without OPcache, PHP recompiles scripts on every request, adding 10-50ms overhead.",
		Fix:      "Verify OPcache is enabled: php -i | grep opcache.enable. " +
			"Tune: opcache.memory_consumption=256, opcache.max_accelerated_files=20000, opcache.revalidate_freq=60.",
		Score: 10,
	}}
}

// Rule: PHP Version Recommendation
type RulePhpVersion struct{}

func (r *RulePhpVersion) Name() string { return "php_version_check" }

func (r *RulePhpVersion) Evaluate(ctx *DiagContext) []DiagFinding {
	// Collect unique PHP versions across all traces, grouped by domain
	type verInfo struct {
		requests int
		domains  map[string]int // domain → request count
	}
	versions := make(map[string]*verInfo)

	for _, t := range ctx.Traces {
		if t.PhpVer == "" {
			continue
		}
		vi, ok := versions[t.PhpVer]
		if !ok {
			vi = &verInfo{domains: make(map[string]int)}
			versions[t.PhpVer] = vi
		}
		vi.requests++
		if t.Host != "" {
			vi.domains[t.Host]++
		}
	}

	if len(versions) == 0 {
		return nil
	}

	// Parse major.minor from version strings
	parseMajorMinor := func(ver string) (int, int) {
		major, minor := 0, 0
		fmt.Sscanf(ver, "%d.%d", &major, &minor)
		return major, minor
	}

	// Check for outdated versions
	var findings []DiagFinding
	hasEOL := false      // PHP < 8.1 (EOL since Nov 2023)
	hasOldSupported := false // PHP 8.1 (security-only since Nov 2024)
	var evidence []string
	totalVersions := len(versions)

	// Sort versions for consistent evidence output
	type verEntry struct {
		ver  string
		info *verInfo
	}
	var sorted []verEntry
	for v, vi := range versions {
		sorted = append(sorted, verEntry{v, vi})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ver < sorted[j].ver })

	for _, entry := range sorted {
		ver := entry.ver
		vi := entry.info
		major, minor := parseMajorMinor(ver)

		// Build domain list for evidence
		var domList []string
		for d, c := range vi.domains {
			domList = append(domList, fmt.Sprintf("%s (%d reqs)", d, c))
		}
		sort.Strings(domList)

		if major < 8 || (major == 8 && minor < 1) {
			// PHP < 8.1 — EOL
			hasEOL = true
			evidence = append(evidence, fmt.Sprintf("PHP %s: %d requests — %s [⛔ EOL]",
				ver, vi.requests, strings.Join(domList, ", ")))
		} else if major == 8 && minor == 1 {
			// PHP 8.1 — security-only since Nov 2024, EOL Dec 2025
			hasOldSupported = true
			evidence = append(evidence, fmt.Sprintf("PHP %s: %d requests — %s [⚠️ security-only]",
				ver, vi.requests, strings.Join(domList, ", ")))
		} else {
			// PHP 8.2+ — actively supported
			evidence = append(evidence, fmt.Sprintf("PHP %s: %d requests — %s [✅ supported]",
				ver, vi.requests, strings.Join(domList, ", ")))
		}
	}

	// Mixed version note
	if totalVersions > 1 {
		evidence = append(evidence, fmt.Sprintf("⚡ %d different PHP versions detected across domains", totalVersions))
	}

	if hasEOL {
		findings = append(findings, DiagFinding{
			Rule:     r.Name(),
			Severity: SevCritical,
			Title:    "End-of-Life PHP Version Detected",
			Description: "One or more domains are running PHP versions that have reached end-of-life " +
				"and no longer receive security patches. PHP 8.0 reached EOL in November 2023. " +
				"PHP 7.x has been EOL since November 2022.",
			Impact: "EOL PHP versions receive no security updates, leaving the server vulnerable to " +
				"known exploits. They also miss significant performance improvements in PHP 8.2+.",
			Fix: "Upgrade to PHP 8.2 or newer. PHP 8.2 introduced readonly classes, DNF types, and " +
				"significant performance improvements (up to 5-10% faster for WordPress). " +
				"PHP 8.4 is the latest stable release (as of 2026). Test compatibility with " +
				"php -l and your test suite before switching.",
			Evidence: evidence,
			Score:    30,
		})
	} else if hasOldSupported {
		findings = append(findings, DiagFinding{
			Rule:     r.Name(),
			Severity: SevWarning,
			Title:    "PHP Version Nearing End-of-Life",
			Description: "One or more domains are running PHP 8.1, which is in security-only support " +
				"(no bug fixes, only critical security patches). Active support ended November 2024.",
			Impact: "PHP 8.1 still receives security patches but misses performance improvements " +
				"and new features from PHP 8.2/8.3/8.4. Upgrade soon before EOL.",
			Fix: "Upgrade to PHP 8.2 or 8.3 for active support with bug fixes and performance gains. " +
				"PHP 8.2 has significant performance improvements for WordPress workloads.",
			Evidence: evidence,
			Score:    15,
		})
	} else if totalVersions > 1 {
		// Multiple supported versions — just an info note
		findings = append(findings, DiagFinding{
			Rule:     r.Name(),
			Severity: SevInfo,
			Title:    "Multiple PHP Versions in Use",
			Description: fmt.Sprintf("%d different PHP versions detected across domains. "+
				"All versions are actively supported.", totalVersions),
			Impact:   "Running different PHP versions increases maintenance complexity. Consider standardizing.",
			Fix:      "Align all domains to the same PHP version where possible for simpler maintenance.",
			Evidence: evidence,
			Score:    3,
		})
	}

	return findings
}

// ─── Helper functions ───

func extractHost(url string) string {
	// Extract host from URL like "https://api.stripe.com/v1/charges"
	url = strings.TrimPrefix(url, "http://")
	url = strings.TrimPrefix(url, "https://")
	if idx := strings.Index(url, "/"); idx > 0 {
		url = url[:idx]
	}
	if idx := strings.Index(url, "?"); idx > 0 {
		url = url[:idx]
	}
	return url
}

// ─── Storage Integration ───

func (s *Storage) LoadTracesForDiag(domain string, windowMin int) (*DiagContext, error) {
	cutoff := currentTime() - int64(windowMin)*60

	query := `SELECT id, phpray_id, host, uri, method, status, duration_ms,
		db_count, db_ms, http_count, http_ms, file_count, file_ms,
		COALESCE(redis_count,0), COALESCE(redis_ms,0),
		memory_peak_mb, wp, n_plus_one, trace_level, error_count,
		COALESCE(php_version, '') as php_version
		FROM traces WHERE timestamp >= ?`
	args := []interface{}{cutoff}

	if domain != "" {
		query += " AND host LIKE ?"
		args = append(args, "%"+domain+"%")
	}
	query += " ORDER BY timestamp DESC LIMIT 1000"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ctx := &DiagContext{Domain: domain}
	for rows.Next() {
		var t TraceRecord
		if err := rows.Scan(&t.ID, &t.PhprayID, &t.Host, &t.URI, &t.Method, &t.Status,
			&t.DurationMs, &t.DBCount, &t.DBMs, &t.HTTPCount, &t.HTTPMs,
			&t.FileCount, &t.FileMs, &t.RedisCount, &t.RedisMs,
			&t.MemoryMB, &t.WP, &t.N1,
			&t.Level, &t.ErrorCount, &t.PhpVer); err != nil {
			continue
		}
		ctx.Traces = append(ctx.Traces, t)
	}

	// Load queries and HTTP calls for traces that have them (normal+ level)
	for i, t := range ctx.Traces {
		if t.Level == "summary" {
			continue
		}

		// Load queries
		qRows, err := s.db.Query(`SELECT sql_text, sql_fingerprint, duration_ms, offset_ms
			FROM queries WHERE trace_id = ? ORDER BY offset_ms`, t.ID)
		if err == nil {
			for qRows.Next() {
				var q QueryRecord
				qRows.Scan(&q.SQL, &q.Fingerprint, &q.DurationMs, &q.OffsetMs)
				ctx.Traces[i].Queries = append(ctx.Traces[i].Queries, q)
			}
			qRows.Close()
		}

		// Load HTTP calls
		hRows, err := s.db.Query(`SELECT url, duration_ms, status
			FROM http_calls WHERE trace_id = ? ORDER BY offset_ms`, t.ID)
		if err == nil {
			for hRows.Next() {
				var h HTTPRecord
				hRows.Scan(&h.URL, &h.DurationMs, &h.Status)
				ctx.Traces[i].HTTPCalls = append(ctx.Traces[i].HTTPCalls, h)
			}
			hRows.Close()
		}

		// Load errors
		eRows, err := s.db.Query(`SELECT error_type, message
			FROM errors WHERE trace_id = ?`, t.ID)
		if err == nil {
			for eRows.Next() {
				var e ErrorRecord
				eRows.Scan(&e.Type, &e.Message)
				ctx.Traces[i].Errors = append(ctx.Traces[i].Errors, e)
			}
			eRows.Close()
		}
	}

	// Compute stats
	ctx.TotalReqs = len(ctx.Traces)
	var totalDur float64
	for _, t := range ctx.Traces {
		totalDur += t.DurationMs
		if t.DurationMs > ctx.MaxDuration {
			ctx.MaxDuration = t.DurationMs
		}
		if t.Status >= 500 {
			ctx.ErrorCount++
		}
		if t.N1 == 1 {
			ctx.N1Count++
		}
		ctx.TotalDB += t.DBCount
		ctx.TotalHTTP += t.HTTPCount
	}
	if ctx.TotalReqs > 0 {
		ctx.AvgDuration = totalDur / float64(ctx.TotalReqs)
	}

	// P95
	if ctx.TotalReqs > 0 {
		durations := make([]float64, len(ctx.Traces))
		for i, t := range ctx.Traces {
			durations[i] = t.DurationMs
		}
		sort.Float64s(durations)
		idx := int(float64(len(durations)) * 0.95)
		if idx >= len(durations) {
			idx = len(durations) - 1
		}
		ctx.P95Duration = durations[idx]
	}

	return ctx, nil
}

// ─── Report Generators ───

// GenerateMarkdownReport renders a DiagReport as a markdown document
func GenerateMarkdownReport(report *DiagReport) string {
	var b strings.Builder

	domain := report.Domain
	if domain == "" {
		domain = "All Domains"
	}

	// Health emoji
	healthEmoji := "\u2764\ufe0f" // ❤️
	if report.HealthScore < 80 {
		healthEmoji = "\U0001F49B" // 💛
	}
	if report.HealthScore < 50 {
		healthEmoji = "\U0001F534" // 🔴
	}

	// Header
	b.WriteString(fmt.Sprintf("# PHPRay Diagnostic Report — %s\n\n", domain))
	b.WriteString(fmt.Sprintf("**Date:** %s  \n", time.Now().Format("2006-01-02 15:04:05 MST")))
	b.WriteString(fmt.Sprintf("**Window:** %d minutes  \n", report.WindowMin))
	b.WriteString(fmt.Sprintf("**Traces analyzed:** %d  \n", report.TraceCount))
	b.WriteString(fmt.Sprintf("**Health Score:** %s %d/100  \n\n", healthEmoji, report.HealthScore))

	// Summary
	b.WriteString("## Summary\n\n")
	b.WriteString(report.Summary)
	b.WriteString("\n\n")

	// Findings overview table
	if len(report.Findings) > 0 {
		b.WriteString("## Findings Overview\n\n")
		b.WriteString("| # | Severity | Finding | Score |\n")
		b.WriteString("|---|----------|---------|-------|\n")
		for i, f := range report.Findings {
			sevIcon := "\u2139\ufe0f" // ℹ️
			if f.Severity == SevWarning {
				sevIcon = "\U0001F7E1" // 🟡
			}
			if f.Severity == SevCritical {
				sevIcon = "\U0001F534" // 🔴
			}
			b.WriteString(fmt.Sprintf("| %d | %s %s | %s | %.0f |\n",
				i+1, sevIcon, f.Severity, f.Title, f.Score))
		}
		b.WriteString("\n")

		// Detailed findings
		b.WriteString("## Detailed Findings\n\n")
		for i, f := range report.Findings {
			sevIcon := "\u2139\ufe0f"
			if f.Severity == SevWarning {
				sevIcon = "\U0001F7E1"
			}
			if f.Severity == SevCritical {
				sevIcon = "\U0001F534"
			}

			b.WriteString(fmt.Sprintf("### %d. %s %s\n\n", i+1, sevIcon, f.Title))
			b.WriteString(fmt.Sprintf("**Severity:** %s  \n", f.Severity))
			b.WriteString(fmt.Sprintf("**Rule:** `%s`  \n\n", f.Rule))

			b.WriteString("**Description:**  \n")
			b.WriteString(f.Description)
			b.WriteString("\n\n")

			if f.Impact != "" {
				b.WriteString("**Impact:**  \n")
				b.WriteString(f.Impact)
				b.WriteString("\n\n")
			}

			if len(f.Evidence) > 0 {
				b.WriteString("**Evidence:**\n")
				for _, ev := range f.Evidence {
					b.WriteString(fmt.Sprintf("- `%s`\n", ev))
				}
				b.WriteString("\n")
			}

			if f.Fix != "" {
				b.WriteString("**Recommended Fix:**  \n")
				b.WriteString(f.Fix)
				b.WriteString("\n\n")
			}

			b.WriteString("---\n\n")
		}
	} else {
		b.WriteString("## Findings\n\nNo issues detected. The application looks healthy!\n\n")
	}

	// Footer
	b.WriteString(fmt.Sprintf("---\n*Generated by PHPRay v%s at %s*\n", version, time.Now().Format(time.RFC3339)))

	return b.String()
}

// GeneratePlainTextReport renders a DiagReport as plain text
func GeneratePlainTextReport(report *DiagReport) string {
	var b strings.Builder

	domain := report.Domain
	if domain == "" {
		domain = "All Domains"
	}

	b.WriteString(fmt.Sprintf("PHPRay Diagnostic Report — %s\n", domain))
	b.WriteString(strings.Repeat("=", 60))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Date:            %s\n", time.Now().Format("2006-01-02 15:04:05 MST")))
	b.WriteString(fmt.Sprintf("Window:          %d minutes\n", report.WindowMin))
	b.WriteString(fmt.Sprintf("Traces analyzed: %d\n", report.TraceCount))
	b.WriteString(fmt.Sprintf("Health Score:    %d/100\n\n", report.HealthScore))

	b.WriteString("SUMMARY\n")
	b.WriteString(strings.Repeat("-", 60))
	b.WriteString("\n")
	b.WriteString(report.Summary)
	b.WriteString("\n\n")

	if len(report.Findings) > 0 {
		b.WriteString("FINDINGS\n")
		b.WriteString(strings.Repeat("-", 60))
		b.WriteString("\n\n")
		for i, f := range report.Findings {
			sevTag := "[INFO]"
			if f.Severity == SevWarning {
				sevTag = "[WARN]"
			}
			if f.Severity == SevCritical {
				sevTag = "[CRIT]"
			}

			b.WriteString(fmt.Sprintf("%d. %s %s (score: %.0f)\n", i+1, sevTag, f.Title, f.Score))
			b.WriteString(fmt.Sprintf("   Rule: %s\n", f.Rule))
			b.WriteString(fmt.Sprintf("   %s\n", f.Description))
			if f.Impact != "" {
				b.WriteString(fmt.Sprintf("   Impact: %s\n", f.Impact))
			}
			if len(f.Evidence) > 0 {
				b.WriteString("   Evidence:\n")
				for _, ev := range f.Evidence {
					b.WriteString(fmt.Sprintf("     - %s\n", ev))
				}
			}
			if f.Fix != "" {
				b.WriteString(fmt.Sprintf("   Fix: %s\n", f.Fix))
			}
			b.WriteString("\n")
		}
	} else {
		b.WriteString("No issues detected. The application looks healthy!\n\n")
	}

	b.WriteString(strings.Repeat("-", 60))
	b.WriteString(fmt.Sprintf("\nGenerated by PHPRay v%s at %s\n", version, time.Now().Format(time.RFC3339)))

	return b.String()
}

// currentTime returns current Unix timestamp (mockable for tests)
var currentTime = func() int64 {
	return time.Now().Unix()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
