/*
 * PHPRay MySQL/MariaDB Hooks
 * 
 * Intercepts mysqli_query, PDO::query, PDO::exec, PDOStatement::execute.
 * Captures: SQL string, execution time, affected rows.
 * 
 * Hook strategy: replace internal_function.handler (zif_handler) on the
 * zend_function_entry for each target function. This is the standard way
 * to hook PHP functions from an extension without modifying PHP source.
 */

#include "phpray.h"
#include "zend_exceptions.h"

#include <string.h>
#include <time.h>

/* ---- Helper: record query in current request ---- */
void phpray_record_query(const char *sql, size_t sql_len, uint64_t duration_ns, 
                         uint32_t affected_rows, uint8_t source) {
    phpray_request_t *req = PHPRAY_G(current_request);
    if (!req || !req->is_sampled) return;
    
    /* Lazy-allocate query array on first query */
    if (!req->queries) {
        uint16_t initial = 16;
        req->queries = ecalloc(initial, sizeof(phpray_query_t));
        req->queries_alloc = initial;
    }
    
    /* Grow if needed (up to PHPRAY_MAX_QUERIES) */
    if (req->query_count >= req->queries_alloc && req->queries_alloc < PHPRAY_MAX_QUERIES) {
        uint16_t new_alloc = req->queries_alloc * 2;
        if (new_alloc > PHPRAY_MAX_QUERIES) new_alloc = PHPRAY_MAX_QUERIES;
        req->queries = erealloc(req->queries, new_alloc * sizeof(phpray_query_t));
        memset(req->queries + req->queries_alloc, 0, 
               (new_alloc - req->queries_alloc) * sizeof(phpray_query_t));
        req->queries_alloc = new_alloc;
    }
    
    if (req->query_count >= PHPRAY_MAX_QUERIES) {
        /* Just increment counter for overflow tracking */
        req->query_count++;
        req->db_total_ns += duration_ns;
        return;
    }
    
    phpray_query_t *q = &req->queries[req->query_count];
    
    /* Copy SQL (truncate if too long) */
    size_t copy_len = sql_len < PHPRAY_MAX_SQL_LEN - 1 ? sql_len : PHPRAY_MAX_SQL_LEN - 1;
    memcpy(q->sql, sql, copy_len);
    q->sql[copy_len] = '\0';
    
    q->duration_ns = duration_ns;
    q->affected_rows = affected_rows;
    q->source = source;
    q->bt_count = 0;
    
    /* Offset from request start */
    struct timespec now;
    clock_gettime(CLOCK_MONOTONIC, &now);
    q->offset_ns = phpray_time_diff_ns(&req->start_time, &now);
    
    /* Capture backtrace for slow queries (>50ms) */
    if (duration_ns >= PHPRAY_SLOW_QUERY_BACKTRACE_NS) {
        q->bt_count = phpray_capture_backtrace(q->bt, PHPRAY_MAX_BACKTRACE_FRAMES);
    }
    
    req->query_count++;
    req->db_total_ns += duration_ns;
}

/* ---- mysqli_query hook ---- */

/* PHP_FUNCTION signature for our replacement:
 * mysqli_query(mysqli $mysql, string $query, int $result_mode = MYSQLI_STORE_RESULT): mysqli_result|bool
 */
static PHP_FUNCTION(phpray_mysqli_query) {
    struct timespec start, end;
    uint64_t duration_ns;
    phpray_request_t *req = PHPRAY_G(current_request);
    
    /* If not tracing, just call original directly */
    if (!req || !req->is_sampled) {
        PHPRAY_G(orig_mysqli_query)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        return;
    }
    
    /* Extract SQL argument before calling original 
     * We parse args ourselves to get the SQL string, but we don't consume them —
     * the original handler will parse them again. We use zend_parse_parameters
     * with a separate approach: peek at arguments directly. */
    zval *args = ZEND_CALL_ARG(execute_data, 1);
    uint32_t argc = ZEND_CALL_NUM_ARGS(execute_data);
    const char *sql = NULL;
    size_t sql_len = 0;
    
    if (argc >= 2) {
        zval *sql_arg = &args[1]; /* second argument = query string */
        if (Z_TYPE_P(sql_arg) == IS_STRING) {
            sql = Z_STRVAL_P(sql_arg);
            sql_len = Z_STRLEN_P(sql_arg);
        }
    }
    
    /* Time the original call */
    clock_gettime(CLOCK_MONOTONIC, &start);
    PHPRAY_G(orig_mysqli_query)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
    clock_gettime(CLOCK_MONOTONIC, &end);
    
    duration_ns = phpray_time_diff_ns(&start, &end);
    
    /* Record the query */
    if (sql) {
        phpray_record_query(sql, sql_len, duration_ns, 0, 0 /* mysqli */);
    }
}

/* ---- PDO::query hook ---- */

