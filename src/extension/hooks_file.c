/*
 * PHPRay Filesystem Hooks
 * 
 * Intercepts file_get_contents and file_put_contents.
 * 
 * Key feature: URL detection for file_get_contents.
 * If the path starts with http:// or https://, the call is recorded as
 * an HTTP call (like curl_exec) instead of a file operation.
 * This captures WordPress's wp_remote_get()-style HTTP without curl.
 * 
 * File ops captured: path, operation (read/write), bytes, timing.
 * Only detailed per-op data at trace_level >= NORMAL; summary always has totals.
 */

#include "phpray.h"

#include <string.h>
#include <time.h>

/* File operation types */
#define PHPRAY_FILE_OP_READ  0
#define PHPRAY_FILE_OP_WRITE 1

/* ---- Data structure for file operations ---- */

typedef struct {
    char     path[PHPRAY_MAX_PATH_LEN];
    uint64_t duration_ns;
    uint64_t offset_ns;   /* offset from request start */
    uint64_t bytes;        /* bytes read/written */
    uint8_t  op_type;      /* PHPRAY_FILE_OP_READ or WRITE */
} phpray_file_op_t;

/* ---- Helper: check if path is a URL ---- */
static int phpray_is_url(const char *path) {
    if (!path) return 0;
    return (strncasecmp(path, "http://", 7) == 0 || 
            strncasecmp(path, "https://", 8) == 0);
}

/* ---- Helper: record file operation ---- */
static void phpray_record_file_op(const char *path, uint8_t op_type, 
                                  uint64_t bytes, uint64_t duration_ns) {
    phpray_request_t *req = PHPRAY_G(current_request);
    if (!req || !req->is_sampled) return;
    
    /* Update totals (always tracked) */
    req->file_op_count++;
    req->file_total_ns += duration_ns;
    
    /* We don't store individual file ops in the request struct to keep it simple.
     * File ops are high-volume (WordPress does hundreds of file reads per request).
     * Instead we track: count, total time, and for full traces, a top-N list.
     * 
     * For Phase 1, we just track counts + totals in the existing struct.
     * Individual file op recording can be added later if needed. */
}

/* ---- file_get_contents hook ---- */
/*
 * file_get_contents(string $filename, bool $use_include_path = false,
 *                   ?resource $context = null, int $offset = 0, 
 *                   ?int $length = null): string|false
 *
 * If filename is a URL (http/https), record as HTTP call.
 * Otherwise, record as file I/O operation.
 */

/* Original handler storage — stored in globals via phpray.h */
static zif_handler orig_file_get_contents = NULL;
static zif_handler orig_file_put_contents = NULL;

