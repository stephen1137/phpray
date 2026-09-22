<?php
/**
 * Verifies the JSONL traces written by the extension for components.php
 * against what the script itself reported (docker/test/components_client.php).
 *
 * Usage: php components_check.php <mode> <trace.jsonl> <client.json>
 *   mode: all | off | sample | url
 *
 * Checks (profiled requests):
 *   - "profiled":1 and a components section with exactly plugins/a, themes/b, vendor/x/y
 *     (never wp-includes, never the entry script)
 *   - calls per component == number of observed entries counted in PHP
 *   - 0 < self_ns <= incl_ns <= duration_ns for every component
 *   - sum(self_ns) ≈ wall time the app spent inside top-level component calls
 *     (Σ self is the union of component intervals: within [0.75, 1.02] × measured;
 *     the lower bound leaves room for scheduler noise on a busy host)
 *   - max(incl_ns) <= measured, pct == incl / duration
 *   - per-process classification cache (phpray_profile_stats in the body): after the first
 *     profiled request cache_misses stays constant while cache_hits keeps growing, i.e. the
 *     observer init never re-scans a path it has seen in this process
 * Checks (non-profiled requests): "profiled":0 and no components key.
 * Mode-specific: off → nothing profiled; all → everything; sample (rate 50) → 30–70 %;
 * url → exactly the requests whose URI matches the prefix.
 */
$mode = $argv[1] ?? 'all';
$jsonl = $argv[2] ?? '/tmp/phpray.jsonl';
$clientFile = $argv[3] ?? '/tmp/client.json';

$errors = [];
$warn = [];
$err = function (string $m) use (&$errors) { $errors[] = $m; };

$client = json_decode((string)file_get_contents($clientFile), true);
if (!is_array($client) || empty($client['results'])) {
    fwrite(STDERR, "no client results in $clientFile\n");
    exit(2);
}

$traces = [];
$bad = 0;
foreach (file($jsonl, FILE_IGNORE_NEW_LINES | FILE_SKIP_EMPTY_LINES) ?: [] as $line) {
    $t = json_decode($line, true);
    if (!is_array($t)) { $bad++; continue; }
    if (isset($t['id'])) $traces[$t['id']] = $t;
}
if ($bad > 0) $err("$bad unparsable JSONL lines");

$profiledCount = 0;
$checked = 0;
$callsTotal = 0;
$sumSelfRatio = [];
foreach ($client['results'] as $r) {
    $id = $r['header_id'] ?: ($r['body']['id'] ?? null);
    if (!$id || !isset($traces[$id])) { $err("request {$r['i']}: no trace for id " . var_export($id, true)); continue; }
    $t = $traces[$id];
    $checked++;
    $expected = $r['body']['expected_calls'] ?? [];
    $measured = (int)($r['body']['app_measured_ns'] ?? 0);
    $duration = (int)round(($t['duration_ms'] ?? 0) * 1e6);

    $shouldProfile = match ($mode) {
        'all'    => true,
        'off'    => false,
        'url'    => str_contains($r['uri'], 'prof=1'),
        default  => null,   // sample: either
    };
    $profiled = (int)($t['profiled'] ?? -1);
    if ($profiled !== 0 && $profiled !== 1) $err("request {$r['i']}: profiled field missing/invalid");
    if ($shouldProfile === true && $profiled !== 1) $err("request {$r['i']}: expected profiled=1 (mode $mode, uri {$r['uri']}), got $profiled");
    if ($shouldProfile === false && $profiled !== 0) $err("request {$r['i']}: expected profiled=0 (mode $mode, uri {$r['uri']}), got $profiled");

    if ($profiled === 0) {
        if (isset($t['components'])) $err("request {$r['i']}: profiled=0 but components present");
        continue;
    }
    $profiledCount++;
    if (!isset($t['components']) || !is_array($t['components'])) { $err("request {$r['i']}: profiled=1 but no components"); continue; }

    $comps = [];
    foreach ($t['components'] as $c) $comps[$c['name']] = $c;
    $names = array_keys($comps);
    sort($names);
    $expNames = array_keys($expected);
    sort($expNames);
    if ($names !== $expNames) $err("request {$r['i']}: component set " . json_encode($names) . " != expected " . json_encode($expNames));
    foreach (['wp-includes', 'app', 'other'] as $forbidden) {
        if (isset($comps[$forbidden])) $err("request {$r['i']}: forbidden component $forbidden present");
    }
    $sumSelf = 0; $maxIncl = 0;
    foreach ($expected as $name => $calls) {
        if (!isset($comps[$name])) continue;
        $c = $comps[$name];
        $callsTotal += (int)$c['calls'];
        if ((int)$c['calls'] !== (int)$calls) $err("request {$r['i']}: $name calls={$c['calls']} expected $calls");
        $incl = (int)$c['incl_ns']; $self = (int)$c['self_ns'];
        if ($self <= 0) $err("request {$r['i']}: $name self_ns=$self (expected > 0)");
        if ($self > $incl) $err("request {$r['i']}: $name self_ns=$self > incl_ns=$incl");
        if ($incl > $duration) $err("request {$r['i']}: $name incl_ns=$incl > duration_ns=$duration");
        if (abs(($c['ms'] ?? 0) - $incl / 1e6) > 0.002) $err("request {$r['i']}: $name ms={$c['ms']} != incl_ns/1e6");
        $pct = $duration > 0 ? $incl / $duration * 100 : 0;
        if (abs(($c['pct'] ?? 0) - $pct) > 0.15) $err("request {$r['i']}: $name pct={$c['pct']} != " . round($pct, 1));
        $sumSelf += $self; $maxIncl = max($maxIncl, $incl);
    }
    if ($measured > 0) {
        $ratio = $sumSelf / $measured;
        $sumSelfRatio[] = $ratio;
        if ($ratio < 0.75 || $ratio > 1.02) $err(sprintf("request %d: sum(self)=%d vs app-measured=%d (ratio %.3f outside [0.75, 1.02])", $r['i'], $sumSelf, $measured, $ratio));
        if ($maxIncl > $measured * 1.02) $err(sprintf("request %d: max(incl)=%d > app-measured=%d", $r['i'], $maxIncl, $measured));
    }
    if (!empty($t['prof_overflow'])) $warn[] = "request {$r['i']}: prof_overflow set";
    // the first profiled request is the one whose numbers we print
    if ($profiledCount === 1) {
        printf("sample trace %s: duration %.2f ms, app-measured %.2f ms, sum(self) %.2f ms\n", $id, $duration / 1e6, $measured / 1e6, $sumSelf / 1e6);
        foreach ($t['components'] as $c) {
            printf("  %-12s incl %8.3f ms  self %8.3f ms  calls %5d  pct %5.1f\n", $c['name'], $c['incl_ns'] / 1e6, $c['self_ns'] / 1e6, $c['calls'], $c['pct']);
        }
    }
}

