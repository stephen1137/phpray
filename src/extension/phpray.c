/*
 * PHPRay — PHP Request Tracing Extension
 * 
 * Phase 1: Core Extension (in progress)
 * - Hooks request lifecycle (RINIT/RSHUTDOWN)
 * - Captures: URI, method, response code, duration, memory peak, UID, PID
 * - Smart sampling: decides trace level (summary/normal/full/alert) at RSHUTDOWN
 * - Dual output: JSON lines (/tmp/phpray.jsonl) + MPSC shared memory ring buffer
 *   (one ring per server, or one private 0600 ring per user when phpray.shm_path
 *   contains %u — multi-tenant hosting, CloudLinux/CageFS)
 * - MySQL/PDO hooking: mysqli_query, PDO::query, PDO::exec, PDOStatement::execute
 * - curl hooking: curl_exec with URL + response code capture
 * - File I/O hooking: file_get_contents (with URL detection), file_put_contents
 * - phpray_mark() userland function for custom profiling marks
 * - X-PHPRay-ID response header for request correlation
 * - PHP error/warning capture per request
 * - CLI SAPI filtering, URI query string fingerprinting
 * - WordPress / application auto-detection
 * - Function profile per component (zend_observer, sampled separately — hooks_profiler.c)
 */

#ifdef HAVE_CONFIG_H
#include "config.h"
#endif

#include "phpray.h"
#include "ringbuffer.h"
#include "control.h"
#include "SAPI.h"
#include "zend_builtin_functions.h"

#include <stdio.h>
#include <stdlib.h>
#include <stdarg.h>
#include <string.h>
#include <time.h>
#include <errno.h>
#include <fcntl.h>
#include <unistd.h>
#include <limits.h>
#include <sys/resource.h>

#ifndef PATH_MAX
#define PATH_MAX 4096
#endif

/* Module globals */
ZEND_DECLARE_MODULE_GLOBALS(phpray)

/* ---- INI handler: phpray.profile_mode = off | sample | url | all ---- */
static PHP_INI_MH(OnUpdateProfileMode) {
    int id = phpray_profile_parse_mode(new_value ? ZSTR_VAL(new_value) : NULL,
                                       new_value ? ZSTR_LEN(new_value) : 0);
    if (id < 0) {
        return FAILURE;   /* unknown value: keep the previous one */
    }
    if (OnUpdateString(entry, new_value, mh_arg1, mh_arg2, mh_arg3, stage) != SUCCESS) {
        return FAILURE;
    }
    PHPRAY_G(profile_mode_id) = id;
    return SUCCESS;
}

/* ---- INI entries ---- */

PHP_INI_BEGIN()
    /* Master switch (process level, PHP_INI_SYSTEM). 0 = the extension loads but
     * installs nothing: no observer, no handler replacement, no signal handlers.
     * Shared hosting installs set it to 0 globally and to 1 only for the accounts
     * that opted in — phpray.enabled alone is PHP_INI_PERDIR and therefore cannot
     * keep MINIT from touching a process (h1 incident, 2026-09-20). */
    STD_PHP_INI_BOOLEAN("phpray.master_switch",        "1",
        PHP_INI_SYSTEM, OnUpdateBool,   master_switch,        zend_phpray_globals, phpray_globals)
    STD_PHP_INI_BOOLEAN("phpray.enabled",             "1",
        PHP_INI_PERDIR, OnUpdateBool,   enabled,              zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.mode",                  "smart",
        PHP_INI_PERDIR, OnUpdateString, mode,                 zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.smart_threshold_normal", "200",
        PHP_INI_PERDIR, OnUpdateLong,   threshold_normal_ms,  zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.smart_threshold_full",   "1000",
        PHP_INI_PERDIR, OnUpdateLong,   threshold_full_ms,    zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.smart_threshold_alert",  "3000",
        PHP_INI_PERDIR, OnUpdateLong,   threshold_alert_ms,   zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.manual_sample_rate",     "10",
        PHP_INI_PERDIR, OnUpdateLong,   manual_sample_rate,   zend_phpray_globals, phpray_globals)
    STD_PHP_INI_BOOLEAN("phpray.always_trace_errors",  "1",
        PHP_INI_PERDIR, OnUpdateBool,   always_trace_errors,  zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.output_path",           PHPRAY_DEFAULT_OUTPUT_PATH,
        PHP_INI_SYSTEM, OnUpdateString, output_path,          zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.ignore_uris",           "",
        PHP_INI_PERDIR, OnUpdateString, ignore_uris,          zend_phpray_globals, phpray_globals)
    STD_PHP_INI_BOOLEAN("phpray.trace_cli",            "0",
        PHP_INI_SYSTEM, OnUpdateBool,   trace_cli,            zend_phpray_globals, phpray_globals)
    STD_PHP_INI_BOOLEAN("phpray.emit_header",          "1",
        PHP_INI_PERDIR, OnUpdateBool,   emit_header,          zend_phpray_globals, phpray_globals)
    STD_PHP_INI_BOOLEAN("phpray.capture_errors",       "1",
        PHP_INI_PERDIR, OnUpdateBool,   capture_errors,       zend_phpray_globals, phpray_globals)
    /* shm_path may contain %u (effective uid) and %g (effective gid). Without a
     * placeholder one ring is created at MINIT (0666 minus umask, as before). With
     * one, each worker attaches its own ring lazily at the first traced request —
     * after PHP-FPM/lsphp has switched to the site's user — created 0600 (%u) or
     * 0660 (%g only), so customers of a shared server cannot read each other's
     * traces; the collector (root) reads them all, e.g. "/run/phpray/ring-*". On
     * CloudLinux/CageFS put the ring in a directory shared with the cages
     * (/run/phpray via cagefs.mp), never in /dev/shm (private per cage). */
    STD_PHP_INI_ENTRY("phpray.shm_path",              PHPRAY_DEFAULT_SHM_PATH,
        PHP_INI_SYSTEM, OnUpdateString, shm_path,             zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.shm_size",              "33554432",
        PHP_INI_SYSTEM, OnUpdateLong,   shm_size,             zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.output_mode",           "file",
        PHP_INI_SYSTEM, OnUpdateString, output_mode,          zend_phpray_globals, phpray_globals)
    STD_PHP_INI_BOOLEAN("phpray.profile_functions",    "1",
        PHP_INI_SYSTEM, OnUpdateBool,   profile_functions,    zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.profile_mode",          "sample",
        PHP_INI_PERDIR, OnUpdateProfileMode, profile_mode,    zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.profile_sample_rate",   "3",
        PHP_INI_PERDIR, OnUpdateLong,   profile_sample_rate,  zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.profile_url",           "",
        PHP_INI_PERDIR, OnUpdateString, profile_url,          zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.profile_max_components", "64",
        PHP_INI_SYSTEM, OnUpdateLong,   profile_max_components, zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.control_path",          PHPRAY_DEFAULT_CONTROL_PATH,
        PHP_INI_SYSTEM, OnUpdateString, control_path,         zend_phpray_globals, phpray_globals)
    /* Crash-recovery health file; same %u / %g placeholders as shm_path (expanded
     * at MINIT: the site's user under lsphp/CGI, the master under PHP-FPM). With
     * %u the file is created 0600 and only used when owned by that uid. Under
     * CageFS the default in /tmp already lands in the user's private /tmp. */
    STD_PHP_INI_ENTRY("phpray.health_path",           PHPRAY_HEALTH_FILE,
        PHP_INI_SYSTEM, OnUpdateString, health_path,          zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.crash_threshold",       "3",
        PHP_INI_SYSTEM, OnUpdateLong,   crash_threshold,      zend_phpray_globals, phpray_globals)
    STD_PHP_INI_ENTRY("phpray.crash_window",          "60",
        PHP_INI_SYSTEM, OnUpdateLong,   crash_window,         zend_phpray_globals, phpray_globals)
PHP_INI_END()

/* ---- Application detection (RSHUTDOWN only; constant/class lookups cost ~100 ns) ---- */
static const char *phpray_app_name(uint8_t app) {
    switch (app) {
        case PHPRAY_APP_WORDPRESS:  return "wordpress";
        case PHPRAY_APP_PRESTASHOP: return "prestashop";
        case PHPRAY_APP_LARAVEL:    return "laravel";
        case PHPRAY_APP_MAGENTO:    return "magento";
        case PHPRAY_APP_SYMFONY:    return "symfony";
        default:                    return "-";
    }
}

static uint8_t phpray_detect_app(void) {
    if (zend_get_constant_str(ZEND_STRL("WPINC")) || zend_get_constant_str(ZEND_STRL("ABSPATH"))) {
        return PHPRAY_APP_WORDPRESS;
    }
    if (zend_get_constant_str(ZEND_STRL("_PS_VERSION_"))) {
        return PHPRAY_APP_PRESTASHOP;
    }
    if (zend_get_constant_str(ZEND_STRL("LARAVEL_START"))) {
        return PHPRAY_APP_LARAVEL;
    }
    if (zend_hash_str_exists(CG(class_table), ZEND_STRL("magento\\framework\\app\\bootstrap"))) {
        return PHPRAY_APP_MAGENTO;
    }
    if (zend_hash_str_exists(CG(class_table), ZEND_STRL("symfony\\component\\httpkernel\\kernel"))) {
        return PHPRAY_APP_SYMFONY;
    }
    return PHPRAY_APP_UNKNOWN;
}

/* ---- Helper: capture PHP backtrace into frame array ---- */
/* Uses zend_fetch_debug_backtrace to get the current PHP call stack.
 * Returns top N frames (skipping internal/hook frames).
 * Each frame contains an abbreviated file path and line number.
 * Cost: ~1-5μs — only called for slow queries (>50ms) and slow HTTP calls (>500ms). */
