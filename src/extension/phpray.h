/*
 * PHPRay — PHP Request Tracing Extension
 * 
 * Lightweight per-request profiling for shared hosting.
 * Hooks into PHP request lifecycle, MySQL queries, curl calls,
 * and filesystem operations. The per-component function profile is a
 * separately sampled layer built on zend_observer (see hooks_profiler.c).
 */

#ifndef PHPRAY_H
#define PHPRAY_H

#ifdef HAVE_CONFIG_H
#include "config.h"
#endif

#include "php.h"
#include "php_ini.h"
#include "ext/standard/info.h"

#include <stdint.h>
#include <time.h>
#include <sys/types.h>
#include <unistd.h>

/* Extension name and version */
#define PHPRAY_NAME    "phpray"
#define PHPRAY_VERSION "0.15.5"

/* Limits */
#define PHPRAY_MAX_URI_LEN        1024
#define PHPRAY_MAX_DOCROOT_LEN    512   /* DOCUMENT_ROOT (site identity: sha1(host + "\0" + docroot)) */

/* Application detected at RSHUTDOWN (cheap constant lookups, no code hooks) */
#define PHPRAY_APP_UNKNOWN    0
#define PHPRAY_APP_WORDPRESS  1
#define PHPRAY_APP_PRESTASHOP 2
#define PHPRAY_APP_LARAVEL    3
#define PHPRAY_APP_MAGENTO    4
#define PHPRAY_APP_SYMFONY    5
#define PHPRAY_MAX_SERVER_NAME    256
#define PHPRAY_MAX_METHOD_LEN     16
#define PHPRAY_MAX_SQL_LEN        2048
#define PHPRAY_MAX_URL_LEN        1024
#define PHPRAY_MAX_PATH_LEN       512
#define PHPRAY_MAX_QUERIES        100
#define PHPRAY_MAX_HTTP_CALLS     50
#define PHPRAY_MAX_FILE_OPS       200
#define PHPRAY_MAX_MARKS          50
#define PHPRAY_MAX_MARK_NAME      64
#define PHPRAY_MAX_ERRORS         20
#define PHPRAY_MAX_ERROR_MSG      256
#define PHPRAY_MAX_REQUEST_ID     128
#define PHPRAY_MAX_FUNC_NAME      128  /* max length of a component name */

/* Function profiler (zend_observer, hooks_profiler.c) */
#define PHPRAY_PROFILE_OFF        0
#define PHPRAY_PROFILE_SAMPLE     1
#define PHPRAY_PROFILE_URL        2
#define PHPRAY_PROFILE_ALL        3
#define PHPRAY_PROFILE_STACK      256   /* frames tracked for self time; deeper → inclusive only */
#define PHPRAY_PROFILE_MIN_COMPONENTS 2
#define PHPRAY_PROFILE_MAX_COMPONENTS 1024
#define PHPRAY_PROFILE_DEFAULT_COMPONENTS 64
#define PHPRAY_PROFILE_OTHER_NAME "other"
#define PHPRAY_CLASS_CACHE_SLOTS  4096  /* per-process filename → component cache (open addressing) */
#define PHPRAY_CLASS_CACHE_PROBES 8
#define PHPRAY_NAMES_MAX          1024  /* per-process component name table; overflow → "other" */
#define PHPRAY_PIDX_NONE          0xFFFF /* cached verdict: file is not a component */
#define PHPRAY_MAX_MULTI_HANDLES  32  /* max concurrent curl_multi easy handles tracked */
#define PHPRAY_MAX_BACKTRACE_FRAMES 5  /* max stack frames captured per slow query/HTTP call */
#define PHPRAY_MAX_FRAME_FILE   128    /* max length of file path per stack frame */

/* Threshold for capturing backtrace (ns) */
#define PHPRAY_SLOW_QUERY_BACKTRACE_NS  50000000ULL   /* 50ms — capture backtrace for queries slower than this */
#define PHPRAY_SLOW_HTTP_BACKTRACE_NS   500000000ULL  /* 500ms — capture backtrace for HTTP calls slower than this */

