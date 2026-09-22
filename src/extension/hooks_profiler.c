/*
 * PHPRay Function Profiler — per-component time attribution via zend_observer.
 *
 * Replaces the former zend_execute_ex hook (which disabled the VM fast call path
 * and OPcache JIT for the whole process and cost 170–330 ns per call on every
 * one of the ~460k user function calls of a WooCommerce request).
 *
 * Design:
 *
 *  - MINIT: if phpray.profile_functions=1 we reserve one op_array extension slot
 *    (zend_get_op_array_extension_handle) and register an fcall observer.
 *    With profile_functions=0 nothing is registered — zero cost.
 *
 *  - Observer init runs once per op_array per request (the run-time cache is
 *    per request). It classifies op_array->filename ONCE into a component
 *    (plugins/<x>, themes/<x>, mu-plugins/<x>, vendor/<v>/<p>) and stores the
 *    component index in the op_array slot. Handlers are returned ONLY for
 *    component files; WordPress core, application code and internal functions
 *    get {NULL, NULL} and are never observed again in this request.
 *    For requests that are not profiled init returns {NULL, NULL} immediately.
 *
 *  - begin/end (hot path): two clock_gettime(CLOCK_MONOTONIC) reads plus a few
 *    loads/stores. No string operations. Inclusive time is measured at the
 *    component boundary (depth counter per component, so recursion and
 *    same-component nesting count once). Self time uses a 256-frame stack with
 *    child-time subtraction: self = time in the component's own frames minus
 *    time in nested observed frames. Core code called from a plugin is charged
 *    to the plugin (both inclusive and self) — that is the product's intent.
 *
 *  - Per-request decision (RINIT): off / all / sample (xorshift RNG, % of
 *    requests) / url (deferred until the first observed call, when
 *    $_SERVER['REQUEST_URI'] is available even under mod_php).
 *
 *  - Robustness: end() verifies the frame identity (execute_data pointer).
 *    Generators (PHP 8.1 does not call end() when an exception unwinds a
 *    generator) and fibers can leave orphan frames; they are popped when the
 *    enclosing frame ends and any frames still open at RSHUTDOWN are closed
 *    there. Stack overflow (>256 open observed frames) keeps inclusive times
 *    exact and marks self times as approximate (prof_overflow).
 */

#include "phpray.h"
#include "control.h"
#include "php_globals.h"
#include "SAPI.h"
#include "zend_observer.h"
#include "zend_extensions.h"

#include <string.h>
#include <strings.h>
#include <time.h>
#include <unistd.h>

/* PHP 8.0 ma ten sam interfejs co 8.1: zend_observer_fcall_register oraz
 * zend_get_op_array_extension_handle(const char *). Poprzednia blokada na 8.1
 * była nadmiarowa i odcinała wersję, która działa. Starsze PHP nie ma
 * obserwatora w ogóle. */
#if PHP_VERSION_ID < 80000
#error "phpray function profiler requires PHP >= 8.0 (zend_observer); build older PHP with PHPRAY_NO_PROFILER"
#endif

/* Slot index in every op_array's run-time cache (reserved at MINIT). */
static int phpray_op_array_ext = -1;

/* Path classes — only the first four are observed. */
#define PHPRAY_CLASS_NONE      0
#define PHPRAY_CLASS_PLUGIN    1
#define PHPRAY_CLASS_THEME     2
#define PHPRAY_CLASS_MUPLUGIN  3
#define PHPRAY_CLASS_VENDOR    4

/* ---- Time ---- */

static zend_always_inline uint64_t phpray_now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

/* ---- RNG (xorshift64*, per process; re-seeded after fork) ---- */

static uint64_t phpray_rng_next(uint32_t pid) {
    uint64_t x;
    if (UNEXPECTED(PHPRAY_G(rng_pid) != pid || PHPRAY_G(rng_state) == 0)) {
        struct timespec ts;
        clock_gettime(CLOCK_MONOTONIC, &ts);
        x = (uint64_t)time(NULL) ^ ((uint64_t)pid << 32) ^ (uint64_t)ts.tv_nsec ^ 0x9E3779B97F4A7C15ULL;
        if (x == 0) x = 0x9E3779B97F4A7C15ULL;
        PHPRAY_G(rng_state) = x;
        PHPRAY_G(rng_pid) = pid;
    }
    x = PHPRAY_G(rng_state);
    x ^= x >> 12;
    x ^= x << 25;
    x ^= x >> 27;
    PHPRAY_G(rng_state) = x;
    return x * 0x2545F4914F6CDD1DULL;
}