uint8_t phpray_capture_backtrace(phpray_frame_t *frames, uint8_t max_frames) {
    zval trace;
    uint8_t count = 0;
    
    if (!frames || max_frames == 0) return 0;
    
    /* Fetch backtrace — skip_last=0, provide_object=0 (lightweight) */
    zend_fetch_debug_backtrace(&trace, 0, DEBUG_BACKTRACE_IGNORE_ARGS, max_frames + 3);
    
    if (Z_TYPE(trace) != IS_ARRAY) {
        zval_ptr_dtor(&trace);
        return 0;
    }
    
    zval *frame_zv;
    ZEND_HASH_FOREACH_VAL(Z_ARRVAL(trace), frame_zv) {
        if (count >= max_frames) break;
        if (Z_TYPE_P(frame_zv) != IS_ARRAY) continue;
        
        /* Get file */
        zval *file_zv = zend_hash_str_find(Z_ARRVAL_P(frame_zv), "file", sizeof("file") - 1);
        zval *line_zv = zend_hash_str_find(Z_ARRVAL_P(frame_zv), "line", sizeof("line") - 1);
        
        if (!file_zv || Z_TYPE_P(file_zv) != IS_STRING) continue;
        
        const char *file = Z_STRVAL_P(file_zv);
        
        /* Skip internal phpray frames */
        if (strstr(file, "phpray") != NULL) continue;
        
        /* Abbreviate path: keep from public_html/ or wp-content/ or vendor/ onwards */
        const char *abbrev = file;
        const char *p;
        if ((p = strstr(file, "/public_html/")) != NULL) {
            abbrev = p + 13; /* skip "/public_html/" */
        } else if ((p = strstr(file, "/wp-content/")) != NULL) {
            abbrev = p + 1; /* keep "wp-content/" */
        } else if ((p = strstr(file, "/vendor/")) != NULL) {
            abbrev = p + 1; /* keep "vendor/" */
        } else if ((p = strstr(file, "/htdocs/")) != NULL) {
            abbrev = p + 8;
        } else if ((p = strstr(file, "/www/")) != NULL) {
            abbrev = p + 5;
        } else {
            /* Last resort: last 2 path components */
            size_t flen = strlen(file);
            const char *end = file + flen;
            const char *slash1 = end - 1;
            while (slash1 > file && *slash1 != '/') slash1--;
            const char *slash2 = slash1 - 1;
            while (slash2 > file && *slash2 != '/') slash2--;
            if (slash2 > file) {
                abbrev = slash2 + 1;
            }
        }
        
        strncpy(frames[count].file, abbrev, PHPRAY_MAX_FRAME_FILE - 1);
        frames[count].file[PHPRAY_MAX_FRAME_FILE - 1] = '\0';
        frames[count].line = line_zv && Z_TYPE_P(line_zv) == IS_LONG ? (uint32_t)Z_LVAL_P(line_zv) : 0;
        count++;
    } ZEND_HASH_FOREACH_END();
    
    zval_ptr_dtor(&trace);
    return count;
}

/* ---- Helper: get CPU time for current process ---- */
static void phpray_get_cpu_time(uint64_t *user_ns, uint64_t *sys_ns) {
    struct rusage ru;
    if (getrusage(RUSAGE_SELF, &ru) == 0) {
        *user_ns = (uint64_t)ru.ru_utime.tv_sec * 1000000000ULL + 
                   (uint64_t)ru.ru_utime.tv_usec * 1000ULL;
        *sys_ns  = (uint64_t)ru.ru_stime.tv_sec * 1000000000ULL + 
                   (uint64_t)ru.ru_stime.tv_usec * 1000ULL;
    } else {
        *user_ns = 0;
        *sys_ns = 0;
    }
}

/* ---- Helper: check if URI should be ignored ---- */
static int phpray_should_ignore_uri(const char *uri) {
    const char *ignore_list;
    char *copy, *token, *saveptr;
    
    if (!uri || !PHPRAY_G(ignore_uris) || PHPRAY_G(ignore_uris)[0] == '\0') {
        return 0;
    }
    
    ignore_list = PHPRAY_G(ignore_uris);
    copy = estrdup(ignore_list);
    token = strtok_r(copy, ",", &saveptr);
    
    while (token) {
        /* Trim leading whitespace */
        while (*token == ' ') token++;
        
        if (strncmp(uri, token, strlen(token)) == 0) {
            efree(copy);
            return 1;
        }
        token = strtok_r(NULL, ",", &saveptr);
    }
    
    efree(copy);
    return 0;
}

/* ---- Helper: parse output_mode INI into integer ---- */
static int phpray_parse_output_mode(const char *mode) {
    if (!mode) return PHPRAY_OUTPUT_FILE;
    if (strcmp(mode, "shm") == 0) return PHPRAY_OUTPUT_SHM;
    if (strcmp(mode, "both") == 0) return PHPRAY_OUTPUT_BOTH;
    return PHPRAY_OUTPUT_FILE;
}

/* ---- Helper: the ring this process writes to right now ----
 *
 * Plain shm_path: the ring created at MINIT (NULL if creation failed).
 * shm_path with %u / %g: attached lazily here, once the process runs as the
 * site's user (FPM workers switch uid after MINIT), and re-attached when the
 * effective identity changes (mod_ruid2 & co. switch per request). Failures are
 * retried at most every PHPRAY_RING_RETRY_SEC so a broken directory costs one
 * stat() per few seconds, never a slow request. */
static phpray_ring_t *phpray_ring_current(void) {
    char path[PATH_MAX];
    phpray_ring_t *ring = (phpray_ring_t *)PHPRAY_G(ring);
    uid_t uid;
    gid_t gid;
    time_t now;
    size_t shm_size;
    mode_t mode;

    if (!PHPRAY_G(ring_per_user)) {
        return ring;
    }

    uid = geteuid();
    gid = getegid();
    if (ring && PHPRAY_G(ring_uid) == uid && PHPRAY_G(ring_gid) == gid) {
        return ring;
    }
    if (ring) {
        /* identity changed: this ring belongs to the previous user */
        phpray_ring_close(ring);
        PHPRAY_G(ring) = NULL;
    }

    now = time(NULL);
    if (PHPRAY_G(ring_retry_at) && now < PHPRAY_G(ring_retry_at)) {
        return NULL;
    }

    if (phpray_path_expand(PHPRAY_G(shm_path), path, sizeof(path), uid, gid) < 0) {
        PHPRAY_G(ring_retry_at) = now + PHPRAY_RING_RETRY_SEC;
        return NULL;
    }
    shm_size = (size_t)PHPRAY_G(shm_size);
    if (shm_size < PHPRAY_RING_MIN_SIZE) {
        shm_size = PHPRAY_DEFAULT_SHM_SIZE;
    }
    mode = (PHPRAY_G(ring_path_flags) & PHPRAY_PATH_HAS_UID) ? 0600 : 0660;

    ring = phpray_ring_attach(path, shm_size, mode);
    if (!ring) {
        PHPRAY_G(ring_retry_at) = now + PHPRAY_RING_RETRY_SEC;
        return NULL;
    }
    PHPRAY_G(ring) = ring;
    PHPRAY_G(ring_uid) = uid;
    PHPRAY_G(ring_gid) = gid;
    PHPRAY_G(ring_retry_at) = 0;
    return ring;
}

/* ---- Helper: write serialized request to ring buffer ---- */
static void phpray_write_ring(phpray_request_t *req) {
    phpray_ring_t *ring = (phpray_ring_t *)PHPRAY_G(ring);
    /* Buffer needs to hold header + strings + queries + http + marks + errors.
     * 100 queries × 2KB SQL = 200KB worst case. Use stack for small, heap for big. */
    uint8_t stack_buf[16384];
    uint8_t *buf = stack_buf;
    uint32_t buf_size = sizeof(stack_buf);
    uint32_t len;
    int heap_alloc = 0;

    if (!ring) return;

    /* Try stack buffer first */
    len = phpray_serialize_request(req, buf, buf_size);
    if (len == 0) {
        /* Stack buffer too small — try heap */
        buf_size = 262144; /* 256KB */
        buf = malloc(buf_size);
        if (!buf) return;
        heap_alloc = 1;
        len = phpray_serialize_request(req, buf, buf_size);
    }

    if (len > 0) {
        phpray_ring_write(ring, buf, len, PHPRAY_RECORD_TRACE);
    }

    if (heap_alloc) {
        free(buf);
    }
}

/* ---- Helper: determine smart sampling mode ---- */
static int phpray_get_mode(void) {
    const char *m = PHPRAY_G(mode);
    if (!m) return 0; /* smart */
    if (strcmp(m, "all") == 0) return 1;
    if (strcmp(m, "manual") == 0) return 2;
    return 0; /* smart (default) */
}

/* ---- Helper: determine trace level based on duration (smart sampling) ---- */
static uint8_t phpray_determine_trace_level(uint64_t duration_ns, uint16_t response_code) {
    double duration_ms = (double)duration_ns / 1000000.0;
    
    /* 5xx errors always get full trace if configured */
    if (PHPRAY_G(always_trace_errors) && response_code >= 500) {
        return PHPRAY_TRACE_FULL;
    }
    
    if (duration_ms >= (double)PHPRAY_G(threshold_alert_ms)) {
        return PHPRAY_TRACE_ALERT;
    }
    if (duration_ms >= (double)PHPRAY_G(threshold_full_ms)) {
        return PHPRAY_TRACE_FULL;
    }
    if (duration_ms >= (double)PHPRAY_G(threshold_normal_ms)) {
        return PHPRAY_TRACE_NORMAL;
    }
    return PHPRAY_TRACE_SUMMARY;
}

/* ---- JSONL record buffer (persistent, grows as needed) ---- */
typedef struct { char *data; size_t len; size_t cap; } phpray_sbuf_t;

static int phpray_sbuf_reserve(phpray_sbuf_t *b, size_t extra) {
    if (b->len + extra + 1 > b->cap) {
        size_t nc = b->cap ? b->cap : 16384;
        char *nd;
        while (nc < b->len + extra + 1) nc *= 2;
        nd = realloc(b->data, nc);
        if (!nd) return -1;
        b->data = nd;
        b->cap = nc;
    }
    return 0;
}

static void phpray_sbuf_putc(phpray_sbuf_t *b, char c) {
    if (phpray_sbuf_reserve(b, 1) != 0) return;
    b->data[b->len++] = c;
}