/* Crash recovery health file (default of phpray.health_path; may contain %u / %g) */
#define PHPRAY_HEALTH_FILE      "/tmp/phpray_health"
#define PHPRAY_CRASH_THRESHOLD  3
#define PHPRAY_CRASH_WINDOW_SEC 60
#define PHPRAY_MAX_CRASH_SLOTS  8
#define PHPRAY_HEALTH_MAGIC     0x50485279  /* "PHRy" */

/* Default config values */
#define PHPRAY_DEFAULT_OUTPUT_PATH "/tmp/phpray.jsonl"
#define PHPRAY_DEFAULT_SHM_PATH    "/dev/shm/phpray"
#define PHPRAY_DEFAULT_SHM_SIZE    33554432  /* 32MB */
#define PHPRAY_DEFAULT_CONTROL_PATH "/dev/shm/phpray-control"

/*
 * Path templates: phpray.shm_path and phpray.health_path may contain "%u"
 * (effective uid) and "%g" (effective gid). A ring path with a placeholder is
 * private to that identity: the file is created 0600 (%u) / 0660 (%g only)
 * and attached lazily by every worker after it has switched to its user, so on
 * shared hosting (CageFS, PHP-FPM pools, mod_ruid2) customers cannot read each
 * other's traces; the collector reads all of them as root (shm_path = ".../ring-*").
 */
#define PHPRAY_PATH_HAS_UID 1
#define PHPRAY_PATH_HAS_GID 2
#define PHPRAY_RING_RETRY_SEC 5   /* after a failed per-user attach: next try no sooner than this */

/* Expand %u / %g / %% in tmpl into out. Returns PHPRAY_PATH_HAS_* flags (0 = no
 * placeholder) or -1 when the result does not fit. (ringbuffer.c) */
int phpray_path_expand(const char *tmpl, char *out, size_t outsz, uid_t uid, gid_t gid);

/* Effective (expanded) health file path of this process. (crash_recovery.c) */
const char *phpray_health_path(void);

/* Output modes */
#define PHPRAY_OUTPUT_FILE 0
#define PHPRAY_OUTPUT_SHM  1
#define PHPRAY_OUTPUT_BOTH 2

/* Smart sampling trace levels (determined at RSHUTDOWN based on duration) */
#define PHPRAY_TRACE_SUMMARY  0  /* <200ms: only timing + URI + status */
#define PHPRAY_TRACE_NORMAL   1  /* 200ms-1s: + queries, curl calls */
#define PHPRAY_TRACE_FULL     2  /* >1s: + all queries, backtrace, full breakdown */
#define PHPRAY_TRACE_ALERT    3  /* >3s: full + trigger alert */

/* Smart sampling default thresholds (ms) */
#define PHPRAY_DEFAULT_THRESHOLD_NORMAL  200
#define PHPRAY_DEFAULT_THRESHOLD_FULL    1000
#define PHPRAY_DEFAULT_THRESHOLD_ALERT   3000

/* ---- Data structures ---- */

/* User-defined mark (phpray_mark() calls) */
typedef struct {
    char     name[PHPRAY_MAX_MARK_NAME];
    uint64_t offset_ns;   /* nanoseconds since request start */
} phpray_mark_t;

/* Captured PHP error/warning */
typedef struct {
    int      type;         /* E_WARNING, E_NOTICE, E_ERROR, etc. */
    char     message[PHPRAY_MAX_ERROR_MSG];
    uint64_t offset_ns;   /* nanoseconds since request start */
} phpray_error_t;

/* Stack frame for backtrace capture */
typedef struct {
    char     file[PHPRAY_MAX_FRAME_FILE]; /* source file path (abbreviated) */
    uint32_t line;                         /* line number */
} phpray_frame_t;