/* ---- INI helpers ---- */

int phpray_profile_parse_mode(const char *value, size_t len) {
    /* The INI scanner turns the keywords off/no/false/none/null into "" (and
     * on/yes/true into "1"), so an empty value means OFF. The default "sample"
     * comes from the entry definition as a literal and is not affected. */
    if (!value || len == 0) return PHPRAY_PROFILE_OFF;
    if (len == 6 && strncasecmp(value, "sample", 6) == 0) return PHPRAY_PROFILE_SAMPLE;
    if (len == 3 && strncasecmp(value, "off", 3) == 0) return PHPRAY_PROFILE_OFF;
    if (len == 4 && strncasecmp(value, "none", 4) == 0) return PHPRAY_PROFILE_OFF;
    if (len == 1 && value[0] == '0') return PHPRAY_PROFILE_OFF;
    if (len == 3 && strncasecmp(value, "all", 3) == 0) return PHPRAY_PROFILE_ALL;
    if (len == 1 && value[0] == '1') return PHPRAY_PROFILE_ALL;
    if (len == 3 && strncasecmp(value, "url", 3) == 0) return PHPRAY_PROFILE_URL;
    return -1;
}

const char *phpray_profile_mode_name(int mode) {
    switch (mode) {
        case PHPRAY_PROFILE_OFF:    return "off";
        case PHPRAY_PROFILE_SAMPLE: return "sample";
        case PHPRAY_PROFILE_URL:    return "url";
        case PHPRAY_PROFILE_ALL:    return "all";
        default:                    return "?";
    }
}

/* ---- Path classification (init time only, never on the hot path) ----
 *
 *   .../wp-content/plugins/<slug>/...     → "plugins/<slug>"
 *   .../wp-content/themes/<slug>/...      → "themes/<slug>"
 *   .../wp-content/mu-plugins/<slug>/...  → "mu-plugins/<slug>"   (or the file name)
 *   .../vendor/<vendor>/<package>/...     → "vendor/<vendor>/<package>"
 *   everything else (wp-includes, wp-admin, app code, main script) → not observed
 *
 * The first matching path segment from the left wins, so a composer package
 * bundled inside a plugin (plugins/woocommerce/vendor/...) stays with the plugin.
 * Single pass over the path; no allocation.
 */
