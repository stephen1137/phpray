/*
 * PHPRay Redis Hooks
 *
 * Intercepts phpredis Redis::get, Redis::set, Redis::del, etc.
 * Captures: total call count and cumulative time (no individual call recording).
 * This makes Redis time visible in the breakdown bar instead of showing as "idle PHP".
 */

#include "phpray.h"
#include <time.h>

/* Macro for generating simple timing hooks.
 * All Redis hooks follow the same pattern: time the original, add to counters. */
#define PHPRAY_REDIS_HOOK(method_name, orig_field) \
static PHP_FUNCTION(phpray_redis_##method_name) { \
    phpray_request_t *req = PHPRAY_G(current_request); \
    if (!req || !req->is_sampled || !PHPRAY_G(orig_field)) { \
        if (PHPRAY_G(orig_field)) { \
            PHPRAY_G(orig_field)(INTERNAL_FUNCTION_PARAM_PASSTHRU); \
        } \
        return; \
    } \
    struct timespec start, end; \
    clock_gettime(CLOCK_MONOTONIC, &start); \
    PHPRAY_G(orig_field)(INTERNAL_FUNCTION_PARAM_PASSTHRU); \
    clock_gettime(CLOCK_MONOTONIC, &end); \
    uint64_t dur = phpray_time_diff_ns(&start, &end); \
    req->redis_call_count++; \
    req->redis_total_ns += dur; \
}

/* Generate hooks for each Redis method */
PHPRAY_REDIS_HOOK(get, orig_redis_get)
PHPRAY_REDIS_HOOK(set, orig_redis_set)
PHPRAY_REDIS_HOOK(del, orig_redis_del)
PHPRAY_REDIS_HOOK(mget, orig_redis_mget)
PHPRAY_REDIS_HOOK(setex, orig_redis_setex)
PHPRAY_REDIS_HOOK(exists, orig_redis_exists)
PHPRAY_REDIS_HOOK(hget, orig_redis_hget)
PHPRAY_REDIS_HOOK(hset, orig_redis_hset)
PHPRAY_REDIS_HOOK(hdel, orig_redis_hdel)
PHPRAY_REDIS_HOOK(incr, orig_redis_incr)
PHPRAY_REDIS_HOOK(lpush, orig_redis_lpush)
PHPRAY_REDIS_HOOK(rpush, orig_redis_rpush)
PHPRAY_REDIS_HOOK(expire, orig_redis_expire)
PHPRAY_REDIS_HOOK(ttl, orig_redis_ttl)
PHPRAY_REDIS_HOOK(ping, orig_redis_ping)

/* Macro for hooking a Redis method */
#define HOOK_REDIS_METHOD(ce, method, orig_field, hook_func) do { \
    zend_function *func = (zend_function *)zend_hash_str_find_ptr( \
        &(ce)->function_table, method, sizeof(method) - 1); \
    if (func && func->type == ZEND_INTERNAL_FUNCTION) { \
        PHPRAY_G(orig_field) = func->internal_function.handler; \
        func->internal_function.handler = zif_phpray_redis_##hook_func; \
    } \
} while(0)

/* Macro for restoring a Redis method */
#define UNHOOK_REDIS_METHOD(ce, method, orig_field) do { \
    if (PHPRAY_G(orig_field)) { \
        zend_function *func = (zend_function *)zend_hash_str_find_ptr( \
            &(ce)->function_table, method, sizeof(method) - 1); \
        if (func && func->type == ZEND_INTERNAL_FUNCTION) { \
            func->internal_function.handler = PHPRAY_G(orig_field); \
        } \
        PHPRAY_G(orig_field) = NULL; \
    } \
} while(0)

static zend_bool hooks_redis_installed = 0;

void phpray_hooks_redis_init(void) {
    zend_class_entry *ce;

    if (hooks_redis_installed) return;
    hooks_redis_installed = 1;

    /* Look up Redis class from phpredis extension */
    zend_string *redis_name = zend_string_init("redis", sizeof("redis") - 1, 0);
    ce = (zend_class_entry *)zend_hash_find_ptr(CG(class_table), redis_name);
    zend_string_release(redis_name);

    if (!ce) return; /* phpredis not loaded */

    HOOK_REDIS_METHOD(ce, "get",    orig_redis_get,    get);
    HOOK_REDIS_METHOD(ce, "set",    orig_redis_set,    set);
    HOOK_REDIS_METHOD(ce, "del",    orig_redis_del,    del);
    HOOK_REDIS_METHOD(ce, "mget",   orig_redis_mget,   mget);
    HOOK_REDIS_METHOD(ce, "setex",  orig_redis_setex,  setex);
    HOOK_REDIS_METHOD(ce, "exists", orig_redis_exists, exists);
    HOOK_REDIS_METHOD(ce, "hget",   orig_redis_hget,   hget);
    HOOK_REDIS_METHOD(ce, "hset",   orig_redis_hset,   hset);
    HOOK_REDIS_METHOD(ce, "hdel",   orig_redis_hdel,   hdel);
    HOOK_REDIS_METHOD(ce, "incr",   orig_redis_incr,   incr);
    HOOK_REDIS_METHOD(ce, "lpush",  orig_redis_lpush,  lpush);
    HOOK_REDIS_METHOD(ce, "rpush",  orig_redis_rpush,  rpush);
    HOOK_REDIS_METHOD(ce, "expire", orig_redis_expire, expire);
    HOOK_REDIS_METHOD(ce, "ttl",    orig_redis_ttl,    ttl);
    HOOK_REDIS_METHOD(ce, "ping",   orig_redis_ping,   ping);
}

void phpray_hooks_redis_shutdown(void) {
    zend_class_entry *ce;

    if (!hooks_redis_installed) return;
    hooks_redis_installed = 0;

    zend_string *redis_name = zend_string_init("redis", sizeof("redis") - 1, 0);
    ce = (zend_class_entry *)zend_hash_find_ptr(CG(class_table), redis_name);
    zend_string_release(redis_name);

    if (!ce) return;

    UNHOOK_REDIS_METHOD(ce, "get",    orig_redis_get);
    UNHOOK_REDIS_METHOD(ce, "set",    orig_redis_set);
    UNHOOK_REDIS_METHOD(ce, "del",    orig_redis_del);
    UNHOOK_REDIS_METHOD(ce, "mget",   orig_redis_mget);
    UNHOOK_REDIS_METHOD(ce, "setex",  orig_redis_setex);
    UNHOOK_REDIS_METHOD(ce, "exists", orig_redis_exists);
    UNHOOK_REDIS_METHOD(ce, "hget",   orig_redis_hget);
    UNHOOK_REDIS_METHOD(ce, "hset",   orig_redis_hset);
    UNHOOK_REDIS_METHOD(ce, "hdel",   orig_redis_hdel);
    UNHOOK_REDIS_METHOD(ce, "incr",   orig_redis_incr);
    UNHOOK_REDIS_METHOD(ce, "lpush",  orig_redis_lpush);
    UNHOOK_REDIS_METHOD(ce, "rpush",  orig_redis_rpush);
    UNHOOK_REDIS_METHOD(ce, "expire", orig_redis_expire);
    UNHOOK_REDIS_METHOD(ce, "ttl",    orig_redis_ttl);
    UNHOOK_REDIS_METHOD(ce, "ping",   orig_redis_ping);
}