/* Captured database query */
typedef struct {
    char     sql[PHPRAY_MAX_SQL_LEN];   /* SQL text (truncated) */
    uint64_t duration_ns;               /* execution time */
    uint64_t offset_ns;                 /* offset from request start */
    uint32_t affected_rows;             /* rows affected/returned */
    uint8_t  source;                    /* 0=mysqli, 1=pdo */
    uint8_t  bt_count;                  /* number of backtrace frames captured */
    phpray_frame_t bt[PHPRAY_MAX_BACKTRACE_FRAMES]; /* backtrace frames (only for slow queries) */
} phpray_query_t;

/* Captured HTTP call (curl) */
typedef struct {
    char     url[PHPRAY_MAX_URL_LEN];   /* effective URL */
    uint64_t duration_ns;               /* execution time */
    uint64_t offset_ns;                 /* offset from request start */
    uint16_t response_code;             /* HTTP response code */
    uint8_t  bt_count;                  /* number of backtrace frames captured */
    phpray_frame_t bt[PHPRAY_MAX_BACKTRACE_FRAMES]; /* backtrace frames (only for slow calls) */
} phpray_http_call_t;

/* Tracking entry for curl_multi easy handles (in-flight async transfers) */
typedef struct {
    void            *easy_handle;       /* zval pointer of CurlHandle for identity comparison */
    char             url[PHPRAY_MAX_URL_LEN];
    struct timespec  start_time;        /* when curl_multi_add_handle was called */
    uint8_t          active;            /* 1 if slot in use */
} phpray_multi_track_t;

/* One component of the function profile (plugin, theme, mu-plugin or composer package) */
typedef struct {
    char     name[PHPRAY_MAX_FUNC_NAME]; /* "plugins/woocommerce", "themes/storefront", "vendor/a/b" */
    uint64_t incl_ns;   /* wall time while the component is on the stack (counted once per boundary) */
    uint64_t self_ns;   /* wall time in the component's own frames, excluding nested observed frames */
    uint64_t enter_ns;  /* timestamp of the outermost entry (valid while depth > 0) */
    uint32_t calls;     /* observed calls (function bodies, includes, generator resumes) */
    uint32_t depth;     /* nesting depth within this component (recursion guard) */
} phpray_comp_t;

/* One open observed frame (for self time) */
typedef struct {
    zend_execute_data *ed;   /* frame identity — verified at end() (generators/fibers unwind out of order) */
    uint64_t start_ns;
    uint64_t child_ns;       /* time spent in nested observed frames */
    uint16_t comp;
} phpray_pframe_t;

/* Per-process classification cache entry: (hash, len) of op_array->filename → name index.
 * Keyed by content hash rather than by pointer: OPcache restarts recycle SHM addresses
 * and non-cached scripts get a fresh zend_string every request. For interned (OPcache)
 * strings the hash is precomputed, so a hit costs a few loads and no path scan. */
typedef struct {
    zend_ulong h;
    uint32_t   len;
    uint16_t   pidx;         /* index into names[] or PHPRAY_PIDX_NONE */
    uint16_t   used;
} phpray_class_slot_t;

typedef struct {
    phpray_class_slot_t slots[PHPRAY_CLASS_CACHE_SLOTS];
    char     (*names)[PHPRAY_MAX_FUNC_NAME]; /* PHPRAY_NAMES_MAX entries, last = "other" */
    uint16_t   name_count;
    uint32_t   hits, misses, evictions, name_overflow;
} phpray_class_cache_t;