static void phpray_sbuf_puts(phpray_sbuf_t *b, const char *str) {
    size_t n = strlen(str);
    if (phpray_sbuf_reserve(b, n) != 0) return;
    memcpy(b->data + b->len, str, n);
    b->len += n;
}

static void phpray_sbuf_printf(phpray_sbuf_t *b, const char *fmt, ...) {
    va_list ap;
    int n;
    if (phpray_sbuf_reserve(b, 64) != 0) return;
    va_start(ap, fmt);
    n = vsnprintf(b->data + b->len, b->cap - b->len, fmt, ap);
    va_end(ap);
    if (n < 0) return;
    if ((size_t)n >= b->cap - b->len) {
        if (phpray_sbuf_reserve(b, (size_t)n) != 0) return;
        va_start(ap, fmt);
        n = vsnprintf(b->data + b->len, b->cap - b->len, fmt, ap);
        va_end(ap);
        if (n < 0) return;
    }
    b->len += (size_t)n;
}

/* ---- Helper: escape string for JSON output ---- */
static void phpray_json_escape(phpray_sbuf_t *b, const char *str) {
    const char *p = str;
    phpray_sbuf_putc(b, '"');
    while (*p) {
        switch (*p) {
            case '"':  phpray_sbuf_puts(b, "\\\""); break;
            case '\\': phpray_sbuf_puts(b, "\\\\"); break;
            case '\n': phpray_sbuf_puts(b, "\\n"); break;
            case '\r': phpray_sbuf_puts(b, "\\r"); break;
            case '\t': phpray_sbuf_puts(b, "\\t"); break;
            default:
                if ((unsigned char)*p < 0x20) {
                    phpray_sbuf_printf(b, "\\u%04x", (unsigned char)*p);
                } else {
                    phpray_sbuf_putc(b, *p);
                }
        }
        p++;
    }
    phpray_sbuf_putc(b, '"');
}

/* ---- Helper: append one record to the JSONL file with a single write() ----
 * The fd is opened with O_APPEND, so concurrent workers (prefork, FPM) never
 * overwrite each other and the kernel serialises whole write() calls on regular
 * files — one record per write() means no interleaved lines. Records larger than
 * what the kernel writes in one go (only on NFS/FUSE, disk-full or a signal that
 * interrupts a partial write) can still be split; that is documented, not locked. */
static void phpray_jsonl_write(const char *data, size_t len) {
    int fd = PHPRAY_G(output_fd);
    if (fd < 0) {
        fd = open(PHPRAY_G(output_path), O_WRONLY | O_APPEND | O_CREAT | O_CLOEXEC, 0644);
        if (fd < 0) { PHPRAY_G(jsonl_errors)++; return; }
        PHPRAY_G(output_fd) = fd;
    }
    while (len > 0) {
        ssize_t w = write(fd, data, len);
        if (w < 0) {
            if (errno == EINTR) continue;
            PHPRAY_G(jsonl_errors)++;
            return;
        }
        data += w;
        len -= (size_t)w;
    }
    PHPRAY_G(jsonl_records)++;
}

/* ---- Helper: generate unique request ID ---- */
static void phpray_generate_request_id(phpray_request_t *req) {
    snprintf(req->phpray_id, PHPRAY_MAX_REQUEST_ID, 
             "pr-%x-%u-%lu", 
             req->pid, (unsigned int)(req->start_time.tv_sec & 0xFFFF),
             (unsigned long)req->request_id);
}

/* ---- Helper: fingerprint URI query string ---- */
/* Replaces query parameter values with * for aggregation:
 * /shop?product_id=1234&color=red → /shop?color=*&product_id=* 
 * Sorts parameters alphabetically for canonical form. */
static void phpray_fingerprint_uri(const char *uri, char *out, size_t out_size) {
    const char *qmark = strchr(uri, '?');
    
    if (!qmark || qmark[1] == '\0') {
        /* No query string — copy as-is (truncated to the buffer) */
        size_t n = strlen(uri);
        if (n >= out_size) n = out_size - 1;
        memcpy(out, uri, n);
        out[n] = '\0';
        return;
    }
    
    /* Copy the path part */
    size_t path_len = (size_t)(qmark - uri);
    if (path_len >= out_size - 1) {
        path_len = out_size - 2;
    }
    memcpy(out, uri, path_len);
    out[path_len] = '?';
    out[path_len + 1] = '\0';
    
    /* Parse query params and collect names (simple — up to 32 params) */
    #define MAX_PARAMS 32
    const char *param_names[MAX_PARAMS];
    size_t param_name_lens[MAX_PARAMS];
    int param_count = 0;
    
    const char *p = qmark + 1;
    while (*p && param_count < MAX_PARAMS) {
        const char *eq = strchr(p, '=');
        const char *amp = strchr(p, '&');
        
        if (!amp) amp = p + strlen(p);
        
        if (eq && eq < amp) {
            param_names[param_count] = p;
            param_name_lens[param_count] = (size_t)(eq - p);
        } else {
            param_names[param_count] = p;
            param_name_lens[param_count] = (size_t)(amp - p);
        }
        param_count++;
        
        if (*amp) {
            p = amp + 1;
        } else {
            break;
        }
    }
    
    /* Simple insertion sort by param name */
    for (int i = 1; i < param_count; i++) {
        const char *key = param_names[i];
        size_t key_len = param_name_lens[i];
        int j = i - 1;
        while (j >= 0) {
            size_t cmp_len = param_name_lens[j] < key_len ? param_name_lens[j] : key_len;
            int cmp = strncmp(param_names[j], key, cmp_len);
            if (cmp > 0 || (cmp == 0 && param_name_lens[j] > key_len)) {
                param_names[j + 1] = param_names[j];
                param_name_lens[j + 1] = param_name_lens[j];
                j--;
            } else {
                break;
            }
        }
        param_names[j + 1] = key;
        param_name_lens[j + 1] = key_len;
    }
    
    /* Build fingerprinted query string */
    size_t pos = path_len + 1;
    for (int i = 0; i < param_count && pos < out_size - 4; i++) {
        if (i > 0 && pos < out_size - 1) {
            out[pos++] = '&';
        }
        size_t copy_len = param_name_lens[i];
        if (pos + copy_len + 2 >= out_size) break;
        memcpy(out + pos, param_names[i], copy_len);
        pos += copy_len;
        out[pos++] = '=';
        out[pos++] = '*';
    }
    out[pos] = '\0';
    #undef MAX_PARAMS
}

/* ---- Error handler hook ---- */
static void phpray_error_cb(int type, zend_string *error_filename, 
                            const uint32_t error_lineno, zend_string *message) {
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (req && req->is_sampled && PHPRAY_G(capture_errors) && 
        req->error_count < PHPRAY_MAX_ERRORS) {
        phpray_error_t *err = &req->errors[req->error_count];
        err->type = type;
        
        /* Capture offset from request start */
        struct timespec now;
        clock_gettime(CLOCK_MONOTONIC, &now);
        err->offset_ns = phpray_time_diff_ns(&req->start_time, &now);
        
        /* Copy truncated error message */
        const char *msg = message ? ZSTR_VAL(message) : "(unknown)";
        strncpy(err->message, msg, PHPRAY_MAX_ERROR_MSG - 1);
        err->message[PHPRAY_MAX_ERROR_MSG - 1] = '\0';
        
        req->error_count++;
    }
    
    /* Always call the original error handler */
    if (PHPRAY_G(original_error_cb)) {
        PHPRAY_G(original_error_cb)(type, error_filename, error_lineno, message);
    }
}

/* ---- Helper: map PHP error type to string ---- */
static const char *phpray_error_type_str(int type) {
    switch (type) {
        case E_ERROR:             return "E_ERROR";
        case E_WARNING:           return "E_WARNING";
        case E_PARSE:             return "E_PARSE";
        case E_NOTICE:            return "E_NOTICE";
        case E_CORE_ERROR:        return "E_CORE_ERROR";
        case E_CORE_WARNING:      return "E_CORE_WARNING";
        case E_COMPILE_ERROR:     return "E_COMPILE_ERROR";
        case E_COMPILE_WARNING:   return "E_COMPILE_WARNING";
        case E_USER_ERROR:        return "E_USER_ERROR";
        case E_USER_WARNING:      return "E_USER_WARNING";
        case E_USER_NOTICE:       return "E_USER_NOTICE";
        case E_STRICT:            return "E_STRICT";
        case E_RECOVERABLE_ERROR: return "E_RECOVERABLE_ERROR";
        case E_DEPRECATED:        return "E_DEPRECATED";
        case E_USER_DEPRECATED:   return "E_USER_DEPRECATED";
        default:                  return "E_UNKNOWN";
    }
}

