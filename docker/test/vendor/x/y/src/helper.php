<?php
/**
 * Fake composer package x/y → component "vendor/x/y".
 */
cnt('vendor/x/y');

function x_helper(int $callbacks): int {
    cnt('vendor/x/y');
    $s = core_work(15000);
    for ($i = 0; $i < $callbacks; $i++) {
        $s += a_callback(6000);      // calls back into plugins/a (plugin nested under vendor)
    }
    return $s;
}