/* Per-request profile state — allocated only for profiled (or url-pending) requests */
typedef struct _phpray_profile_t {
    phpray_comp_t *comps;    /* comp_max entries; last one reserved for "other" */
    uint16_t *map;           /* PHPRAY_NAMES_MAX entries: process name index → comp index + 1 (0 = unused) */
    uint16_t comp_count;
    uint16_t comp_max;
    uint8_t  on;             /* 1 = handlers are collecting */
    uint8_t  pending;        /* 1 = url mode, decision deferred to the first observed call */
    uint8_t  overflow;       /* 1 = frame stack overflowed; self times approximate */
    uint32_t stack_len;      /* logical depth (may exceed PHPRAY_PROFILE_STACK) */
    /* on-demand profiling from the control table (overrides profile_mode for this request) */
    uint8_t  ctl;            /* 1 = decision comes from a control-table entry */
    uint16_t ctl_rate;       /* % of matching requests to profile */
    char     ctl_prefix[136];/* URI prefix ("" = every URI) */
    phpray_pframe_t stack[PHPRAY_PROFILE_STACK];
} phpray_profile_t;

/* Per-request trace data (built during request, flushed at RSHUTDOWN) */
typedef struct {
    /* Request identity */
    uint64_t request_id;
    uint32_t uid;
    uint32_t pid;
    char     phpray_id[PHPRAY_MAX_REQUEST_ID]; /* unique request ID for correlation */
    
    /* Timing */
    struct timespec start_time;
    struct timespec end_time;
    uint64_t duration_ns;
    uint64_t cpu_user_ns;
    uint64_t cpu_sys_ns;
    
    /* Request info */
    char     server_name[PHPRAY_MAX_SERVER_NAME];
    char     request_uri[PHPRAY_MAX_URI_LEN];
    char     request_method[PHPRAY_MAX_METHOD_LEN];
    char     docroot[PHPRAY_MAX_DOCROOT_LEN];   /* DOCUMENT_ROOT (RSHUTDOWN: sapi_getenv → $_SERVER → dirname(script)) */
    uint16_t response_code;
    uint64_t memory_peak;
    
    /* Counters (Phase 0: just counters, no details) */
    uint16_t query_count;
    uint64_t db_total_ns;
    uint16_t http_call_count;
    uint64_t http_total_ns;
    uint16_t file_op_count;
    uint64_t file_total_ns;
    uint16_t redis_call_count;
    uint64_t redis_total_ns;

    /* WordPress detection */
    uint8_t  wp_detected;
    uint8_t  app;            /* PHPRAY_APP_* detected at RSHUTDOWN by well-known constants */
    
    /* Smart sampling — determined at RSHUTDOWN */
    uint8_t  is_sampled;     /* 1 if this request is being traced */
    uint8_t  trace_level;    /* 0=summary, 1=normal, 2=full, 3=alert */
    
    /* User-defined marks (phpray_mark() calls) */
    phpray_mark_t marks[PHPRAY_MAX_MARKS];
    uint16_t      mark_count;
    
    /* Captured PHP errors/warnings */
    phpray_error_t errors[PHPRAY_MAX_ERRORS];
    uint16_t       error_count;
    
    /* N+1 query detection */
    uint8_t  n_plus_one;     /* 1 if N+1 query pattern detected */
    
    /* Function-level profiling (per-component, zend_observer) — see hooks_profiler.c.
     * profiled = 1 when this request had the profile switched on (its components
     * section is complete); prof is NULL for non-profiled requests. */
    uint8_t                  profiled;
    struct _phpray_profile_t *prof;
    
    /* Captured database queries */
    phpray_query_t *queries;       /* dynamically allocated when needed */
    uint16_t        queries_alloc; /* allocated capacity */
    
    /* Captured HTTP calls */
    phpray_http_call_t *http_calls;     /* dynamically allocated when needed */
    uint16_t            http_calls_alloc;

    /* curl_multi tracking: in-flight async transfers */
    phpray_multi_track_t multi_tracks[PHPRAY_MAX_MULTI_HANDLES];
    uint8_t              multi_track_count;
    
} phpray_request_t;

/* ---- Module globals ---- */