/* ---- Helper: write trace to the JSONL file (one record = one write()) ---- */
static void phpray_write_trace(phpray_request_t *req) {
    phpray_sbuf_t sb, *ob = &sb;
    double duration_ms, cpu_user_ms, cpu_sys_ms, memory_mb;
    const char *trace_level_str;

    /* Persistent record buffer: allocated once per process, reused by every request */
    sb.data = PHPRAY_G(jsonl_buf);
    sb.cap = PHPRAY_G(jsonl_cap);
    sb.len = 0;
    if (phpray_sbuf_reserve(ob, 16384) != 0) return;
    
    duration_ms = (double)req->duration_ns / 1000000.0;
    cpu_user_ms = (double)req->cpu_user_ns / 1000000.0;
    cpu_sys_ms  = (double)req->cpu_sys_ns / 1000000.0;
    memory_mb   = (double)req->memory_peak / (1024.0 * 1024.0);
    
    switch (req->trace_level) {
        case PHPRAY_TRACE_NORMAL: trace_level_str = "normal"; break;
        case PHPRAY_TRACE_FULL:   trace_level_str = "full"; break;
        case PHPRAY_TRACE_ALERT:  trace_level_str = "alert"; break;
        default:                  trace_level_str = "summary"; break;
    }
    
    /* Write JSON line — using proper escaping */
    phpray_sbuf_puts(ob, "{");
    
    phpray_sbuf_printf(ob, "\"ts\":%lu", (unsigned long)time(NULL));
    phpray_sbuf_printf(ob, ",\"uid\":%u", req->uid);
    phpray_sbuf_printf(ob, ",\"pid\":%u", req->pid);
    phpray_sbuf_printf(ob, ",\"rid\":%lu", (unsigned long)req->request_id);
    
    /* Request ID for correlation */
    phpray_sbuf_puts(ob, ",\"id\":");
    phpray_json_escape(ob, req->phpray_id);
    
    phpray_sbuf_puts(ob, ",\"host\":");
    phpray_json_escape(ob, req->server_name[0] ? req->server_name : "-");
    
    phpray_sbuf_puts(ob, ",\"method\":");
    phpray_json_escape(ob, req->request_method[0] ? req->request_method : "-");
    
    phpray_sbuf_puts(ob, ",\"uri\":");
    phpray_json_escape(ob, req->request_uri[0] ? req->request_uri : "-");
    
    /* URI fingerprint (normalized for aggregation) */
    if (req->request_uri[0] && strchr(req->request_uri, '?')) {
        char fingerprint[PHPRAY_MAX_URI_LEN];
        phpray_fingerprint_uri(req->request_uri, fingerprint, sizeof(fingerprint));
        phpray_sbuf_puts(ob, ",\"uri_fp\":");
        phpray_json_escape(ob, fingerprint);
    }
    
    phpray_sbuf_printf(ob, ",\"status\":%u", req->response_code);
    phpray_sbuf_printf(ob, ",\"duration_ms\":%.3f", duration_ms);
    phpray_sbuf_printf(ob, ",\"cpu_user_ms\":%.3f", cpu_user_ms);
    phpray_sbuf_printf(ob, ",\"cpu_sys_ms\":%.3f", cpu_sys_ms);
    phpray_sbuf_printf(ob, ",\"memory_peak_mb\":%.2f", memory_mb);
    phpray_sbuf_printf(ob, ",\"wp\":%u", req->wp_detected);
    phpray_sbuf_printf(ob, ",\"app\":\"%s\"", phpray_app_name(req->app));
    phpray_sbuf_printf(ob, ",\"profiled\":%u", req->profiled ? 1 : 0);
    if (req->docroot[0]) {
        phpray_sbuf_puts(ob, ",\"docroot\":");
        phpray_json_escape(ob, req->docroot);
    }
    if (req->n_plus_one) {
        phpray_sbuf_puts(ob, ",\"n1\":1");
    }
    
    phpray_sbuf_puts(ob, ",\"level\":");
    phpray_json_escape(ob, trace_level_str);
    
    phpray_sbuf_puts(ob, ",\"php_ver\":");
    phpray_json_escape(ob, PHP_VERSION);
    
    /* Write user marks if any */
    if (req->mark_count > 0) {
        phpray_sbuf_puts(ob, ",\"marks\":[");
        for (uint16_t i = 0; i < req->mark_count; i++) {
            if (i > 0) phpray_sbuf_putc(ob, ',');
            phpray_sbuf_puts(ob, "{\"n\":");
            phpray_json_escape(ob, req->marks[i].name);
            phpray_sbuf_printf(ob, ",\"t\":%.3f}", (double)req->marks[i].offset_ns / 1000000.0);
        }
        phpray_sbuf_putc(ob, ']');
    }
    
    /* Write errors if any */
    if (req->error_count > 0) {
        phpray_sbuf_puts(ob, ",\"errors\":[");
        for (uint16_t i = 0; i < req->error_count; i++) {
            if (i > 0) phpray_sbuf_putc(ob, ',');
            phpray_sbuf_puts(ob, "{\"type\":");
            phpray_json_escape(ob, phpray_error_type_str(req->errors[i].type));
            phpray_sbuf_puts(ob, ",\"msg\":");
            phpray_json_escape(ob, req->errors[i].message);
            phpray_sbuf_printf(ob, ",\"t\":%.3f}", (double)req->errors[i].offset_ns / 1000000.0);
        }
        phpray_sbuf_putc(ob, ']');
    }
    
    /* Write query stats */
    if (req->query_count > 0) {
        phpray_sbuf_printf(ob, ",\"db_count\":%u", req->query_count);
        phpray_sbuf_printf(ob, ",\"db_ms\":%.3f", (double)req->db_total_ns / 1000000.0);
        
        /* Write individual queries for normal+ trace level */
        if (req->trace_level >= PHPRAY_TRACE_NORMAL && req->queries) {
            uint16_t write_count = req->query_count < req->queries_alloc ? 
                                   req->query_count : req->queries_alloc;
            /* For normal level: top 5 slowest. For full+: all */
            /* Simple selection of top N by duration (for normal level) */
            if (req->trace_level < PHPRAY_TRACE_FULL && write_count > 5) {
                /* Sort top 5 by duration (simple insertion sort on small array) */
                /* We just find the top 5 indices */
                uint16_t top[5] = {0};
                uint64_t top_dur[5] = {0};
                for (uint16_t i = 0; i < write_count; i++) {
                    uint64_t dur = req->queries[i].duration_ns;
                    for (int j = 0; j < 5; j++) {
                        if (dur > top_dur[j]) {
                            /* Shift down */
                            for (int k = 4; k > j; k--) {
                                top[k] = top[k-1];
                                top_dur[k] = top_dur[k-1];
                            }
                            top[j] = i;
                            top_dur[j] = dur;
                            break;
                        }
                    }
                }
                
                phpray_sbuf_puts(ob, ",\"queries\":[");
                for (int j = 0; j < 5 && top_dur[j] > 0; j++) {
                    if (j > 0) phpray_sbuf_putc(ob, ',');
                    phpray_query_t *q = &req->queries[top[j]];
                    phpray_sbuf_puts(ob, "{\"sql\":");
                    phpray_json_escape(ob, q->sql);
                    phpray_sbuf_printf(ob, ",\"ms\":%.3f", (double)q->duration_ns / 1000000.0);
                    phpray_sbuf_printf(ob, ",\"t\":%.3f", (double)q->offset_ns / 1000000.0);
                    phpray_sbuf_printf(ob, ",\"src\":%u", q->source);
                    /* Include backtrace if captured */
                    if (q->bt_count > 0) {
                        phpray_sbuf_puts(ob, ",\"bt\":[");
                        for (uint8_t b = 0; b < q->bt_count; b++) {
                            if (b > 0) phpray_sbuf_putc(ob, ',');
                            phpray_sbuf_puts(ob, "{\"f\":");
                            phpray_json_escape(ob, q->bt[b].file);
                            phpray_sbuf_printf(ob, ",\"l\":%u}", q->bt[b].line);
                        }
                        phpray_sbuf_putc(ob, ']');
                    }
                    phpray_sbuf_putc(ob, '}');
                }
                phpray_sbuf_putc(ob, ']');
            } else {
                phpray_sbuf_puts(ob, ",\"queries\":[");
                for (uint16_t i = 0; i < write_count; i++) {
                    if (i > 0) phpray_sbuf_putc(ob, ',');
                    phpray_query_t *q = &req->queries[i];
                    phpray_sbuf_puts(ob, "{\"sql\":");
                    phpray_json_escape(ob, q->sql);
                    phpray_sbuf_printf(ob, ",\"ms\":%.3f", (double)q->duration_ns / 1000000.0);
                    phpray_sbuf_printf(ob, ",\"t\":%.3f", (double)q->offset_ns / 1000000.0);
                    if (q->affected_rows > 0) {
                        phpray_sbuf_printf(ob, ",\"rows\":%u", q->affected_rows);
                    }
                    phpray_sbuf_printf(ob, ",\"src\":%u", q->source);
                    /* Include backtrace if captured */
                    if (q->bt_count > 0) {
                        phpray_sbuf_puts(ob, ",\"bt\":[");
                        for (uint8_t b = 0; b < q->bt_count; b++) {
                            if (b > 0) phpray_sbuf_putc(ob, ',');
                            phpray_sbuf_puts(ob, "{\"f\":");
                            phpray_json_escape(ob, q->bt[b].file);
                            phpray_sbuf_printf(ob, ",\"l\":%u}", q->bt[b].line);
                        }
                        phpray_sbuf_putc(ob, ']');
                    }
                    phpray_sbuf_putc(ob, '}');
                }
                phpray_sbuf_putc(ob, ']');
            }
        }
    }
    
    /* Write HTTP call stats */
    if (req->http_call_count > 0) {
        phpray_sbuf_printf(ob, ",\"http_count\":%u", req->http_call_count);
        phpray_sbuf_printf(ob, ",\"http_ms\":%.3f", (double)req->http_total_ns / 1000000.0);
        
        /* Write individual HTTP calls for normal+ trace level */
        if (req->trace_level >= PHPRAY_TRACE_NORMAL && req->http_calls) {
            uint16_t write_count = req->http_call_count < req->http_calls_alloc ?
                                   req->http_call_count : req->http_calls_alloc;
            phpray_sbuf_puts(ob, ",\"http_calls\":[");
            for (uint16_t i = 0; i < write_count; i++) {
                if (i > 0) phpray_sbuf_putc(ob, ',');
                phpray_http_call_t *call = &req->http_calls[i];
                phpray_sbuf_puts(ob, "{\"url\":");
                phpray_json_escape(ob, call->url);
                phpray_sbuf_printf(ob, ",\"ms\":%.3f", (double)call->duration_ns / 1000000.0);
                phpray_sbuf_printf(ob, ",\"status\":%u", call->response_code);
                phpray_sbuf_printf(ob, ",\"t\":%.3f", (double)call->offset_ns / 1000000.0);
                /* Include backtrace if captured */
                if (call->bt_count > 0) {
                    phpray_sbuf_puts(ob, ",\"bt\":[");
                    for (uint8_t b = 0; b < call->bt_count; b++) {
                        if (b > 0) phpray_sbuf_putc(ob, ',');
                        phpray_sbuf_puts(ob, "{\"f\":");
                        phpray_json_escape(ob, call->bt[b].file);
                        phpray_sbuf_printf(ob, ",\"l\":%u}", call->bt[b].line);
                    }
                    phpray_sbuf_putc(ob, ']');
                }
                phpray_sbuf_putc(ob, '}');
            }
            phpray_sbuf_putc(ob, ']');
        }
    }
    
    /* Write file I/O stats */
    if (req->file_op_count > 0) {
        phpray_sbuf_printf(ob, ",\"file_count\":%u", req->file_op_count);
        phpray_sbuf_printf(ob, ",\"file_ms\":%.3f", (double)req->file_total_ns / 1000000.0);
    }

    /* Write Redis stats */
    if (req->redis_call_count > 0) {
        phpray_sbuf_printf(ob, ",\"redis_count\":%u", req->redis_call_count);
        phpray_sbuf_printf(ob, ",\"redis_ms\":%.3f", (double)req->redis_total_ns / 1000000.0);
    }

    /* Function profile: components sorted by inclusive time (phpray_profiler_request_finish).
     * Present only for profiled requests, regardless of the trace level — the
     * profile is the reason this request paid for the observer. */
    if (req->profiled && req->prof && req->prof->comp_count > 0) {
        phpray_profile_t *p = req->prof;
        phpray_sbuf_puts(ob, ",\"components\":[");
        for (uint16_t i = 0; i < p->comp_count; i++) {
            phpray_comp_t *c = &p->comps[i];
            if (i > 0) phpray_sbuf_putc(ob, ',');
            phpray_sbuf_puts(ob, "{\"name\":");
            phpray_json_escape(ob, c->name);
            phpray_sbuf_printf(ob, ",\"ms\":%.3f", (double)c->incl_ns / 1000000.0);
            phpray_sbuf_printf(ob, ",\"incl_ns\":%llu", (unsigned long long)c->incl_ns);
            phpray_sbuf_printf(ob, ",\"self_ns\":%llu", (unsigned long long)c->self_ns);
            phpray_sbuf_printf(ob, ",\"calls\":%u", c->calls);
            phpray_sbuf_printf(ob, ",\"pct\":%.1f}",
                req->duration_ns > 0 ?
                (double)c->incl_ns / (double)req->duration_ns * 100.0 : 0.0);
        }
        phpray_sbuf_putc(ob, ']');
        if (p->overflow) {
            phpray_sbuf_puts(ob, ",\"prof_overflow\":1");
        }
    }
    
    phpray_sbuf_puts(ob, "}\n");

    phpray_jsonl_write(sb.data, sb.len);

    /* Keep the (possibly grown) buffer for the next request; freed in MSHUTDOWN */
    PHPRAY_G(jsonl_buf) = sb.data;
    PHPRAY_G(jsonl_cap) = sb.cap;
}

