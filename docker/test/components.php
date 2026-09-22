<?php
/**
 * PHPRay function-profile test — entry script ("app" code, never observed).
 *
 * Exercises the zend_observer profiler with fake component directories:
 *   wp-content/plugins/a/   → plugins/a
 *   wp-content/themes/b/    → themes/b
 *   vendor/x/y/             → vendor/x/y
 *   wp-includes/            → core (NOT observed; its time is charged to the caller)
 *
 * Scenarios: nested components (plugin → core → theme → vendor → plugin callback),
 * recursion inside one component, an exception unwinding through two components,
 * a generator (begin/end per resume), a closure, and core called from app code.
 *
 * The response carries the exact number of observed entries per component
 * (counted in PHP) and the wall time the app spent inside top-level component
 * calls, so docker/test/components_check.php can verify calls / incl_ns / self_ns
 * in the JSONL trace written by the extension.
 */

$GLOBALS['cnt'] = [];
function cnt(string $component): void {
    $GLOBALS['cnt'][$component] = ($GLOBALS['cnt'][$component] ?? 0) + 1;
}

$measured = 0;          // ns spent by the app inside top-level calls into components

require __DIR__ . '/wp-includes/core.php';            // core: not a component

$t = hrtime(true); require __DIR__ . '/wp-content/plugins/a/a.php';        $measured += hrtime(true) - $t;
$t = hrtime(true); require __DIR__ . '/wp-content/themes/b/functions.php'; $measured += hrtime(true) - $t;
$t = hrtime(true); require __DIR__ . '/vendor/x/y/src/helper.php';         $measured += hrtime(true) - $t;

$rounds = (int)($_GET['rounds'] ?? 6);

// 1. plugin → core → theme → vendor → plugin callback (nested components)
$t = hrtime(true); $r1 = a_work($rounds); $measured += hrtime(true) - $t;

// 2. recursion within plugins/a
$t = hrtime(true); $r2 = a_fib(13); $measured += hrtime(true) - $t;

// 3. exception thrown in themes/b, propagating through a plugins/a frame, caught in app code
$caught = '';
$t = hrtime(true);
try {
    a_wrap_throw();
} catch (RuntimeException $e) {
    $caught = $e->getMessage();
}
$measured += hrtime(true) - $t;

// 4. generator declared in plugins/a (observer: begin/end per resume)
$t = hrtime(true); $sum = 0; foreach (a_gen() as $v) { $sum += $v; } $measured += hrtime(true) - $t;

// 5. closure created in plugins/a, invoked from app code
$t = hrtime(true); $closure = a_closure(); $r5 = $closure(4); $measured += hrtime(true) - $t;

// 6. core called directly from app code — must not appear in the profile at all
$r6 = core_direct(5000);

// 7. vendor package called directly from app code (calls back into plugins/a)
$t = hrtime(true); $r7 = x_helper(3); $measured += hrtime(true) - $t;

header('Content-Type: application/json');
echo json_encode([
    'id'              => function_exists('phpray_request_id') ? phpray_request_id() : null,
    'expected_calls'  => $GLOBALS['cnt'],
    'app_measured_ns' => $measured,
    'checks'          => ['sum' => $sum, 'caught' => $caught, 'r1' => $r1, 'r2' => $r2, 'r5' => $r5, 'r6' => $r6, 'r7' => $r7],
    'profile_mode'    => ini_get('phpray.profile_mode'),
    'uri'             => $_SERVER['REQUEST_URI'] ?? null,
    'stats'           => function_exists('phpray_profile_stats') ? phpray_profile_stats() : null,
]) . "\n";