ZEND_BEGIN_MODULE_GLOBALS(phpray)
    /* Configuration (php.ini) */
    zend_bool master_switch;            /* PHP_INI_SYSTEM: 0 = extension stays inert (no hooks at all) */
    zend_bool enabled;
    char     *mode;                     /* "smart", "all", "manual" */
    zend_long threshold_normal_ms;      /* smart mode: threshold for normal trace (ms) */
    zend_long threshold_full_ms;        /* smart mode: threshold for full trace (ms) */
    zend_long threshold_alert_ms;       /* smart mode: threshold for alert (ms) */
    zend_long manual_sample_rate;       /* manual mode: % of requests (0-100) */
    zend_bool always_trace_errors;      /* always full-trace 5xx responses */
    char     *output_path;              /* Phase 0: output file path */
    char     *ignore_uris;             /* comma-separated list of URI prefixes to ignore */
    zend_bool trace_cli;               /* trace CLI SAPI requests (default: off) */
    zend_bool emit_header;             /* emit X-PHPRay-ID response header */
    zend_bool capture_errors;          /* capture PHP errors/warnings per request */

    /* Ring buffer / shared memory config (Phase 1) */
    char     *shm_path;                /* shared memory file path (template: %u / %g allowed) */
    zend_long shm_size;                /* shared memory size in bytes */
    char     *output_mode;             /* "file", "shm", "both" */
    char     *control_path;            /* SYSTEM: shared-memory control table written by the collector */
    char     *health_path;             /* SYSTEM: crash-recovery health file (template: %u / %g allowed) */
    uint64_t  control_hits;            /* requests whose profile decision came from the control table */

    /* Per-identity ring (shm_path contains %u / %g): attached lazily, re-attached
     * when the effective uid/gid of the process changes (mod_ruid2 style SAPIs) */
    zend_bool ring_per_user;
    int       ring_path_flags;         /* PHPRAY_PATH_HAS_* of shm_path */
    uid_t     ring_uid;                /* identity the attached ring belongs to */
    gid_t     ring_gid;
    time_t    ring_retry_at;           /* no attach attempt before this time (after a failure) */

    /* Runtime state */
    phpray_request_t *current_request;
    uint64_t          request_counter;

    /* JSONL output: fd opened with O_APPEND; each record is one write() (no interleaving
     * between prefork/FPM workers for records the kernel writes in one go) */
    int    output_fd;
    char  *jsonl_buf;                  /* persistent record buffer (grows as needed) */
    size_t jsonl_cap;
    uint64_t jsonl_records, jsonl_errors;

    /* Ring buffer handle (Phase 1) — void* to avoid including ringbuffer.h */
    void *ring;
    int   output_mode_id;              /* cached: PHPRAY_OUTPUT_FILE/SHM/BOTH */

    /* Original error handler (for error hooking) */
    void (*original_error_cb)(int type, zend_string *error_filename,
                              const uint32_t error_lineno, zend_string *message);
    
    /* Function profiler (zend_observer) */
    zend_bool profile_functions;       /* SYSTEM: register the observer at MINIT at all */
    char     *profile_mode;            /* PERDIR: "off" | "sample" | "url" | "all" */
    int       profile_mode_id;         /* cached PHPRAY_PROFILE_* (set by the INI handler) */
    zend_long profile_sample_rate;     /* PERDIR: % of requests profiled in sample mode */
    char     *profile_url;             /* PERDIR: comma-separated URI prefixes for url mode */
    zend_long profile_max_components;  /* SYSTEM: components per request, excess → "other" */
    zend_bool profile_observer_registered; /* observer registered in this process */
    phpray_profile_t *profile;         /* hot path: active profile of the current request or NULL */
    phpray_class_cache_t *class_cache; /* per-process (per-thread under ZTS) filename classification cache */
    uint64_t  rng_state;               /* xorshift64* state for profile sampling */
    uint32_t  rng_pid;                 /* pid the RNG was seeded for (re-seed after fork) */

    /* Crash recovery config */
    zend_long crash_threshold;          /* crashes within window to trigger disable */
    zend_long crash_window;             /* crash window in seconds */

    /* Original function handlers (for MySQL/curl hooking) */
    zif_handler orig_mysqli_query;
    zif_handler orig_pdo_query;
    zif_handler orig_pdo_exec;
    zif_handler orig_pdo_stmt_execute;
    zif_handler orig_curl_exec;
    zif_handler orig_curl_multi_add_handle;
    zif_handler orig_curl_multi_remove_handle;
    zif_handler orig_curl_multi_info_read;

    /* Redis hooks */
    zif_handler orig_redis_get;
    zif_handler orig_redis_set;
    zif_handler orig_redis_del;
    zif_handler orig_redis_mget;
    zif_handler orig_redis_setex;
    zif_handler orig_redis_exists;
    zif_handler orig_redis_hget;
    zif_handler orig_redis_hset;
    zif_handler orig_redis_hdel;
    zif_handler orig_redis_incr;
    zif_handler orig_redis_lpush;
    zif_handler orig_redis_rpush;
    zif_handler orig_redis_expire;
    zif_handler orig_redis_ttl;
    zif_handler orig_redis_ping;