/* ---- Userland functions ---- */

/* phpray_mark(string $name): void
 * Records a named timing mark at the current point in request execution.
 * Used for custom profiling — zero overhead when extension is disabled. */
PHP_FUNCTION(phpray_mark) {
    char *name;
    size_t name_len;
    phpray_request_t *req;
    phpray_mark_t *mark;
    struct timespec now;
    
    ZEND_PARSE_PARAMETERS_START(1, 1)
        Z_PARAM_STRING(name, name_len)
    ZEND_PARSE_PARAMETERS_END();
    
    req = PHPRAY_G(current_request);
    if (!req || !req->is_sampled || req->mark_count >= PHPRAY_MAX_MARKS) {
        RETURN_NULL();
    }
    
    mark = &req->marks[req->mark_count];
    strncpy(mark->name, name, PHPRAY_MAX_MARK_NAME - 1);
    mark->name[PHPRAY_MAX_MARK_NAME - 1] = '\0';
    
    clock_gettime(CLOCK_MONOTONIC, &now);
    mark->offset_ns = phpray_time_diff_ns(&req->start_time, &now);
    
    req->mark_count++;
    
    RETURN_NULL();
}

/* phpray_is_tracing(): bool
 * Returns true if the current request is being traced. */
PHP_FUNCTION(phpray_is_tracing) {
    ZEND_PARSE_PARAMETERS_NONE();
    
    phpray_request_t *req = PHPRAY_G(current_request);
    RETURN_BOOL(req && req->is_sampled);
}

/* phpray_request_id(): string|false
 * Returns the PHPRay request ID for the current request, or false if not tracing. */
PHP_FUNCTION(phpray_request_id) {
    ZEND_PARSE_PARAMETERS_NONE();
    
    phpray_request_t *req = PHPRAY_G(current_request);
    if (!req || !req->is_sampled || req->phpray_id[0] == '\0') {
        RETURN_FALSE;
    }
    
    RETURN_STRING(req->phpray_id);
}

/* phpray_ring_stats(): array|false
 * Returns ring buffer statistics, or false if ring buffer is not active. */
PHP_FUNCTION(phpray_ring_stats) {
    phpray_ring_t *ring;

    ZEND_PARSE_PARAMETERS_NONE();

    ring = phpray_ring_current();   /* per-user ring: attach now if not yet */
    if (!ring) {
        RETURN_FALSE;
    }

    array_init(return_value);
    add_assoc_long(return_value, "records", (zend_long)phpray_ring_records(ring));
    add_assoc_long(return_value, "drops", (zend_long)phpray_ring_drops(ring));
    add_assoc_double(return_value, "fill_pct", phpray_ring_fill_pct(ring));
    add_assoc_long(return_value, "capacity", (zend_long)phpray_ring_capacity(ring));
    add_assoc_string(return_value, "path", ring->path ? ring->path : "");
    add_assoc_bool(return_value, "per_user", PHPRAY_G(ring_per_user) ? 1 : 0);
}

/* phpray_health_status(): array
 * Returns crash recovery status: crashes, threshold, window, disabled, last_crash_time. */
PHP_FUNCTION(phpray_health_status) {
    uint32_t crashes, disabled;
    int64_t  last_crash, last_reset;

    ZEND_PARSE_PARAMETERS_NONE();

    phpray_health_get_status(&crashes, &disabled, &last_crash, &last_reset);

    array_init(return_value);
    add_assoc_long(return_value, "crashes", (zend_long)crashes);
    add_assoc_long(return_value, "threshold", (zend_long)PHPRAY_G(crash_threshold));
    add_assoc_long(return_value, "window_sec", (zend_long)PHPRAY_G(crash_window));
    add_assoc_bool(return_value, "disabled", disabled ? 1 : 0);
    add_assoc_long(return_value, "last_crash_time", (zend_long)last_crash);
    add_assoc_long(return_value, "last_reset", (zend_long)last_reset);
}

/* phpray_profile_stats(): array
 * Per-process function-profile statistics: classification cache hits/misses,
 * interned component names, JSONL writer counters. Used by the test suite. */
PHP_FUNCTION(phpray_profile_stats) {
    phpray_class_cache_t *cc;
    ZEND_PARSE_PARAMETERS_NONE();

    cc = PHPRAY_G(class_cache);
    array_init(return_value);
    add_assoc_bool(return_value, "observer", PHPRAY_G(profile_observer_registered) ? 1 : 0);
    add_assoc_string(return_value, "mode", (char *)phpray_profile_mode_name(PHPRAY_G(profile_mode_id)));
    add_assoc_bool(return_value, "profiled", (PHPRAY_G(current_request) && PHPRAY_G(current_request)->profiled) ? 1 : 0);
    add_assoc_long(return_value, "cache_slots", PHPRAY_CLASS_CACHE_SLOTS);
    add_assoc_long(return_value, "cache_hits", cc ? (zend_long)cc->hits : 0);
    add_assoc_long(return_value, "cache_misses", cc ? (zend_long)cc->misses : 0);
    add_assoc_long(return_value, "cache_evictions", cc ? (zend_long)cc->evictions : 0);
    add_assoc_long(return_value, "names", cc ? (zend_long)cc->name_count : 0);
    add_assoc_long(return_value, "name_overflow", cc ? (zend_long)cc->name_overflow : 0);
    add_assoc_long(return_value, "jsonl_records", (zend_long)PHPRAY_G(jsonl_records));
    add_assoc_long(return_value, "jsonl_errors", (zend_long)PHPRAY_G(jsonl_errors));
}

/* phpray_control_status(): array
 * Shared-memory control table as seen by this worker: mapped?, path, seq, entries. */
PHP_FUNCTION(phpray_control_status) {
    phpray_ctrl_entry_t entries[PHPRAY_CTRL_MAX_ENTRIES];
    uint32_t seq = 0;
    uint64_t updated = 0;
    int n;
    zval list;

    ZEND_PARSE_PARAMETERS_NONE();

    phpray_control_maybe_open(PHPRAY_G(control_path), (uint64_t)time(NULL));
    array_init(return_value);
    add_assoc_string(return_value, "path", PHPRAY_G(control_path) ? PHPRAY_G(control_path) : "");
    add_assoc_bool(return_value, "mapped", phpray_control_mapped());
    add_assoc_long(return_value, "hits", (zend_long)PHPRAY_G(control_hits));
    array_init(&list);
    n = phpray_control_snapshot(entries, PHPRAY_CTRL_MAX_ENTRIES, &seq, &updated);
    if (n > 0) {
        int i;
        for (i = 0; i < n; i++) {
            zval e;
            char hbuf[24];
            if (entries[i].until == 0) continue;
            array_init(&e);
            snprintf(hbuf, sizeof(hbuf), "%016llx", (unsigned long long)entries[i].docroot_hash);
            add_assoc_string(&e, "docroot_hash", hbuf);
            add_assoc_string(&e, "url_prefix", entries[i].url_prefix);
            add_assoc_long(&e, "sample_rate", entries[i].sample_rate);
            add_assoc_long(&e, "until", (zend_long)entries[i].until);
            add_assoc_bool(&e, "active", (entries[i].flags & PHPRAY_CTRL_FLAG_ACTIVE) ? 1 : 0);
            add_next_index_zval(&list, &e);
        }
    }
    add_assoc_zval(return_value, "entries", &list);
    add_assoc_long(return_value, "seq", (zend_long)seq);
    add_assoc_long(return_value, "updated_at", (zend_long)updated);
}