static int phpray_classify_path(const char *path, size_t len, char *name, size_t name_size) {
    const char *p = path, *end = path + len;

    while (p < end) {
        const char *seg = p;
        const char *seg_end = memchr(seg, '/', (size_t)(end - seg));
        size_t seg_len;
        if (!seg_end) break;                       /* last segment = file name */
        seg_len = (size_t)(seg_end - seg);
        p = seg_end + 1;
        if (seg_len == 0) continue;

        if (seg_len == 10 && memcmp(seg, "wp-content", 10) == 0) {
            const char *type = p;
            const char *type_end = memchr(type, '/', (size_t)(end - type));
            size_t type_len, prefix_len;
            const char *prefix;
            int cls;
            const char *slug, *slug_end;
            size_t slug_len;

            if (!type_end) return PHPRAY_CLASS_NONE;   /* wp-content/<file> */
            type_len = (size_t)(type_end - type);
            if (type_len == 7 && memcmp(type, "plugins", 7) == 0) {
                prefix = "plugins/"; prefix_len = 8; cls = PHPRAY_CLASS_PLUGIN;
            } else if (type_len == 6 && memcmp(type, "themes", 6) == 0) {
                prefix = "themes/"; prefix_len = 7; cls = PHPRAY_CLASS_THEME;
            } else if (type_len == 10 && memcmp(type, "mu-plugins", 10) == 0) {
                prefix = "mu-plugins/"; prefix_len = 11; cls = PHPRAY_CLASS_MUPLUGIN;
            } else {
                return PHPRAY_CLASS_NONE;              /* wp-content/uploads, languages, ... */
            }
            slug = type_end + 1;
            if (slug >= end) return PHPRAY_CLASS_NONE;
            slug_end = memchr(slug, '/', (size_t)(end - slug));
            slug_len = slug_end ? (size_t)(slug_end - slug) : (size_t)(end - slug);
            if (slug_len == 0) return PHPRAY_CLASS_NONE;
            if (prefix_len + slug_len >= name_size) slug_len = name_size - 1 - prefix_len;
            memcpy(name, prefix, prefix_len);
            memcpy(name + prefix_len, slug, slug_len);
            name[prefix_len + slug_len] = '\0';
            return cls;
        }

        if (seg_len == 6 && memcmp(seg, "vendor", 6) == 0) {
            const char *v = p;
            const char *v_end = memchr(v, '/', (size_t)(end - v));
            const char *pkg, *pkg_end;
            size_t v_len, pkg_len, out;

            if (!v_end) return PHPRAY_CLASS_NONE;      /* vendor/autoload.php → not a package */
            v_len = (size_t)(v_end - v);
            pkg = v_end + 1;
            if (v_len == 0 || pkg >= end) return PHPRAY_CLASS_NONE;
            pkg_end = memchr(pkg, '/', (size_t)(end - pkg));
            if (!pkg_end) {
                /* vendor/composer/ClassLoader.php → "vendor/composer" */
                pkg_len = 0;
            } else {
                pkg_len = (size_t)(pkg_end - pkg);
            }
            out = 7;
            if (out + v_len >= name_size) v_len = name_size - 1 - out;
            memcpy(name, "vendor/", 7);
            memcpy(name + out, v, v_len);
            out += v_len;
            if (pkg_len > 0) {
                if (out + 1 + pkg_len >= name_size) pkg_len = (name_size > out + 1) ? name_size - 1 - out - 1 : 0;
                if (pkg_len > 0) {
                    name[out++] = '/';
                    memcpy(name + out, pkg, pkg_len);
                    out += pkg_len;
                }
            }
            name[out] = '\0';
            return PHPRAY_CLASS_VENDOR;
        }
    }
    return PHPRAY_CLASS_NONE;
}

/* ---- Per-process classification cache + component name table ---- */

static phpray_class_cache_t *phpray_class_cache_get(void) {
    phpray_class_cache_t *cc = PHPRAY_G(class_cache);
    if (UNEXPECTED(!cc)) {
        cc = pecalloc(1, sizeof(phpray_class_cache_t), 1);
        if (!cc) return NULL;
        cc->names = pecalloc(PHPRAY_NAMES_MAX, PHPRAY_MAX_FUNC_NAME, 1);
        if (!cc->names) { pefree(cc, 1); return NULL; }
        memcpy(cc->names[PHPRAY_NAMES_MAX - 1], PHPRAY_PROFILE_OTHER_NAME, sizeof(PHPRAY_PROFILE_OTHER_NAME));
        PHPRAY_G(class_cache) = cc;
    }
    return cc;
}

void phpray_profiler_globals_free(void) {
    phpray_class_cache_t *cc = PHPRAY_G(class_cache);
    if (cc) {
        if (cc->names) pefree(cc->names, 1);
        pefree(cc, 1);
        PHPRAY_G(class_cache) = NULL;
    }
}

/* Intern a component name in the process table (cache misses only). */
static uint16_t phpray_names_intern(phpray_class_cache_t *cc, const char *name) {
    size_t len = strlen(name);
    uint16_t i, last = PHPRAY_NAMES_MAX - 1;
    for (i = 0; i < cc->name_count; i++) {
        if (cc->names[i][len] == '\0' && memcmp(cc->names[i], name, len) == 0) return i;
    }
    if (cc->name_count < last) {
        i = cc->name_count++;
        memcpy(cc->names[i], name, len + 1);
        return i;
    }
    cc->name_overflow++;
    return last;   /* "other" */
}

/* Cache lookup by (hash, len); returns 1 and *pidx on hit. */
static zend_always_inline int phpray_class_cache_find(phpray_class_cache_t *cc, zend_ulong h, uint32_t len, uint16_t *pidx) {
    uint32_t i = (uint32_t)(h ^ (h >> 29)) & (PHPRAY_CLASS_CACHE_SLOTS - 1);
    int n;
    for (n = 0; n < PHPRAY_CLASS_CACHE_PROBES; n++) {
        phpray_class_slot_t *sl = &cc->slots[(i + n) & (PHPRAY_CLASS_CACHE_SLOTS - 1)];
        if (!sl->used) return 0;
        if (sl->h == h && sl->len == len) { *pidx = sl->pidx; return 1; }
    }
    return 0;
}