static PHP_FUNCTION(phpray_pdo_query) {
    struct timespec start, end;
    uint64_t duration_ns;
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (!req || !req->is_sampled) {
        PHPRAY_G(orig_pdo_query)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        return;
    }
    
    /* Peek at SQL argument (first arg) */
    zval *args = ZEND_CALL_ARG(execute_data, 1);
    uint32_t argc = ZEND_CALL_NUM_ARGS(execute_data);
    const char *sql = NULL;
    size_t sql_len = 0;
    
    if (argc >= 1 && Z_TYPE_P(&args[0]) == IS_STRING) {
        sql = Z_STRVAL_P(&args[0]);
        sql_len = Z_STRLEN_P(&args[0]);
    }
    
    clock_gettime(CLOCK_MONOTONIC, &start);
    PHPRAY_G(orig_pdo_query)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
    clock_gettime(CLOCK_MONOTONIC, &end);
    
    duration_ns = phpray_time_diff_ns(&start, &end);
    
    if (sql) {
        phpray_record_query(sql, sql_len, duration_ns, 0, 1 /* pdo */);
    }
}

/* ---- PDO::exec hook ---- */

static PHP_FUNCTION(phpray_pdo_exec) {
    struct timespec start, end;
    uint64_t duration_ns;
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (!req || !req->is_sampled) {
        PHPRAY_G(orig_pdo_exec)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        return;
    }
    
    zval *args = ZEND_CALL_ARG(execute_data, 1);
    uint32_t argc = ZEND_CALL_NUM_ARGS(execute_data);
    const char *sql = NULL;
    size_t sql_len = 0;
    
    if (argc >= 1 && Z_TYPE_P(&args[0]) == IS_STRING) {
        sql = Z_STRVAL_P(&args[0]);
        sql_len = Z_STRLEN_P(&args[0]);
    }
    
    clock_gettime(CLOCK_MONOTONIC, &start);
    PHPRAY_G(orig_pdo_exec)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
    clock_gettime(CLOCK_MONOTONIC, &end);
    
    duration_ns = phpray_time_diff_ns(&start, &end);
    
    if (sql) {
        /* For exec, the return value is the affected row count */
        uint32_t affected = 0;
        if (Z_TYPE_P(return_value) == IS_LONG) {
            affected = (uint32_t)Z_LVAL_P(return_value);
        }
        phpray_record_query(sql, sql_len, duration_ns, affected, 1 /* pdo */);
    }
}

/* ---- PDOStatement::execute hook ---- */

static PHP_FUNCTION(phpray_pdo_stmt_execute) {
    struct timespec start, end;
    uint64_t duration_ns;
    phpray_request_t *req = PHPRAY_G(current_request);
    
    if (!req || !req->is_sampled) {
        PHPRAY_G(orig_pdo_stmt_execute)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
        return;
    }
    
    /* For prepared statements, we try to get the query string from the
     * PDOStatement object's queryString property */
    const char *sql = NULL;
    size_t sql_len = 0;
    zval *this_obj = getThis();
    
    if (this_obj) {
        zval rv;
#if PHP_VERSION_ID >= 80000
        zval *query_zv = zend_read_property(Z_OBJCE_P(this_obj), Z_OBJ_P(this_obj),
                                      "queryString", sizeof("queryString") - 1, 1, &rv);
#else
        zval *query_zv = zend_read_property(Z_OBJCE_P(this_obj), this_obj,
                                      "queryString", sizeof("queryString") - 1, 1, &rv);
#endif
        if (query_zv && Z_TYPE_P(query_zv) == IS_STRING) {
            sql = Z_STRVAL_P(query_zv);
            sql_len = Z_STRLEN_P(query_zv);
        }
    }
    
    clock_gettime(CLOCK_MONOTONIC, &start);
    PHPRAY_G(orig_pdo_stmt_execute)(INTERNAL_FUNCTION_PARAM_PASSTHRU);
    clock_gettime(CLOCK_MONOTONIC, &end);
    
    duration_ns = phpray_time_diff_ns(&start, &end);
    
    phpray_record_query(
        sql ? sql : "(prepared)", 
        sql ? sql_len : sizeof("(prepared)") - 1, 
        duration_ns, 0, 1 /* pdo */
    );
}

/* ---- Hook installation ---- */

/* Flag to track if hooks have been installed (lazy init) */
static zend_bool hooks_mysql_installed = 0;