/* phpray_health_reset(): void
 * Resets the crash counter and re-enables the extension. */
PHP_FUNCTION(phpray_health_reset) {
    ZEND_PARSE_PARAMETERS_NONE();

    phpray_health_reset();
}

/* ---- Function registration ---- */

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_phpray_mark, 0, 1, IS_NULL, 0)
    ZEND_ARG_TYPE_INFO(0, name, IS_STRING, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_phpray_is_tracing, 0, 0, _IS_BOOL, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_phpray_request_id, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_phpray_ring_stats, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_phpray_health_status, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_phpray_health_reset, 0, 0, IS_NULL, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_phpray_profile_stats, 0, 0, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_INFO_EX(arginfo_phpray_control_status, 0, 0, 0)
ZEND_END_ARG_INFO()

static const zend_function_entry phpray_functions[] = {
    PHP_FE(phpray_mark,       arginfo_phpray_mark)
    PHP_FE(phpray_is_tracing, arginfo_phpray_is_tracing)
    PHP_FE(phpray_request_id, arginfo_phpray_request_id)
    PHP_FE(phpray_ring_stats,    arginfo_phpray_ring_stats)
    PHP_FE(phpray_health_status, arginfo_phpray_health_status)
    PHP_FE(phpray_health_reset,  arginfo_phpray_health_reset)
    PHP_FE(phpray_profile_stats, arginfo_phpray_profile_stats)
    PHP_FE(phpray_control_status, arginfo_phpray_control_status)
    PHP_FE_END
};

/* ---- Module lifecycle ---- */

/* GINIT — initialize globals */
static PHP_GINIT_FUNCTION(phpray) {
#if defined(COMPILE_DL_PHPRAY) && defined(ZTS)
    ZEND_TSRMLS_CACHE_UPDATE();
#endif
    memset(phpray_globals, 0, sizeof(zend_phpray_globals));
    phpray_globals->output_fd = -1;
}

/* GSHUTDOWN — per-thread persistent memory (classification cache) */
static PHP_GSHUTDOWN_FUNCTION(phpray) {
    (void)phpray_globals;
    phpray_profiler_globals_free();
}

/* MINIT — module initialization */
PHP_MINIT_FUNCTION(phpray) {
    REGISTER_INI_ENTRIES();
    PHPRAY_G(output_fd) = -1;

    /* Master switch off: the extension is present in the process and inert. Nothing
     * below this line runs, so an unrelated site on a shared server cannot be
     * affected by a module it never asked for. */
    if (!PHPRAY_G(master_switch)) {
        PHPRAY_G(enabled) = 0;
        PHPRAY_G(current_request) = NULL;
        PHPRAY_G(profile) = NULL;
        PHPRAY_G(ring) = NULL;
        return SUCCESS;
    }

    /* phpray.enabled is PHP_INI_PERDIR: it decides per request (RINIT), never here.
     * Hooks are installed unconditionally so that a vhost/pool enabled via
     * php_value/.user.ini on a globally disabled server gets SQL/curl/file data and
     * the profile. Every hook's fast path is a single test of current_request. */

    /* Crash recovery: check health file before initializing anything */
    if (phpray_health_init() < 0) {
        php_error_docref(NULL, E_WARNING,
            "PHPRay auto-disabled due to repeated crashes in last %ld seconds. "
            "Remove %s to re-enable.",
            (long)PHPRAY_G(crash_window), phpray_health_path());
        PHPRAY_G(enabled) = 0;
        return SUCCESS;
    }
    phpray_health_install_handlers();

    PHPRAY_G(request_counter) = 0;
    
    /* Seed random for manual sampling mode */
    srand((unsigned int)(time(NULL) ^ getpid()));
    
    /* Install error handler hook (the callback checks capture_errors per request) */
    PHPRAY_G(original_error_cb) = zend_error_cb;
    zend_error_cb = phpray_error_cb;
    
    /* Install MySQL/PDO hooks */
    phpray_hooks_mysql_init();
    
    /* Install curl hooks */
    phpray_hooks_curl_init();

    /* Install file I/O hooks (file_get_contents, file_put_contents) */
    phpray_hooks_file_init();

    /* Redis hooks are deferred to RINIT — phpredis may not be loaded yet in MINIT */

    /* Register the function profiler observer (zend_observer; only if profile_functions=1) */
    phpray_profiler_init();

    /* Parse output mode and initialize ring buffer if needed */
    PHPRAY_G(output_mode_id) = phpray_parse_output_mode(PHPRAY_G(output_mode));

    PHPRAY_G(ring) = NULL;
    PHPRAY_G(ring_per_user) = 0;
    PHPRAY_G(ring_path_flags) = 0;
    PHPRAY_G(ring_retry_at) = 0;

    if (PHPRAY_G(output_mode_id) == PHPRAY_OUTPUT_SHM ||
        PHPRAY_G(output_mode_id) == PHPRAY_OUTPUT_BOTH) {
        char path[PATH_MAX];
        size_t shm_size = (size_t)PHPRAY_G(shm_size);
        int flags;
        if (shm_size < PHPRAY_RING_MIN_SIZE) {
            shm_size = PHPRAY_DEFAULT_SHM_SIZE;
        }
        flags = phpray_path_expand(PHPRAY_G(shm_path) ? PHPRAY_G(shm_path) : PHPRAY_DEFAULT_SHM_PATH,
                                   path, sizeof(path), geteuid(), getegid());
        if (flags > 0) {
            /* %u / %g: one private ring per identity, attached by each worker at
             * its first traced request (see phpray_ring_current) — not here, where
             * PHP-FPM still runs as root and a CageFS cage is not entered yet. */
            PHPRAY_G(ring_per_user) = 1;
            PHPRAY_G(ring_path_flags) = flags;
        } else if (flags == 0) {
            PHPRAY_G(ring) = phpray_ring_create(path, shm_size);
            /* Non-fatal if shm creation fails — file output still works */
        }
        /* flags < 0: template too long — no ring, file output still works */
    }

    return SUCCESS;
}

/* MSHUTDOWN — module shutdown */
PHP_MSHUTDOWN_FUNCTION(phpray) {
    if (!PHPRAY_G(master_switch)) {
        UNREGISTER_INI_ENTRIES();
        return SUCCESS;
    }

    /* Crash recovery cleanup (restore signal handlers, munmap) */
    phpray_health_shutdown();

    /* Restore hooks */
    phpray_profiler_shutdown();
    phpray_hooks_redis_shutdown();
    phpray_hooks_file_shutdown();
    phpray_hooks_curl_shutdown();
    phpray_hooks_mysql_shutdown();
    
    /* Restore original error handler */
    if (PHPRAY_G(original_error_cb)) {
        zend_error_cb = PHPRAY_G(original_error_cb);
        PHPRAY_G(original_error_cb) = NULL;
    }
    
    if (PHPRAY_G(output_fd) >= 0) {
        close(PHPRAY_G(output_fd));
        PHPRAY_G(output_fd) = -1;
    }
    if (PHPRAY_G(jsonl_buf)) {
        free(PHPRAY_G(jsonl_buf));
        PHPRAY_G(jsonl_buf) = NULL;
        PHPRAY_G(jsonl_cap) = 0;
    }

    /* Close ring buffer and unlink shared memory */
    if (PHPRAY_G(ring)) {
        phpray_ring_close((phpray_ring_t *)PHPRAY_G(ring));
        PHPRAY_G(ring) = NULL;
    }

    UNREGISTER_INI_ENTRIES();
    return SUCCESS;
}

/* RINIT — request initialization */
PHP_RINIT_FUNCTION(phpray) {
    phpray_request_t *req;
    int mode;

    /* No profile until phpray_profiler_request_init() decides otherwise — every
     * early return below must leave the observer handlers with nothing to do. */
    PHPRAY_G(profile) = NULL;

    if (!PHPRAY_G(master_switch)) {
        PHPRAY_G(current_request) = NULL;
        return SUCCESS;
    }

    /* Deferred hook: Redis class is not available in MINIT (phpredis loads later).
     * hooks_redis_init() is idempotent — only hooks once. */
    phpray_hooks_redis_init();

    if (!PHPRAY_G(enabled)) {
        PHPRAY_G(current_request) = NULL;
        return SUCCESS;
    }

    /* Crash recovery fast path: single mmap read, no syscalls */
    if (phpray_health_check() < 0) {
        PHPRAY_G(current_request) = NULL;
        return SUCCESS;
    }

    /* Skip CLI SAPI unless explicitly enabled */
    if (!PHPRAY_G(trace_cli) && sapi_module.name && 
        strcmp(sapi_module.name, "cli") == 0) {
        PHPRAY_G(current_request) = NULL;
        return SUCCESS;
    }
    
    mode = phpray_get_mode();
    
    /* Manual mode: random sampling */
    if (mode == 2) {
        uint32_t roll = (uint32_t)(rand() % 100);
        if (roll >= (uint32_t)PHPRAY_G(manual_sample_rate)) {
            PHPRAY_G(current_request) = NULL;
            return SUCCESS;
        }
    }
    
    /* Allocate request struct (Zend allocator — per-request) */
    req = ecalloc(1, sizeof(phpray_request_t));
    
    /* Fill basic info */
    req->request_id = ++PHPRAY_G(request_counter);
    req->uid = getuid();
    req->pid = getpid();
    req->is_sampled = 1;
    req->mark_count = 0;
    req->error_count = 0;
    
    /* Capture start time (monotonic clock) */
    clock_gettime(CLOCK_MONOTONIC, &req->start_time);
    phpray_get_cpu_time(&req->cpu_user_ns, &req->cpu_sys_ns);
    
    /* Capture request info from SAPI — these are available in RINIT even for FPM */
    if (SG(request_info).request_uri) {
        strncpy(req->request_uri, SG(request_info).request_uri, PHPRAY_MAX_URI_LEN - 1);
        req->request_uri[PHPRAY_MAX_URI_LEN - 1] = '\0';
    }
    if (SG(request_info).request_method) {
        strncpy(req->request_method, SG(request_info).request_method, PHPRAY_MAX_METHOD_LEN - 1);
        req->request_method[PHPRAY_MAX_METHOD_LEN - 1] = '\0';
    }
    
    /* Check if URI should be ignored */
    if (phpray_should_ignore_uri(req->request_uri)) {
        efree(req);
        PHPRAY_G(current_request) = NULL;
        return SUCCESS;
    }
    
    /* WordPress detection: check SCRIPT_FILENAME for wp- markers */
    if (SG(request_info).path_translated) {
        if (strstr(SG(request_info).path_translated, "/wp-") ||
            strstr(SG(request_info).path_translated, "/wordpress/")) {
            req->wp_detected = 1;
        }
    }
    
    /* Generate unique request ID */
    phpray_generate_request_id(req);
    
    /* Emit X-PHPRay-ID response header if configured */
    if (PHPRAY_G(emit_header)) {
        sapi_header_line header_line;
        char header_buf[256];
        snprintf(header_buf, sizeof(header_buf), "X-PHPRay-ID: %s", req->phpray_id);
        header_line.line = header_buf;
        header_line.line_len = strlen(header_buf);
        header_line.response_code = 0;
        sapi_header_op(SAPI_HEADER_ADD, &header_line);
    }
    
    /* Function profile decision for this request (off/all/sample/url) */
    phpray_profiler_request_init(req);

    PHPRAY_G(current_request) = req;
    
    return SUCCESS;
}

/* RSHUTDOWN — request shutdown */
PHP_RSHUTDOWN_FUNCTION(phpray) {
    phpray_request_t *req;
    uint64_t cpu_user_end, cpu_sys_end;
    int mode;

    if (!PHPRAY_G(master_switch)) {
        return SUCCESS;
    }
    req = PHPRAY_G(current_request);
    
    PHPRAY_G(profile) = NULL;   /* stop the observer handlers first */

    if (!req || !req->is_sampled) {
        if (req) {
            phpray_profiler_request_free(req);
            efree(req);
            PHPRAY_G(current_request) = NULL;
        }
        return SUCCESS;
    }
    
    /* Capture end time */
    clock_gettime(CLOCK_MONOTONIC, &req->end_time);
    req->duration_ns = phpray_time_diff_ns(&req->start_time, &req->end_time);

    /* Close open profile frames, finalize component times, sort by inclusive time */
    phpray_profiler_request_finish(req);
    
    /* Capture end CPU time (compute delta) */
    phpray_get_cpu_time(&cpu_user_end, &cpu_sys_end);
    req->cpu_user_ns = cpu_user_end - req->cpu_user_ns;
    req->cpu_sys_ns = cpu_sys_end - req->cpu_sys_ns;
    
    /* Capture memory peak */
    req->memory_peak = zend_memory_peak_usage(1);
    
    /* Capture response code */
    req->response_code = SG(sapi_headers).http_response_code;
    if (req->response_code == 0) {
        req->response_code = 200;  /* Default if not explicitly set */
    }
    
    /*
     * Capture HTTP_HOST — deferred from RINIT because $_SERVER may not be
     * populated in FPM at that point due to auto_globals_jit.
     */
    if (req->server_name[0] == '\0') {
        /* Method 1: sapi_getenv — most reliable for FPM/CGI */
        {
            char *host_val = sapi_getenv(ZEND_STRL("HTTP_HOST"));
            if (host_val) {
                strncpy(req->server_name, host_val, PHPRAY_MAX_SERVER_NAME - 1);
                req->server_name[PHPRAY_MAX_SERVER_NAME - 1] = '\0';
                efree(host_val);
            }
        }
    }
    
    if (req->server_name[0] == '\0') {
        /* Method 2: Force-populate $_SERVER if JIT-deferred, then check */
        {
            zend_string *server_str = zend_string_init("_SERVER", sizeof("_SERVER") - 1, 0);
            zend_is_auto_global(server_str);
            zend_string_release(server_str);
        }
        
        if (Z_TYPE(PG(http_globals)[TRACK_VARS_SERVER]) == IS_ARRAY) {
            zval *host_zv = zend_hash_str_find(
                Z_ARRVAL(PG(http_globals)[TRACK_VARS_SERVER]),
                "HTTP_HOST", sizeof("HTTP_HOST") - 1
            );
            if (host_zv && Z_TYPE_P(host_zv) == IS_STRING) {
                strncpy(req->server_name, Z_STRVAL_P(host_zv), PHPRAY_MAX_SERVER_NAME - 1);
                req->server_name[PHPRAY_MAX_SERVER_NAME - 1] = '\0';
            }
            
            /* Fallback: SERVER_NAME */
            if (req->server_name[0] == '\0') {
                zval *sn_zv = zend_hash_str_find(
                    Z_ARRVAL(PG(http_globals)[TRACK_VARS_SERVER]),
                    "SERVER_NAME", sizeof("SERVER_NAME") - 1
                );
                if (sn_zv && Z_TYPE_P(sn_zv) == IS_STRING) {
                    strncpy(req->server_name, Z_STRVAL_P(sn_zv), PHPRAY_MAX_SERVER_NAME - 1);
                    req->server_name[PHPRAY_MAX_SERVER_NAME - 1] = '\0';
                }
            }
        }
    }
    
    /* ---- Real REQUEST_URI ----
     * Under mod_php + mod_rewrite SG(request_info).request_uri is the REWRITTEN script
     * (/index.php). $_SERVER['REQUEST_URI'] keeps the original path; use it when present.
     * We never force-populate $_SERVER here (that would cost every request). */
    {
        char *env_uri = sapi_getenv(ZEND_STRL("REQUEST_URI"));
        if (env_uri) {
            if (env_uri[0] != '\0' && strcmp(env_uri, req->request_uri) != 0) {
                strncpy(req->request_uri, env_uri, PHPRAY_MAX_URI_LEN - 1);
                req->request_uri[PHPRAY_MAX_URI_LEN - 1] = '\0';
            }
            efree(env_uri);
        } else if (Z_TYPE(PG(http_globals)[TRACK_VARS_SERVER]) == IS_ARRAY) {
            zval *uri_zv = zend_hash_str_find(
                Z_ARRVAL(PG(http_globals)[TRACK_VARS_SERVER]),
                "REQUEST_URI", sizeof("REQUEST_URI") - 1
            );
            if (uri_zv && Z_TYPE_P(uri_zv) == IS_STRING && Z_STRLEN_P(uri_zv) > 0
                && strcmp(Z_STRVAL_P(uri_zv), req->request_uri) != 0) {
                strncpy(req->request_uri, Z_STRVAL_P(uri_zv), PHPRAY_MAX_URI_LEN - 1);
                req->request_uri[PHPRAY_MAX_URI_LEN - 1] = '\0';
            }
        }
    }

    /* ---- DOCUMENT_ROOT (site identity for the cloud shipper: sha1(host + "\0" + docroot)) ----
     * FPM/CGI and mod_php expose it through sapi_getenv (mod_php: r->subprocess_env);
     * otherwise take it from $_SERVER if the array already exists; last resort: the
     * directory of the executed script (stable per site, not the real docroot). */
    {
        char *dr = sapi_getenv(ZEND_STRL("DOCUMENT_ROOT"));
        if (dr) {
            if (dr[0] != '\0') {
                strncpy(req->docroot, dr, PHPRAY_MAX_DOCROOT_LEN - 1);
                req->docroot[PHPRAY_MAX_DOCROOT_LEN - 1] = '\0';
            }
            efree(dr);
        }
        if (req->docroot[0] == '\0' && Z_TYPE(PG(http_globals)[TRACK_VARS_SERVER]) == IS_ARRAY) {
            zval *dr_zv = zend_hash_str_find(Z_ARRVAL(PG(http_globals)[TRACK_VARS_SERVER]),
                                             "DOCUMENT_ROOT", sizeof("DOCUMENT_ROOT") - 1);
            if (dr_zv && Z_TYPE_P(dr_zv) == IS_STRING && Z_STRLEN_P(dr_zv) > 0) {
                strncpy(req->docroot, Z_STRVAL_P(dr_zv), PHPRAY_MAX_DOCROOT_LEN - 1);
                req->docroot[PHPRAY_MAX_DOCROOT_LEN - 1] = '\0';
            }
        }
        if (req->docroot[0] == '\0' && SG(request_info).path_translated) {
            const char *pt = SG(request_info).path_translated;
            const char *slash = strrchr(pt, '/');
            size_t n = slash ? (size_t)(slash - pt) : 0;
            if (n >= PHPRAY_MAX_DOCROOT_LEN) n = PHPRAY_MAX_DOCROOT_LEN - 1;
            if (n > 0) {
                memcpy(req->docroot, pt, n);
                req->docroot[n] = '\0';
            }
        }
    }

    /* ---- Application detection (constants exist only after the script ran) ---- */
    req->app = phpray_detect_app();
    if (req->app == PHPRAY_APP_WORDPRESS) {
        req->wp_detected = 1;
    }

    /* ---- Smart Sampling Decision ---- */
    mode = phpray_get_mode();
    
    if (mode == 0) {
        /* Smart mode: determine trace level based on duration */
        req->trace_level = phpray_determine_trace_level(req->duration_ns, req->response_code);
        
        /* Bump to at least NORMAL if request has errors */
        if (req->error_count > 0 && req->trace_level < PHPRAY_TRACE_NORMAL) {
            req->trace_level = PHPRAY_TRACE_NORMAL;
        }
    } else if (mode == 1) {
        /* All mode: always full trace */
        req->trace_level = PHPRAY_TRACE_FULL;
    } else {
        /* Manual mode: already sampled in RINIT, default to normal */
        req->trace_level = PHPRAY_TRACE_NORMAL;
    }
    
    /* ---- N+1 Query Detection ---- */
    /* Simple heuristic: hash SQL fingerprints, check for >5 repetitions.
     * Uses a small hash table (64 slots) — good enough for pattern detection. */
    if (req->query_count > 5 && req->queries) {
        #define N1_SLOTS 64
        uint16_t slot_counts[N1_SLOTS];
        memset(slot_counts, 0, sizeof(slot_counts));
        
        uint16_t check_count = req->query_count < req->queries_alloc ?
                               req->query_count : req->queries_alloc;
        
        for (uint16_t i = 0; i < check_count; i++) {
            /* Simple DJB2 hash of the first 64 chars of SQL (structure, not params) */
            const char *s = req->queries[i].sql;
            uint32_t h = 5381;
            int n = 0;
            while (*s && n < 64) {
                /* Skip digits (parameter values) for fingerprinting */
                if (*s >= '0' && *s <= '9') { s++; continue; }
                /* Skip quoted strings */
                if (*s == '\'') {
                    s++;
                    while (*s && *s != '\'') s++;
                    if (*s) s++;
                    continue;
                }
                h = ((h << 5) + h) + (unsigned char)*s;
                s++;
                n++;
            }
            slot_counts[h % N1_SLOTS]++;
        }
        
        /* Check if any slot has >5 hits (likely N+1 pattern) */
        for (int i = 0; i < N1_SLOTS; i++) {
            if (slot_counts[i] > 5) {
                req->n_plus_one = 1;
                break;
            }
        }
        #undef N1_SLOTS
    }
    
    /* Flush any still-tracked curl_multi handles (transfers that weren't properly completed) */
    if (req->multi_track_count > 0) {
        struct timespec now;
        clock_gettime(CLOCK_MONOTONIC, &now);
        for (int i = 0; i < PHPRAY_MAX_MULTI_HANDLES; i++) {
            if (req->multi_tracks[i].active) {
                uint64_t dur = phpray_time_diff_ns(&req->multi_tracks[i].start_time, &now);
                phpray_record_http_call(
                    req->multi_tracks[i].url[0] ? req->multi_tracks[i].url : "(curl_multi:incomplete)",
                    dur, 0);
                req->multi_tracks[i].active = 0;
            }
        }
        req->multi_track_count = 0;
    }

    /* Write trace — output based on configured mode */
    if (PHPRAY_G(output_mode_id) == PHPRAY_OUTPUT_FILE ||
        PHPRAY_G(output_mode_id) == PHPRAY_OUTPUT_BOTH) {
        phpray_write_trace(req);
    }
    if ((PHPRAY_G(output_mode_id) == PHPRAY_OUTPUT_SHM ||
         PHPRAY_G(output_mode_id) == PHPRAY_OUTPUT_BOTH) && phpray_ring_current()) {
        phpray_write_ring(req);
    }
    
    /* Cleanup dynamic arrays */
    if (req->queries) {
        efree(req->queries);
    }
    if (req->http_calls) {
        efree(req->http_calls);
    }
    phpray_profiler_request_free(req);
    
    efree(req);
    PHPRAY_G(current_request) = NULL;
    
    return SUCCESS;
}

/* MINFO — phpinfo() output */
PHP_MINFO_FUNCTION(phpray) {
    char buf[64];
    
    php_info_print_table_start();
    php_info_print_table_header(2, "PHPRay Support",
        PHPRAY_G(master_switch) ? "enabled" : "inert (phpray.master_switch=0)");
    php_info_print_table_row(2, "Version", PHPRAY_VERSION);
    php_info_print_table_row(2, "Master switch", PHPRAY_G(master_switch) ? "1 (hooks installed)" : "0 (no hooks installed)");
    php_info_print_table_row(2, "Mode", PHPRAY_G(mode) ? PHPRAY_G(mode) : "smart");
    php_info_print_table_row(2, "Output Path", PHPRAY_G(output_path) ? PHPRAY_G(output_path) : PHPRAY_DEFAULT_OUTPUT_PATH);
    
    snprintf(buf, sizeof(buf), "%ldms / %ldms / %ldms",
        PHPRAY_G(threshold_normal_ms),
        PHPRAY_G(threshold_full_ms),
        PHPRAY_G(threshold_alert_ms));
    php_info_print_table_row(2, "Smart Thresholds (normal/full/alert)", buf);
    
    snprintf(buf, sizeof(buf), "%ld%%", PHPRAY_G(manual_sample_rate));
    php_info_print_table_row(2, "Manual Sample Rate", buf);
    
    php_info_print_table_row(2, "Always Trace Errors", PHPRAY_G(always_trace_errors) ? "Yes" : "No");
    php_info_print_table_row(2, "Trace CLI", PHPRAY_G(trace_cli) ? "Yes" : "No");
    php_info_print_table_row(2, "X-PHPRay-ID Header", PHPRAY_G(emit_header) ? "Yes" : "No");
    php_info_print_table_row(2, "Capture PHP Errors", PHPRAY_G(capture_errors) ? "Yes" : "No");
    
    php_info_print_table_row(2, "Output Mode", PHPRAY_G(output_mode) ? PHPRAY_G(output_mode) : "file");
    php_info_print_table_row(2, "SHM Path", PHPRAY_G(shm_path) ? PHPRAY_G(shm_path) : PHPRAY_DEFAULT_SHM_PATH);

    snprintf(buf, sizeof(buf), "%ld bytes (%.1f MB)",
        PHPRAY_G(shm_size), (double)PHPRAY_G(shm_size) / (1024.0 * 1024.0));
    php_info_print_table_row(2, "SHM Size", buf);

    if (PHPRAY_G(ring)) {
        php_info_print_table_row(2, "Ring Buffer", PHPRAY_G(ring_per_user)
            ? ((phpray_ring_t *)PHPRAY_G(ring))->path : "Active");
    } else {
        php_info_print_table_row(2, "Ring Buffer", PHPRAY_G(ring_per_user)
            ? "per-user (%u/%g), attached at the first traced request" : "Inactive");
    }
    php_info_print_table_row(2, "Health File", phpray_health_path());

    php_info_print_table_row(2, "Function Profiler (zend_observer)",
        PHPRAY_G(profile_observer_registered) ? "registered" : "not registered (profile_functions=0)");
    php_info_print_table_row(2, "Profile Mode", phpray_profile_mode_name(PHPRAY_G(profile_mode_id)));
    snprintf(buf, sizeof(buf), "%ld%%", (long)PHPRAY_G(profile_sample_rate));
    php_info_print_table_row(2, "Profile Sample Rate", buf);
    php_info_print_table_row(2, "Profile URL Prefixes",
        (PHPRAY_G(profile_url) && PHPRAY_G(profile_url)[0]) ? PHPRAY_G(profile_url) : "(none)");
    snprintf(buf, sizeof(buf), "%ld", (long)PHPRAY_G(profile_max_components));
    php_info_print_table_row(2, "Profile Max Components", buf);
    if (PHPRAY_G(class_cache)) {
        phpray_class_cache_t *cc = PHPRAY_G(class_cache);
        snprintf(buf, sizeof(buf), "%u names, %u hits, %u misses, %u evictions",
            (unsigned)cc->name_count, (unsigned)cc->hits, (unsigned)cc->misses, (unsigned)cc->evictions);
        php_info_print_table_row(2, "Profile Classification Cache", buf);
    }
    snprintf(buf, sizeof(buf), "%lu records, %lu write errors",
        (unsigned long)PHPRAY_G(jsonl_records), (unsigned long)PHPRAY_G(jsonl_errors));
    php_info_print_table_row(2, "JSONL Writer (O_APPEND, one write per record)", buf);
    snprintf(buf, sizeof(buf), "%s (%s, %lu on-demand requests)",
        PHPRAY_G(control_path) ? PHPRAY_G(control_path) : "-",
        phpray_control_mapped() ? "mapped" : "not mapped",
        (unsigned long)PHPRAY_G(control_hits));
    php_info_print_table_row(2, "Control Table (shm)", buf);

    if (PHPRAY_G(ring)) {
        phpray_ring_t *r = (phpray_ring_t *)PHPRAY_G(ring);
        snprintf(buf, sizeof(buf), "%lu records, %lu drops, %.1f%% full",
            (unsigned long)phpray_ring_records(r),
            (unsigned long)phpray_ring_drops(r),
            phpray_ring_fill_pct(r));
        php_info_print_table_row(2, "Ring Buffer Stats", buf);
    }

    /* Crash recovery status */
    {
        uint32_t cr_crashes, cr_disabled;
        int64_t cr_last_crash, cr_last_reset;
        phpray_health_get_status(&cr_crashes, &cr_disabled, &cr_last_crash, &cr_last_reset);
        snprintf(buf, sizeof(buf), "%u crashes, %s",
            cr_crashes, cr_disabled ? "AUTO-DISABLED" : "OK");
        php_info_print_table_row(2, "Crash Recovery", buf);
        snprintf(buf, sizeof(buf), "%ld crashes / %ld sec",
            PHPRAY_G(crash_threshold), PHPRAY_G(crash_window));
        php_info_print_table_row(2, "Crash Threshold", buf);
    }

    php_info_print_table_row(2, "Userland Functions", "phpray_mark(), phpray_is_tracing(), phpray_request_id(), phpray_ring_stats(), phpray_health_status(), phpray_health_reset(), phpray_profile_stats(), phpray_control_status()");

    php_info_print_table_end();
    
    DISPLAY_INI_ENTRIES();
}

/* ---- Module entry ---- */

zend_module_entry phpray_module_entry = {
    STANDARD_MODULE_HEADER,
    PHPRAY_NAME,
    phpray_functions,       /* userland functions */
    PHP_MINIT(phpray),
    PHP_MSHUTDOWN(phpray),
    PHP_RINIT(phpray),
    PHP_RSHUTDOWN(phpray),
    PHP_MINFO(phpray),
    PHPRAY_VERSION,
    PHP_MODULE_GLOBALS(phpray),
    PHP_GINIT(phpray),
    PHP_GSHUTDOWN(phpray),
    NULL,                   /* post deactivate */
    STANDARD_MODULE_PROPERTIES_EX
};

#ifdef COMPILE_DL_PHPRAY
#ifdef ZTS
ZEND_TSRMLS_CACHE_DEFINE()
#endif
ZEND_GET_MODULE(phpray)
#endif
