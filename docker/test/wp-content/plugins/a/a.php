<?php
/**
 * Fake plugin "a" → component "plugins/a".
 * Every observed entry (this file's top-level code, each function body, each
 * closure invocation, each generator resume) calls cnt('plugins/a') exactly once,
 * so the entry script can predict the observer's `calls` counter.
 */
cnt('plugins/a');

function a_work(int $rounds): int {
    cnt('plugins/a');
    $s = 0;
    for ($i = 0; $i < $rounds; $i++) {
        $s += core_work(40000);      // core called from a plugin → charged to plugins/a
        $s += b_render(30000);       // nested component: themes/b (→ vendor/x/y → plugins/a)
    }
    return $s;
}

function a_fib(int $n): int {
    cnt('plugins/a');               // recursion inside one component: incl counted once, calls per invocation
    return $n < 2 ? $n : a_fib($n - 1) + a_fib($n - 2);
}

function a_wrap_throw(): void {
    cnt('plugins/a');
    core_work(5000);
    b_throw();                      // exception unwinds through this frame: end() with retval NULL
}

function a_gen(): Generator {
    cnt('plugins/a');               // resume 1
    core_work(3000);
    yield 1;
    cnt('plugins/a');               // resume 2
    core_work(3000);
    yield 2;
    cnt('plugins/a');               // resume 3
    core_work(3000);
    yield 3;
    cnt('plugins/a');               // resume 4 (final return)
}

function a_callback(int $n): int {
    cnt('plugins/a');               // called back from vendor/x/y
    return core_work($n);
}

function a_closure(): Closure {
    cnt('plugins/a');
    return function (int $n): int {
        cnt('plugins/a');           // closure body lives in this file → plugins/a
        return core_work($n * 3000);
    };
}