void phpray_hooks_mysql_init(void) {
    zend_function *func;
    zend_class_entry *ce;
    
    if (hooks_mysql_installed) return;
    hooks_mysql_installed = 1;
    
    /* Hook mysqli_query — safe in MINIT, it's a plain function */
    func = (zend_function *)zend_hash_str_find_ptr(
        CG(function_table), "mysqli_query", sizeof("mysqli_query") - 1);
    if (func && func->type == ZEND_INTERNAL_FUNCTION) {
        PHPRAY_G(orig_mysqli_query) = func->internal_function.handler;
        func->internal_function.handler = zif_phpray_mysqli_query;
    }
    
    /* Hook PDO::query and PDO::exec
     * PDO class should be registered by now (loaded via shared extension).
     * We look it up directly from the class table instead of zend_lookup_class
     * which triggers autoloading and is unsafe in MINIT. */
    {
        zend_string *pdo_name = zend_string_init("pdo", sizeof("pdo") - 1, 0);
        ce = (zend_class_entry *)zend_hash_find_ptr(CG(class_table), pdo_name);
        zend_string_release(pdo_name);
        
        if (ce) {
            /* PDO::query */
            func = (zend_function *)zend_hash_str_find_ptr(
                &ce->function_table, "query", sizeof("query") - 1);
            if (func && func->type == ZEND_INTERNAL_FUNCTION) {
                PHPRAY_G(orig_pdo_query) = func->internal_function.handler;
                func->internal_function.handler = zif_phpray_pdo_query;
            }
            
            /* PDO::exec */
            func = (zend_function *)zend_hash_str_find_ptr(
                &ce->function_table, "exec", sizeof("exec") - 1);
            if (func && func->type == ZEND_INTERNAL_FUNCTION) {
                PHPRAY_G(orig_pdo_exec) = func->internal_function.handler;
                func->internal_function.handler = zif_phpray_pdo_exec;
            }
        }
    }
    
    /* Hook PDOStatement::execute */
    {
        zend_string *stmt_name = zend_string_init("pdostatement", sizeof("pdostatement") - 1, 0);
        ce = (zend_class_entry *)zend_hash_find_ptr(CG(class_table), stmt_name);
        zend_string_release(stmt_name);
        
        if (ce) {
            func = (zend_function *)zend_hash_str_find_ptr(
                &ce->function_table, "execute", sizeof("execute") - 1);
            if (func && func->type == ZEND_INTERNAL_FUNCTION) {
                PHPRAY_G(orig_pdo_stmt_execute) = func->internal_function.handler;
                func->internal_function.handler = zif_phpray_pdo_stmt_execute;
            }
        }
    }
}

void phpray_hooks_mysql_shutdown(void) {
    zend_function *func;
    zend_class_entry *ce;
    
    if (!hooks_mysql_installed) return;
    hooks_mysql_installed = 0;
    
    /* Restore mysqli_query */
    if (PHPRAY_G(orig_mysqli_query)) {
        func = (zend_function *)zend_hash_str_find_ptr(
            CG(function_table), "mysqli_query", sizeof("mysqli_query") - 1);
        if (func && func->type == ZEND_INTERNAL_FUNCTION) {
            func->internal_function.handler = PHPRAY_G(orig_mysqli_query);
        }
        PHPRAY_G(orig_mysqli_query) = NULL;
    }
    
    /* Restore PDO::query, PDO::exec */
    {
        zend_string *pdo_name = zend_string_init("pdo", sizeof("pdo") - 1, 0);
        ce = (zend_class_entry *)zend_hash_find_ptr(CG(class_table), pdo_name);
        zend_string_release(pdo_name);
        
        if (ce) {
            if (PHPRAY_G(orig_pdo_query)) {
                func = (zend_function *)zend_hash_str_find_ptr(
                    &ce->function_table, "query", sizeof("query") - 1);
                if (func && func->type == ZEND_INTERNAL_FUNCTION) {
                    func->internal_function.handler = PHPRAY_G(orig_pdo_query);
                }
                PHPRAY_G(orig_pdo_query) = NULL;
            }
            if (PHPRAY_G(orig_pdo_exec)) {
                func = (zend_function *)zend_hash_str_find_ptr(
                    &ce->function_table, "exec", sizeof("exec") - 1);
                if (func && func->type == ZEND_INTERNAL_FUNCTION) {
                    func->internal_function.handler = PHPRAY_G(orig_pdo_exec);
                }
                PHPRAY_G(orig_pdo_exec) = NULL;
            }
        }
    }
    
    /* Restore PDOStatement::execute */
    if (PHPRAY_G(orig_pdo_stmt_execute)) {
        zend_string *stmt_name = zend_string_init("pdostatement", sizeof("pdostatement") - 1, 0);
        ce = (zend_class_entry *)zend_hash_find_ptr(CG(class_table), stmt_name);
        zend_string_release(stmt_name);
        
        if (ce) {
            func = (zend_function *)zend_hash_str_find_ptr(
                &ce->function_table, "execute", sizeof("execute") - 1);
            if (func && func->type == ZEND_INTERNAL_FUNCTION) {
                func->internal_function.handler = PHPRAY_G(orig_pdo_stmt_execute);
            }
        }
        PHPRAY_G(orig_pdo_stmt_execute) = NULL;
    }
}