static void phpray_class_cache_insert(phpray_class_cache_t *cc, zend_ulong h, uint32_t len, uint16_t pidx) {
    uint32_t i = (uint32_t)(h ^ (h >> 29)) & (PHPRAY_CLASS_CACHE_SLOTS - 1);
    phpray_class_slot_t *victim = &cc->slots[i];
    int n;
    for (n = 0; n < PHPRAY_CLASS_CACHE_PROBES; n++) {
        phpray_class_slot_t *sl = &cc->slots[(i + n) & (PHPRAY_CLASS_CACHE_SLOTS - 1)];
        if (!sl->used) { victim = sl; break; }
    }
    if (victim->used) cc->evictions++;
    victim->h = h; victim->len = len; victim->pidx = pidx; victim->used = 1;
}

/* Map a process name index to this request's component slot (allocating it on first use). */
static uint16_t phpray_comp_for_pidx(phpray_profile_t *p, phpray_class_cache_t *cc, uint16_t pidx) {
    uint16_t c = p->map[pidx];
    uint16_t last = (uint16_t)(p->comp_max - 1);
    if (c) return (uint16_t)(c - 1);
    if (p->comp_count < last) {
        c = p->comp_count++;
        memcpy(p->comps[c].name, cc->names[pidx], PHPRAY_MAX_FUNC_NAME);
    } else {
        /* Table full — the last slot aggregates everything else as "other". */
        if (p->comp_count == last) {
            p->comp_count = (uint16_t)(last + 1);
            memcpy(p->comps[last].name, PHPRAY_PROFILE_OTHER_NAME, sizeof(PHPRAY_PROFILE_OTHER_NAME));
        }
        c = last;
    }
    p->map[pidx] = (uint16_t)(c + 1);
    return c;
}

/* ---- url mode: deferred decision ---- */

static int phpray_uri_prefix_match(const char *uri, const char *list) {
    const char *p = list;
    if (!uri || !list) return 0;
    while (*p) {
        const char *q;
        size_t len;
        while (*p == ' ' || *p == '\t' || *p == ',') p++;
        if (!*p) break;
        q = p;
        while (*q && *q != ',') q++;
        len = (size_t)(q - p);
        while (len > 0 && (p[len - 1] == ' ' || p[len - 1] == '\t')) len--;
        if (len > 0 && strncmp(uri, p, len) == 0) return 1;
        p = q;
    }
    return 0;
}

static void phpray_profile_decide(phpray_profile_t *p) {
    const char *uri = NULL;
    char *env = NULL;
    zval *server = &PG(http_globals)[TRACK_VARS_SERVER];
    int attempt, env_tried = 0;

    for (attempt = 0; attempt < 2 && !uri; attempt++) {
        if (Z_TYPE_P(server) == IS_ARRAY) {
            zval *z = zend_hash_str_find(Z_ARRVAL_P(server), "REQUEST_URI", sizeof("REQUEST_URI") - 1);
            if (z && Z_TYPE_P(z) == IS_STRING && Z_STRLEN_P(z) > 0) {
                uri = Z_STRVAL_P(z);
                break;
            }
        }
        if (!env_tried) {
            env_tried = 1;
            env = sapi_getenv(ZEND_STRL("REQUEST_URI"));   /* FPM/CGI: works; mod_php: NULL */
            if (env && env[0]) {
                uri = env;
                break;
            }
        }
        if (attempt == 0) {
            /* Neither source available yet (mod_php + auto_globals_jit, no WordPress):
             * materialise $_SERVER once for this request and retry. */
            zend_is_auto_global_str(ZEND_STRL("_SERVER"));
        }
    }
    if (!uri && SG(request_info).request_uri) {
        uri = SG(request_info).request_uri;
    }

    p->pending = 0;
    if (p->ctl) {
        /* on-demand entry from the control table: one prefix + its own sample rate */
        int match = uri && (p->ctl_prefix[0] == '\0' || strncmp(uri, p->ctl_prefix, strlen(p->ctl_prefix)) == 0);
        if (match && p->ctl_rate < 100) {
            phpray_request_t *req = PHPRAY_G(current_request);
            uint32_t pid = req ? req->pid : (uint32_t)getpid();
            if (p->ctl_rate == 0 || (phpray_rng_next(pid) % 100) >= (uint64_t)p->ctl_rate) match = 0;
        }
        if (match) {
            phpray_request_t *req = PHPRAY_G(current_request);
            p->on = 1;
            if (req) req->profiled = 1;
            PHPRAY_G(control_hits)++;
        } else {
            p->on = 0;
            PHPRAY_G(profile) = NULL;
        }
    } else if (uri && phpray_uri_prefix_match(uri, PHPRAY_G(profile_url))) {
        phpray_request_t *req = PHPRAY_G(current_request);
        p->on = 1;
        if (req) req->profiled = 1;
    } else {
        p->on = 0;
        PHPRAY_G(profile) = NULL;   /* every later begin/end/init returns immediately */
    }
    if (env) efree(env);
}

