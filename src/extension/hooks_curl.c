/*
 * PHPRay curl Hooks
 * 
 * Intercepts curl_exec and curl_multi_* to capture external HTTP calls.
 * 
 * curl_exec: synchronous — wrap with timer, extract URL + response code after.
 * 
 * curl_multi_*: asynchronous — tracking across multiple calls:
 *   curl_multi_add_handle($mh, $ch)    → register easy handle, capture URL + start time
 *   curl_multi_info_read($mh)          → detect completed transfers, record HTTP call
 *   curl_multi_remove_handle($mh, $ch) → cleanup tracking slot (fallback if info_read missed)
 * 
 * We track easy handles by their zval pointer identity (stable within a request).
 */

#include "phpray.h"

#include <string.h>
#include <time.h>

/* ---- Helper: record HTTP call in current request ---- */
void phpray_record_http_call(const char *url, uint64_t duration_ns, uint16_t response_code) {
    phpray_request_t *req = PHPRAY_G(current_request);
    if (!req || !req->is_sampled) return;
    
    /* Lazy-allocate http_calls array */
    if (!req->http_calls) {
        uint16_t initial = 8;
        req->http_calls = ecalloc(initial, sizeof(phpray_http_call_t));
        req->http_calls_alloc = initial;
    }
    
    /* Grow if needed */
    if (req->http_call_count >= req->http_calls_alloc && 
        req->http_calls_alloc < PHPRAY_MAX_HTTP_CALLS) {
        uint16_t new_alloc = req->http_calls_alloc * 2;
        if (new_alloc > PHPRAY_MAX_HTTP_CALLS) new_alloc = PHPRAY_MAX_HTTP_CALLS;
        req->http_calls = erealloc(req->http_calls, new_alloc * sizeof(phpray_http_call_t));
        memset(req->http_calls + req->http_calls_alloc, 0,
               (new_alloc - req->http_calls_alloc) * sizeof(phpray_http_call_t));
        req->http_calls_alloc = new_alloc;
    }
    
    if (req->http_call_count >= PHPRAY_MAX_HTTP_CALLS) {
        /* Just track totals */
        req->http_call_count++;
        req->http_total_ns += duration_ns;
        return;
    }
    
    phpray_http_call_t *call = &req->http_calls[req->http_call_count];
    
    if (url) {
        size_t url_len = strlen(url);
        size_t copy_len = url_len < PHPRAY_MAX_URL_LEN - 1 ? url_len : PHPRAY_MAX_URL_LEN - 1;
        memcpy(call->url, url, copy_len);
        call->url[copy_len] = '\0';
    }
    
    call->duration_ns = duration_ns;
    call->bt_count = 0;
    call->response_code = response_code;
    
    /* Offset from request start */
    struct timespec now;
    clock_gettime(CLOCK_MONOTONIC, &now);
    call->offset_ns = phpray_time_diff_ns(&req->start_time, &now);
    
    /* Capture backtrace for slow HTTP calls (>500ms) */
    if (duration_ns >= PHPRAY_SLOW_HTTP_BACKTRACE_NS) {
        call->bt_count = phpray_capture_backtrace(call->bt, PHPRAY_MAX_BACKTRACE_FRAMES);
    }
    
    req->http_call_count++;
    req->http_total_ns += duration_ns;
}

/* ---- curl_exec hook ---- */
/* 
 * curl_exec(CurlHandle $handle): string|bool
 * 
 * We wrap the original: start timer → call original → stop timer → 
 * extract URL and response code from the curl handle via reflection.
 * 
 * Getting CURLINFO data from PHP's curl handle:
 * After curl_exec completes, the response code is available via
 * curl_getinfo($ch, CURLINFO_RESPONSE_CODE). We call the PHP function
 * to get it since the internal curl handle isn't directly exposed.
 */