static PHP_FUNCTION(phpray_file_get_contents) {
    struct timespec start, end;
    uint64_t duration_ns;
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (!req || !req->is_sampled || !orig_file_get_contents) {
        if (orig_file_get_contents) {
            orig_file_get_contents(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        }
        return;
    }
    
    /* Extract the filename argument before calling original */
    char *filename = NULL;
    uint32_t argc = ZEND_CALL_NUM_ARGS(execute_data);
    
    if (argc >= 1) {
        zval *arg = ZEND_CALL_ARG(execute_data, 1);
        if (Z_TYPE_P(arg) == IS_STRING) {
            filename = Z_STRVAL_P(arg);
        }
    }
    
    /* Time the original call */
    clock_gettime(CLOCK_MONOTONIC, &start);
    orig_file_get_contents(INTERNAL_FUNCTION_PARAM_PASSTHRU);
    clock_gettime(CLOCK_MONOTONIC, &end);
    
    duration_ns = phpray_time_diff_ns(&start, &end);
    
    /* Determine bytes read from return value */
    uint64_t bytes = 0;
    if (Z_TYPE_P(return_value) == IS_STRING) {
        bytes = Z_STRLEN_P(return_value);
    }
    
    if (filename && phpray_is_url(filename)) {
        /* This is an HTTP call via stream wrapper — record as HTTP call.
         * We can't get HTTP response code from file_get_contents easily,
         * so we record 0 and use the return value to infer success/failure. */
        uint16_t response_code = 0;
        if (Z_TYPE_P(return_value) == IS_FALSE) {
            response_code = 0; /* Failed — no response code available */
        } else {
            response_code = 200; /* Assume 200 on success — file_get_contents 
                                  * returns false on 4xx/5xx by default */
        }
        phpray_record_http_call(filename ? filename : "(unknown)", duration_ns, response_code);
    } else {
        /* Regular file read */
        phpray_record_file_op(filename ? filename : "(unknown)", 
                             PHPRAY_FILE_OP_READ, bytes, duration_ns);
    }
}

/* ---- file_put_contents hook ---- */
/*
 * file_put_contents(string $filename, mixed $data, int $flags = 0,
 *                   ?resource $context = null): int|false
 */

static PHP_FUNCTION(phpray_file_put_contents) {
    struct timespec start, end;
    uint64_t duration_ns;
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (!req || !req->is_sampled || !orig_file_put_contents) {
        if (orig_file_put_contents) {
            orig_file_put_contents(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        }
        return;
    }
    
    /* Extract filename */
    char *filename = NULL;
    uint32_t argc = ZEND_CALL_NUM_ARGS(execute_data);
    
    if (argc >= 1) {
        zval *arg = ZEND_CALL_ARG(execute_data, 1);
        if (Z_TYPE_P(arg) == IS_STRING) {
            filename = Z_STRVAL_P(arg);
        }
    }
    
    /* Time the original call */
    clock_gettime(CLOCK_MONOTONIC, &start);
    orig_file_put_contents(INTERNAL_FUNCTION_PARAM_PASSTHRU);
    clock_gettime(CLOCK_MONOTONIC, &end);
    
    duration_ns = phpray_time_diff_ns(&start, &end);
    
    /* Bytes written from return value */
    uint64_t bytes = 0;
    if (Z_TYPE_P(return_value) == IS_LONG) {
        bytes = (uint64_t)Z_LVAL_P(return_value);
    }
    
    phpray_record_file_op(filename ? filename : "(unknown)",
                         PHPRAY_FILE_OP_WRITE, bytes, duration_ns);
}

/* ---- Hook installation ---- */

void phpray_hooks_file_init(void) {
    zend_function *func;
    
    /* Hook file_get_contents */
    func = (zend_function *)zend_hash_str_find_ptr(
        CG(function_table), "file_get_contents", sizeof("file_get_contents") - 1);
    if (func && func->type == ZEND_INTERNAL_FUNCTION) {
        orig_file_get_contents = func->internal_function.handler;
        func->internal_function.handler = zif_phpray_file_get_contents;
    }
    
    /* Hook file_put_contents */
    func = (zend_function *)zend_hash_str_find_ptr(
        CG(function_table), "file_put_contents", sizeof("file_put_contents") - 1);
    if (func && func->type == ZEND_INTERNAL_FUNCTION) {
        orig_file_put_contents = func->internal_function.handler;
        func->internal_function.handler = zif_phpray_file_put_contents;
    }
}

void phpray_hooks_file_shutdown(void) {
    if (orig_file_get_contents) {
        zend_function *func = (zend_function *)zend_hash_str_find_ptr(
            CG(function_table), "file_get_contents", sizeof("file_get_contents") - 1);
        if (func && func->type == ZEND_INTERNAL_FUNCTION) {
            func->internal_function.handler = orig_file_get_contents;
        }
        orig_file_get_contents = NULL;
    }
    
    if (orig_file_put_contents) {
        zend_function *func = (zend_function *)zend_hash_str_find_ptr(
            CG(function_table), "file_put_contents", sizeof("file_put_contents") - 1);
        if (func && func->type == ZEND_INTERNAL_FUNCTION) {
            func->internal_function.handler = orig_file_put_contents;
        }
        orig_file_put_contents = NULL;
    }
}