/* ---- Frame stack ---- */

/* Pop the top frame at time `now`: credit self time, close the component
 * boundary if this was its outermost frame, charge the duration to the parent. */
static zend_always_inline void phpray_pop_frame(phpray_profile_t *p, uint64_t now) {
    phpray_pframe_t *f = &p->stack[--p->stack_len];
    phpray_comp_t *comp = &p->comps[f->comp];
    uint64_t d = now - f->start_ns;
    uint64_t own = d > f->child_ns ? d - f->child_ns : 0;

    comp->self_ns += own;
    if (comp->depth > 0 && --comp->depth == 0) {
        comp->incl_ns += now - comp->enter_ns;
    }
    if (p->stack_len > 0) {
        p->stack[p->stack_len - 1].child_ns += d;
    }
}

/* ---- Observer handlers (hot path) ---- */

static void phpray_observer_begin(zend_execute_data *ed) {
    phpray_profile_t *p = PHPRAY_G(profile);
    uintptr_t slot;
    phpray_comp_t *comp;
    uint64_t now;

    if (UNEXPECTED(!p)) return;
    if (UNEXPECTED(p->pending)) {
        phpray_profile_decide(p);
        if (!p->on) return;
    }
    slot = (uintptr_t)ZEND_OP_ARRAY_EXTENSION(&ed->func->op_array, phpray_op_array_ext);
    if (UNEXPECTED(slot == 0 || slot > p->comp_max)) return;   /* not classified in this request */

    now = phpray_now_ns();
    comp = &p->comps[slot - 1];
    comp->calls++;
    if (comp->depth++ == 0) {
        comp->enter_ns = now;
    }
    if (EXPECTED(p->stack_len < PHPRAY_PROFILE_STACK)) {
        phpray_pframe_t *f = &p->stack[p->stack_len];
        f->ed = ed;
        f->start_ns = now;
        f->child_ns = 0;
        f->comp = (uint16_t)(slot - 1);
    } else {
        p->overflow = 1;
    }
    p->stack_len++;
}

static void phpray_observer_end(zend_execute_data *ed, zval *retval) {
    phpray_profile_t *p = PHPRAY_G(profile);
    uint64_t now;
    uint32_t top;

    (void)retval;   /* NULL when unwinding an exception — handled identically */

    if (UNEXPECTED(!p) || UNEXPECTED(!p->on) || UNEXPECTED(p->stack_len == 0)) return;

    now = phpray_now_ns();

    if (UNEXPECTED(p->stack_len > PHPRAY_PROFILE_STACK)) {
        /* This frame was never recorded (stack overflow): inclusive time only. */
        uintptr_t slot = (uintptr_t)ZEND_OP_ARRAY_EXTENSION(&ed->func->op_array, phpray_op_array_ext);
        if (slot != 0 && slot <= p->comp_max) {
            phpray_comp_t *comp = &p->comps[slot - 1];
            if (comp->depth > 0 && --comp->depth == 0) {
                comp->incl_ns += now - comp->enter_ns;
            }
        }
        p->stack_len--;
        return;
    }

    top = p->stack_len - 1;
    if (UNEXPECTED(p->stack[top].ed != ed)) {
        /* Out-of-order end: a generator/fiber frame above us never got its end()
         * (PHP 8.1 skips end() when an exception unwinds a generator) or a
         * suspended fiber left frames open. Find our frame, close the orphans. */
        uint32_t i = top;
        while (i > 0 && p->stack[i].ed != ed) i--;
        if (p->stack[i].ed != ed) return;   /* unknown frame — ignore */
        while (p->stack_len - 1 > i) {
            phpray_pop_frame(p, now);
        }
    }
    phpray_pop_frame(p, now);
}