static PHP_FUNCTION(phpray_curl_exec) {
    struct timespec start, end;
    uint64_t duration_ns;
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (!req || !req->is_sampled) {
        PHPRAY_G(orig_curl_exec)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        return;
    }
    
    /* Time the original call */
    clock_gettime(CLOCK_MONOTONIC, &start);
    PHPRAY_G(orig_curl_exec)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
    clock_gettime(CLOCK_MONOTONIC, &end);
    
    duration_ns = phpray_time_diff_ns(&start, &end);
    
    /* Extract URL and response code via curl_getinfo() 
     * We call it as a PHP function since the internal handle isn't exposed */
    const char *url = NULL;
    uint16_t response_code = 0;
    
    zval *args = ZEND_CALL_ARG(execute_data, 1);
    uint32_t argc = ZEND_CALL_NUM_ARGS(execute_data);
    
    if (argc >= 1) {
        zval *ch = &args[0]; /* CurlHandle */
        
        /* Call curl_getinfo($ch, CURLINFO_EFFECTIVE_URL) */
        zval func_name, retval, params[2];
        ZVAL_STRING(&func_name, "curl_getinfo");
        ZVAL_COPY(&params[0], ch);
        
        /* CURLINFO_EFFECTIVE_URL = 0x100001 in PHP (= 1048577) */
        ZVAL_LONG(&params[1], 1048577);
        
        if (call_user_function(CG(function_table), NULL, &func_name, &retval, 2, params) == SUCCESS) {
            if (Z_TYPE(retval) == IS_STRING) {
                url = Z_STRVAL(retval);
            }
        }
        
        /* Get response code: curl_getinfo($ch, CURLINFO_RESPONSE_CODE) = 0x200002 = 2097154 */
        zval retval2;
        ZVAL_LONG(&params[1], 2097154);
        
        if (call_user_function(CG(function_table), NULL, &func_name, &retval2, 2, params) == SUCCESS) {
            if (Z_TYPE(retval2) == IS_LONG) {
                response_code = (uint16_t)Z_LVAL(retval2);
            }
            zval_ptr_dtor(&retval2);
        }
        
        /* Record the HTTP call */
        phpray_record_http_call(url, duration_ns, response_code);
        
        /* Cleanup */
        zval_ptr_dtor(&retval);
        zval_ptr_dtor(&params[0]);
        zval_ptr_dtor(&params[1]);
        zval_ptr_dtor(&func_name);
    } else {
        phpray_record_http_call("(unknown)", duration_ns, 0);
    }
}

/* ---- curl_multi tracking helpers ---- */

/* Find a tracking slot by easy handle pointer */
static phpray_multi_track_t *phpray_multi_find(phpray_request_t *req, void *easy_ptr) {
    for (int i = 0; i < PHPRAY_MAX_MULTI_HANDLES; i++) {
        if (req->multi_tracks[i].active && req->multi_tracks[i].easy_handle == easy_ptr) {
            return &req->multi_tracks[i];
        }
    }
    return NULL;
}

/* Allocate a tracking slot */
static phpray_multi_track_t *phpray_multi_alloc(phpray_request_t *req) {
    if (req->multi_track_count >= PHPRAY_MAX_MULTI_HANDLES) {
        return NULL; /* All slots in use */
    }
    for (int i = 0; i < PHPRAY_MAX_MULTI_HANDLES; i++) {
        if (!req->multi_tracks[i].active) {
            req->multi_track_count++;
            return &req->multi_tracks[i];
        }
    }
    return NULL;
}

/* Free a tracking slot */
static void phpray_multi_free(phpray_request_t *req, phpray_multi_track_t *track) {
    track->active = 0;
    track->easy_handle = NULL;
    if (req->multi_track_count > 0) {
        req->multi_track_count--;
    }
}