// classification cache: misses frozen after the first profiled request, hits growing
$statsSeq = [];
foreach ($client['results'] as $r) {
    $st = $r['body']['stats'] ?? null;
    if (is_array($st) && !empty($st['profiled'])) $statsSeq[] = $st;
}
if (count($statsSeq) >= 3) {
    $first = $statsSeq[0]; $second = $statsSeq[1]; $last = end($statsSeq);
    if ($first['cache_misses'] <= 0) $err("cache: no misses recorded in the first profiled request");
    if ($second['cache_misses'] !== $first['cache_misses'] || $last['cache_misses'] !== $first['cache_misses'])
        $err(sprintf("cache: misses keep growing after the first profiled request (%d → %d → %d): paths are re-scanned", $first['cache_misses'], $second['cache_misses'], $last['cache_misses']));
    if ($last['cache_hits'] <= $second['cache_hits']) $err(sprintf("cache: hits do not grow (%d → %d)", $second['cache_hits'], $last['cache_hits']));
    if ($last['cache_evictions'] > 0) $warn[] = "cache: {$last['cache_evictions']} evictions";
    if ($last['names'] !== 3) $err("cache: expected 3 interned component names, got {$last['names']}");
    if (($last['jsonl_errors'] ?? 0) > 0) $err("jsonl: {$last['jsonl_errors']} write errors");
    printf("cache: misses %d (frozen after request 1), hits %d → %d, names %d, evictions %d, jsonl records %d\n",
        $first['cache_misses'], $second['cache_hits'], $last['cache_hits'], $last['names'], $last['cache_evictions'], $last['jsonl_records'] ?? -1);
}

if ($mode === 'sample' && $checked > 0) {
    $frac = $profiledCount / $checked;
    if ($frac < 0.30 || $frac > 0.70) $err(sprintf("sample: profiled fraction %.2f outside [0.30, 0.70] (rate 50)", $frac));
}
if ($mode === 'all' && $profiledCount !== $checked) $err("all: $profiledCount of $checked profiled");
if ($mode === 'off' && $profiledCount !== 0) $err("off: $profiledCount profiled");

printf("mode=%s traces=%d checked=%d profiled=%d observed_calls_total=%d sum(self)/measured=%s\n",
    $mode, count($traces), $checked, $profiledCount, $callsTotal,
    $sumSelfRatio ? sprintf("%.3f..%.3f", min($sumSelfRatio), max($sumSelfRatio)) : '-');
foreach ($warn as $w) echo "WARN: $w\n";
if ($errors) {
    echo "FAIL (" . count($errors) . " errors):\n";
    foreach (array_slice($errors, 0, 25) as $e) echo "  - $e\n";
    exit(1);
}
echo "OK\n";