/* ---- Observer init: once per op_array per request ---- */

static zend_observer_fcall_handlers phpray_observer_init(zend_execute_data *ed) {
    zend_observer_fcall_handlers none = { NULL, NULL };
    zend_observer_fcall_handlers both = { phpray_observer_begin, phpray_observer_end };
    phpray_profile_t *p = PHPRAY_G(profile);
    phpray_class_cache_t *cc;
    zend_function *func;
    zend_string *filename;
    zend_ulong h;
    uint32_t len;
    uint16_t pidx, idx;

    if (!p) return none;                                   /* request not profiled → cost 0 */

    func = ed->func;
    if (!func || !ZEND_USER_CODE(func->type)) return none; /* internal function */
    filename = func->op_array.filename;
    if (!filename) return none;
    cc = phpray_class_cache_get();
    if (UNEXPECTED(!cc)) return none;

    /* Per-process cache: interned (OPcache) filenames carry a precomputed hash, so a
     * hit never touches the path; a fresh (non-interned) string is hashed once and the
     * hash stays cached in the zend_string for every other op_array of that file. */
    h = ZSTR_H(filename);
    if (!h) h = zend_string_hash_val(filename);
    len = (uint32_t)ZSTR_LEN(filename);
    if (phpray_class_cache_find(cc, h, len, &pidx)) {
        cc->hits++;
    } else {
        char name[PHPRAY_MAX_FUNC_NAME];
        cc->misses++;
        if (phpray_classify_path(ZSTR_VAL(filename), ZSTR_LEN(filename), name, sizeof(name)) == PHPRAY_CLASS_NONE) {
            pidx = PHPRAY_PIDX_NONE;                       /* core / app code: never observed */
        } else {
            pidx = phpray_names_intern(cc, name);
        }
        phpray_class_cache_insert(cc, h, len, pidx);
    }
    if (pidx == PHPRAY_PIDX_NONE) return none;

    idx = phpray_comp_for_pidx(p, cc, pidx);
    ZEND_OP_ARRAY_EXTENSION(&func->op_array, phpray_op_array_ext) = (void *)(uintptr_t)(idx + 1);
    return both;
}

/* ---- Lifecycle ---- */

void phpray_profiler_init(void) {
    if (!PHPRAY_G(profile_functions)) {
        return;
    }
    phpray_op_array_ext = zend_get_op_array_extension_handle("phpray");
    zend_observer_fcall_register(phpray_observer_init);
    PHPRAY_G(profile_observer_registered) = 1;
}

void phpray_profiler_shutdown(void) {
    /* The observer API has no unregister; the process is going away. */
    PHPRAY_G(profile) = NULL;
    phpray_control_close();
}