/* Extract URL from a curl easy handle using curl_getinfo */
static void phpray_curl_get_url(zval *ch, char *buf, size_t buf_len) {
    zval func_name, retval, params[2];
    buf[0] = '\0';
    
    ZVAL_STRING(&func_name, "curl_getinfo");
    ZVAL_COPY(&params[0], ch);
    ZVAL_LONG(&params[1], 1048577); /* CURLINFO_EFFECTIVE_URL */
    
    if (call_user_function(CG(function_table), NULL, &func_name, &retval, 2, params) == SUCCESS) {
        if (Z_TYPE(retval) == IS_STRING && Z_STRLEN(retval) > 0) {
            size_t copy_len = Z_STRLEN(retval) < buf_len - 1 ? Z_STRLEN(retval) : buf_len - 1;
            memcpy(buf, Z_STRVAL(retval), copy_len);
            buf[copy_len] = '\0';
        }
        zval_ptr_dtor(&retval);
    }
    
    zval_ptr_dtor(&params[0]);
    zval_ptr_dtor(&params[1]);
    zval_ptr_dtor(&func_name);
    
    /* If CURLINFO_EFFECTIVE_URL was empty, try CURLOPT_URL via curl_getinfo without param */
    if (buf[0] == '\0') {
        ZVAL_STRING(&func_name, "curl_getinfo");
        ZVAL_COPY(&params[0], ch);
        
        if (call_user_function(CG(function_table), NULL, &func_name, &retval, 1, params) == SUCCESS) {
            if (Z_TYPE(retval) == IS_ARRAY) {
                zval *url_val = zend_hash_str_find(Z_ARRVAL(retval), "url", sizeof("url") - 1);
                if (url_val && Z_TYPE_P(url_val) == IS_STRING) {
                    size_t copy_len = Z_STRLEN_P(url_val) < buf_len - 1 ? Z_STRLEN_P(url_val) : buf_len - 1;
                    memcpy(buf, Z_STRVAL_P(url_val), copy_len);
                    buf[copy_len] = '\0';
                }
            }
            zval_ptr_dtor(&retval);
        }
        
        zval_ptr_dtor(&params[0]);
        zval_ptr_dtor(&func_name);
    }
}

/* Extract HTTP response code from a curl easy handle */
static uint16_t phpray_curl_get_response_code(zval *ch) {
    zval func_name, retval, params[2];
    uint16_t code = 0;
    
    ZVAL_STRING(&func_name, "curl_getinfo");
    ZVAL_COPY(&params[0], ch);
    ZVAL_LONG(&params[1], 2097154); /* CURLINFO_RESPONSE_CODE */
    
    if (call_user_function(CG(function_table), NULL, &func_name, &retval, 2, params) == SUCCESS) {
        if (Z_TYPE(retval) == IS_LONG) {
            code = (uint16_t)Z_LVAL(retval);
        }
        zval_ptr_dtor(&retval);
    }
    
    zval_ptr_dtor(&params[0]);
    zval_ptr_dtor(&params[1]);
    zval_ptr_dtor(&func_name);
    
    return code;
}

/* Record a completed multi transfer */
static void phpray_multi_complete(phpray_request_t *req, phpray_multi_track_t *track, zval *ch) {
    struct timespec now;
    clock_gettime(CLOCK_MONOTONIC, &now);
    uint64_t duration_ns = phpray_time_diff_ns(&track->start_time, &now);
    
    /* Get response code */
    uint16_t response_code = 0;
    if (ch) {
        response_code = phpray_curl_get_response_code(ch);
    }
    
    /* Record the HTTP call */
    phpray_record_http_call(track->url[0] ? track->url : "(curl_multi)", duration_ns, response_code);
    
    /* Free the slot */
    phpray_multi_free(req, track);
}

/* ---- curl_multi_add_handle hook ---- */
/*
 * curl_multi_add_handle(CurlMultiHandle $multi_handle, CurlHandle $handle): int
 * 
 * We record the easy handle pointer, URL, and start time.
 */
