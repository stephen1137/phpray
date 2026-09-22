<?php
/**
 * Fake theme "b" → component "themes/b".
 */
cnt('themes/b');

function b_render(int $n): int {
    cnt('themes/b');
    $s = core_work($n);             // core called from the theme → charged to themes/b
    $s += x_helper(2);              // nested component: vendor/x/y
    return $s;
}

function b_throw(): void {
    cnt('themes/b');
    core_work(2000);
    throw new RuntimeException('boom from themes/b');
}