void phpray_profiler_request_init(phpray_request_t *req) {
    phpray_profile_t *p;
    zend_long max;
    int on = 0, pending = 0, ctl = 0;
    uint16_t ctl_rate = 100;
    char ctl_prefix[PHPRAY_CTRL_PREFIX_LEN] = "";

    PHPRAY_G(profile) = NULL;
    req->prof = NULL;
    req->profiled = 0;

    if (!PHPRAY_G(profile_observer_registered)) {
        return;
    }

    switch (PHPRAY_G(profile_mode_id)) {
        case PHPRAY_PROFILE_ALL:
            on = 1;
            break;
        case PHPRAY_PROFILE_SAMPLE: {
            zend_long rate = PHPRAY_G(profile_sample_rate);
            if (rate >= 100) {
                on = 1;
            } else if (rate > 0) {
                on = (phpray_rng_next(req->pid) % 100) < (uint64_t)rate;
            }
            break;
        }
        case PHPRAY_PROFILE_URL:
            if (PHPRAY_G(profile_url) && PHPRAY_G(profile_url)[0] != '\0') pending = 1;
            break;
        default:
            break;
    }

    /* On-demand profiling requested through the control table (collector / console /
     * DirectAdmin plugin): an active entry for this DOCUMENT_ROOT wins over profile_mode
     * unless the request is already profiled. The decision itself is deferred to the
     * first observed call, when the real REQUEST_URI is known (as in url mode). */
    if (!on) {
        phpray_ctrl_match_t m;
        uint64_t now_epoch = (uint64_t)time(NULL);   /* wall clock: shared with phpray_control_status() */
        phpray_control_maybe_open(PHPRAY_G(control_path), now_epoch);
        if (phpray_control_mapped()) {
            char *dr = sapi_getenv(ZEND_STRL("DOCUMENT_ROOT"));
            const char *docroot = dr;
            size_t dlen = dr ? strlen(dr) : 0;
            char dirbuf[PHPRAY_MAX_DOCROOT_LEN];
            if (dlen == 0 && SG(request_info).path_translated) {
                /* same fallback as the trace's "docroot" field: directory of the script */
                const char *pt = SG(request_info).path_translated;
                const char *slash = strrchr(pt, '/');
                size_t n = slash ? (size_t)(slash - pt) : 0;
                if (n >= sizeof(dirbuf)) n = sizeof(dirbuf) - 1;
                memcpy(dirbuf, pt, n);
                dirbuf[n] = '\0';
                docroot = dirbuf;
                dlen = n;
            }
            if (dlen > 0 && phpray_control_lookup(docroot, dlen, now_epoch, &m)) {
                ctl = 1;
                pending = 1;
                ctl_rate = m.sample_rate;
                memcpy(ctl_prefix, m.url_prefix, sizeof(ctl_prefix));
            }
            if (dr) efree(dr);
        }
    }
    if (!on && !pending) {
        return;
    }

    max = PHPRAY_G(profile_max_components);
    if (max < PHPRAY_PROFILE_MIN_COMPONENTS) max = PHPRAY_PROFILE_MIN_COMPONENTS;
    if (max > PHPRAY_PROFILE_MAX_COMPONENTS) max = PHPRAY_PROFILE_MAX_COMPONENTS;

    p = ecalloc(1, sizeof(phpray_profile_t));
    p->comps = ecalloc((size_t)max, sizeof(phpray_comp_t));
    p->map = ecalloc(PHPRAY_NAMES_MAX, sizeof(uint16_t));
    p->comp_max = (uint16_t)max;
    p->on = (uint8_t)on;
    p->pending = (uint8_t)pending;
    p->ctl = (uint8_t)ctl;
    p->ctl_rate = ctl_rate;
    memcpy(p->ctl_prefix, ctl_prefix, sizeof(p->ctl_prefix));

    req->prof = p;
    req->profiled = (uint8_t)on;
    PHPRAY_G(profile) = p;
}

static void phpray_sort_components(phpray_profile_t *p) {
    /* Insertion sort by inclusive time, descending (≤ 1024 entries, usually < 64). */
    uint16_t i, j;
    for (i = 1; i < p->comp_count; i++) {
        phpray_comp_t key = p->comps[i];
        j = i;
        while (j > 0 && p->comps[j - 1].incl_ns < key.incl_ns) {
            p->comps[j] = p->comps[j - 1];
            j--;
        }
        p->comps[j] = key;
    }
}

void phpray_profiler_request_finish(phpray_request_t *req) {
    phpray_profile_t *p = req->prof;
    uint64_t now;
    uint16_t i;

    PHPRAY_G(profile) = NULL;   /* no more collection from here on */
    if (!p) return;

    if (!p->on) {
        req->profiled = 0;
        return;
    }

    now = phpray_now_ns();
    /* Frames still open (suspended fibers, generators closed by an exception on
     * PHP 8.1, unrecorded overflow frames): close them at request end. */
    if (p->stack_len > PHPRAY_PROFILE_STACK) {
        p->stack_len = PHPRAY_PROFILE_STACK;
    }
    while (p->stack_len > 0) {
        phpray_pop_frame(p, now);
    }
    for (i = 0; i < p->comp_count; i++) {
        if (p->comps[i].depth > 0) {
            p->comps[i].incl_ns += now - p->comps[i].enter_ns;
            p->comps[i].depth = 0;
        }
        if (p->comps[i].self_ns > p->comps[i].incl_ns) {
            p->comps[i].self_ns = p->comps[i].incl_ns;   /* clock jitter guard */
        }
    }
    phpray_sort_components(p);
}

void phpray_profiler_request_free(phpray_request_t *req) {
    phpray_profile_t *p = req->prof;
    if (!p) return;
    if (p->comps) efree(p->comps);
    if (p->map) efree(p->map);
    efree(p);
    req->prof = NULL;
    if (PHPRAY_G(profile) == p) PHPRAY_G(profile) = NULL;
}