static PHP_FUNCTION(phpray_curl_multi_add_handle) {
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (!req || !req->is_sampled) {
        PHPRAY_G(orig_curl_multi_add_handle)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        return;
    }
    
    /* Get arguments before calling original (we need the easy handle zval) */
    uint32_t argc = ZEND_CALL_NUM_ARGS(execute_data);
    zval *args = ZEND_CALL_ARG(execute_data, 1);
    
    /* Call original first */
    PHPRAY_G(orig_curl_multi_add_handle)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
    
    /* Only track if the add succeeded (returns 0 = CURLM_OK) */
    if (Z_TYPE_P(return_value) == IS_LONG && Z_LVAL_P(return_value) == 0 && argc >= 2) {
        zval *ch = &args[1]; /* CurlHandle */
        
        phpray_multi_track_t *track = phpray_multi_alloc(req);
        if (track) {
            track->active = 1;
            track->easy_handle = (void *)Z_OBJ_P(ch); /* use object pointer as identity */
            clock_gettime(CLOCK_MONOTONIC, &track->start_time);
            
            /* Capture URL at add time (before transfer starts, URL is already set) */
            phpray_curl_get_url(ch, track->url, PHPRAY_MAX_URL_LEN);
        }
    }
}

/* ---- curl_multi_info_read hook ---- */
/*
 * curl_multi_info_read(CurlMultiHandle $multi_handle, &$msgs_in_queue = null): array|false
 * 
 * Returns info about completed transfers. When a transfer completes:
 *   ["msg" => CURLMSG_DONE, "result" => CURLE_OK, "handle" => CurlHandle]
 * 
 * We check if the returned handle matches a tracked one and record the HTTP call.
 */
static PHP_FUNCTION(phpray_curl_multi_info_read) {
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (!req || !req->is_sampled) {
        PHPRAY_G(orig_curl_multi_info_read)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        return;
    }
    
    /* Call original */
    PHPRAY_G(orig_curl_multi_info_read)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
    
    /* Check if we got a completed transfer */
    if (Z_TYPE_P(return_value) == IS_ARRAY) {
        zval *handle_zv = zend_hash_str_find(Z_ARRVAL_P(return_value), "handle", sizeof("handle") - 1);
        if (handle_zv && Z_TYPE_P(handle_zv) == IS_OBJECT) {
            void *easy_ptr = (void *)Z_OBJ_P(handle_zv);
            
            phpray_multi_track_t *track = phpray_multi_find(req, easy_ptr);
            if (track) {
                /* Update URL in case of redirects */
                char final_url[PHPRAY_MAX_URL_LEN];
                phpray_curl_get_url(handle_zv, final_url, PHPRAY_MAX_URL_LEN);
                if (final_url[0]) {
                    memcpy(track->url, final_url, PHPRAY_MAX_URL_LEN);
                }
                
                phpray_multi_complete(req, track, handle_zv);
            }
        }
    }
}

/* ---- curl_multi_remove_handle hook ---- */
/*
 * curl_multi_remove_handle(CurlMultiHandle $multi_handle, CurlHandle $handle): int
 * 
 * Fallback: if the user removes a handle without calling curl_multi_info_read,
 * we still record the HTTP call here. If already recorded via info_read, the
 * tracking slot is already freed so this is a no-op.
 */
static PHP_FUNCTION(phpray_curl_multi_remove_handle) {
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (!req || !req->is_sampled) {
        PHPRAY_G(orig_curl_multi_remove_handle)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        return;
    }
    
    /* Get arguments before calling original */
    uint32_t argc = ZEND_CALL_NUM_ARGS(execute_data);
    zval *args = ZEND_CALL_ARG(execute_data, 1);
    
    /* Check if we're still tracking this handle (not yet recorded via info_read) */
    if (argc >= 2) {
        zval *ch = &args[1];
        if (Z_TYPE_P(ch) == IS_OBJECT) {
            void *easy_ptr = (void *)Z_OBJ_P(ch);
            phpray_multi_track_t *track = phpray_multi_find(req, easy_ptr);
            if (track) {
                /* Transfer was completed but info_read wasn't called — record it now */
                phpray_multi_complete(req, track, ch);
            }
        }
    }
    
    /* Call original */
    PHPRAY_G(orig_curl_multi_remove_handle)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
}