ZEND_END_MODULE_GLOBALS(phpray)

ZEND_EXTERN_MODULE_GLOBALS(phpray)

#define PHPRAY_G(v) ZEND_MODULE_GLOBALS_ACCESSOR(phpray, v)

#if defined(ZTS) && defined(COMPILE_DL_PHPRAY)
ZEND_TSRMLS_CACHE_EXTERN()
#endif

/* Hook lifecycle functions */
void phpray_hooks_mysql_init(void);
void phpray_hooks_mysql_shutdown(void);
void phpray_hooks_curl_init(void);
void phpray_hooks_curl_shutdown(void);
void phpray_hooks_file_init(void);
void phpray_hooks_file_shutdown(void);
void phpray_profiler_init(void);                                  /* MINIT: register observer */
void phpray_profiler_shutdown(void);                              /* MSHUTDOWN */
void phpray_profiler_request_init(phpray_request_t *req);         /* RINIT: per-request decision */
void phpray_profiler_request_finish(phpray_request_t *req);       /* RSHUTDOWN: close frames, sort */
void phpray_profiler_request_free(phpray_request_t *req);         /* RSHUTDOWN: release memory */
void phpray_profiler_globals_free(void);                          /* GSHUTDOWN: free the class cache */
int  phpray_profile_parse_mode(const char *value, size_t len);    /* -1 = invalid */
const char *phpray_profile_mode_name(int mode);
void phpray_hooks_redis_init(void);
void phpray_hooks_redis_shutdown(void);

/* Helper: record a query in the current request */
void phpray_record_query(const char *sql, size_t sql_len, uint64_t duration_ns, 
                         uint32_t affected_rows, uint8_t source);

/* Helper: record an HTTP call in the current request */
void phpray_record_http_call(const char *url, uint64_t duration_ns, uint16_t response_code);

/* Helper: capture PHP backtrace into frame array (returns number of frames captured) */
uint8_t phpray_capture_backtrace(phpray_frame_t *frames, uint8_t max_frames);

/* Crash recovery lifecycle functions */
int  phpray_health_init(void);
int  phpray_health_check(void);
void phpray_health_install_handlers(void);
void phpray_health_shutdown(void);
void phpray_health_reset(void);
void phpray_health_get_status(uint32_t *crashes, uint32_t *disabled,
                               int64_t *last_crash, int64_t *last_reset_out);

/* Utility */
static inline uint64_t phpray_time_diff_ns(struct timespec *start, struct timespec *end) {
    return ((uint64_t)(end->tv_sec - start->tv_sec) * 1000000000ULL) +
           ((uint64_t)end->tv_nsec - (uint64_t)start->tv_nsec);
}

#endif /* PHPRAY_H */
