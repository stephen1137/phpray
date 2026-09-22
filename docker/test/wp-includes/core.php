<?php
/**
 * Fake WordPress core — NOT a component: the observer must never attach here.
 * Time spent in core_work() must be charged to whichever component called it.
 */
function core_work(int $n): int {
    $s = 0;
    for ($i = 0; $i < $n; $i++) {
        $s += ($i * 7) % 13;
    }
    return $s;
}

function core_direct(int $n): int {
    // called straight from the entry script (app code) — must not show up anywhere
    return core_work($n);
}