/* ---- Hook installation ---- */

void phpray_hooks_curl_init(void) {
    zend_function *func;
    
    /* Hook curl_exec */
    func = (zend_function *)zend_hash_str_find_ptr(
        CG(function_table), "curl_exec", sizeof("curl_exec") - 1);
    if (func && func->type == ZEND_INTERNAL_FUNCTION) {
        PHPRAY_G(orig_curl_exec) = func->internal_function.handler;
        func->internal_function.handler = zif_phpray_curl_exec;
    }
    
    /* Hook curl_multi_add_handle */
    func = (zend_function *)zend_hash_str_find_ptr(
        CG(function_table), "curl_multi_add_handle", sizeof("curl_multi_add_handle") - 1);
    if (func && func->type == ZEND_INTERNAL_FUNCTION) {
        PHPRAY_G(orig_curl_multi_add_handle) = func->internal_function.handler;
        func->internal_function.handler = zif_phpray_curl_multi_add_handle;
    }
    
    /* Hook curl_multi_info_read */
    func = (zend_function *)zend_hash_str_find_ptr(
        CG(function_table), "curl_multi_info_read", sizeof("curl_multi_info_read") - 1);
    if (func && func->type == ZEND_INTERNAL_FUNCTION) {
        PHPRAY_G(orig_curl_multi_info_read) = func->internal_function.handler;
        func->internal_function.handler = zif_phpray_curl_multi_info_read;
    }
    
    /* Hook curl_multi_remove_handle */
    func = (zend_function *)zend_hash_str_find_ptr(
        CG(function_table), "curl_multi_remove_handle", sizeof("curl_multi_remove_handle") - 1);
    if (func && func->type == ZEND_INTERNAL_FUNCTION) {
        PHPRAY_G(orig_curl_multi_remove_handle) = func->internal_function.handler;
        func->internal_function.handler = zif_phpray_curl_multi_remove_handle;
    }
}

void phpray_hooks_curl_shutdown(void) {
    if (PHPRAY_G(orig_curl_exec)) {
        zend_function *func = (zend_function *)zend_hash_str_find_ptr(
            CG(function_table), "curl_exec", sizeof("curl_exec") - 1);
        if (func && func->type == ZEND_INTERNAL_FUNCTION) {
            func->internal_function.handler = PHPRAY_G(orig_curl_exec);
        }
        PHPRAY_G(orig_curl_exec) = NULL;
    }
    
    if (PHPRAY_G(orig_curl_multi_add_handle)) {
        zend_function *func = (zend_function *)zend_hash_str_find_ptr(
            CG(function_table), "curl_multi_add_handle", sizeof("curl_multi_add_handle") - 1);
        if (func && func->type == ZEND_INTERNAL_FUNCTION) {
            func->internal_function.handler = PHPRAY_G(orig_curl_multi_add_handle);
        }
        PHPRAY_G(orig_curl_multi_add_handle) = NULL;
    }
    
    if (PHPRAY_G(orig_curl_multi_info_read)) {
        zend_function *func = (zend_function *)zend_hash_str_find_ptr(
            CG(function_table), "curl_multi_info_read", sizeof("curl_multi_info_read") - 1);
        if (func && func->type == ZEND_INTERNAL_FUNCTION) {
            func->internal_function.handler = PHPRAY_G(orig_curl_multi_info_read);
        }
        PHPRAY_G(orig_curl_multi_info_read) = NULL;
    }
    
    if (PHPRAY_G(orig_curl_multi_remove_handle)) {
        zend_function *func = (zend_function *)zend_hash_str_find_ptr(
            CG(function_table), "curl_multi_remove_handle", sizeof("curl_multi_remove_handle") - 1);
        if (func && func->type == ZEND_INTERNAL_FUNCTION) {
            func->internal_function.handler = PHPRAY_G(orig_curl_multi_remove_handle);
        }
        PHPRAY_G(orig_curl_multi_remove_handle) = NULL;
    }
}
