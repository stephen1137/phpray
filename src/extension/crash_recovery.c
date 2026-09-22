/*
 * PHPRay — Crash Recovery
 *
 * Auto-disables the extension if it crashes repeatedly, preventing
 * customer sites from being impacted on shared hosting.
 *
 * Architecture:
 *   - Health file (phpray.health_path, default /tmp/phpray_health) is mmap'd
 *     for fast access; the path may contain %u / %g (expanded with the
 *     effective uid/gid of the process running MINIT — the user itself under
 *     lsphp/CGI/CageFS, the master under PHP-FPM) and is then created 0600
 *   - Signal handler (SIGSEGV/SIGBUS/SIGABRT) records crash timestamps
 *   - MINIT checks crash count within time window; disables if > threshold
 *   - RINIT fast path: single mmap read of disabled flag
 *
 * CRITICAL: Signal handler uses ONLY async-signal-safe functions.
 */

#include "phpray.h"

#include <fcntl.h>
#include <signal.h>
#include <sys/mman.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <unistd.h>
#include <string.h>
#include <errno.h>
#include <limits.h>

#ifndef PATH_MAX
#define PATH_MAX 4096
#endif

/* Health file structure — mmap'd, shared across FPM workers */
typedef struct {
    uint32_t magic;
    uint32_t version;
    uint32_t crash_count;
    uint32_t disabled;
    int64_t  crash_times[PHPRAY_MAX_CRASH_SLOTS];
    uint32_t crash_slot;
    int64_t  last_reset;
} phpray_health_t;

/* Module-level state (not per-request, survives across requests) */
static phpray_health_t *health_map = NULL;
static int              health_fd  = -1;
static char             health_path_eff[PATH_MAX] = PHPRAY_HEALTH_FILE;

const char *phpray_health_path(void) {
    return health_path_eff;
}

/* Saved original signal handlers */
static struct sigaction orig_sigsegv;
static struct sigaction orig_sigbus;
static struct sigaction orig_sigabrt;
static int handlers_installed = 0;

/* ---- Async-signal-safe crash handler ---- */

/*
 * MUST use ONLY async-signal-safe functions:
 *   write(), _exit(), getpid(), clock_gettime(), signal()
 *
 * NO: malloc, printf, PHP/Zend functions, stdio, syslog
 */
static void phpray_crash_handler(int sig) {
    if (health_map && health_map->magic == PHPRAY_HEALTH_MAGIC) {
        /* Record crash timestamp in ring slot */
        uint32_t slot = __atomic_fetch_add(&health_map->crash_slot, 1, __ATOMIC_SEQ_CST)
                        % PHPRAY_MAX_CRASH_SLOTS;

        struct timespec ts;
        clock_gettime(CLOCK_REALTIME, &ts);
        __atomic_store_n(&health_map->crash_times[slot], (int64_t)ts.tv_sec, __ATOMIC_SEQ_CST);

        /* Increment crash counter */
        __atomic_fetch_add(&health_map->crash_count, 1, __ATOMIC_SEQ_CST);

        /* Write a short message to stderr (async-signal-safe) */
        const char msg[] = "PHPRay: crash detected, recording in health file\n";
        (void)write(STDERR_FILENO, msg, sizeof(msg) - 1);
    }

    /* Reraise with original handler.
     * SA_RESETHAND already restored the default, so raise() will
     * invoke the default action (core dump / terminate). */
    raise(sig);
}

/* ---- Health file init (called in MINIT) ---- */

int phpray_health_init(void) {
    int fd;
    int created = 0;
    phpray_health_t *map;
    struct stat st;
    uint32_t recent_crashes;
    int64_t  now;
    int64_t  window;
    struct timespec ts;

    const char *tmpl;
    int         flags;
    mode_t      mode;

    window = (int64_t)PHPRAY_G(crash_window);
    if (window <= 0) {
        window = PHPRAY_CRASH_WINDOW_SEC;
    }

    /* Resolve the path template (%u / %g). A placeholder makes the file private
     * to this identity; without one it stays shared and world-writable as before. */
    tmpl = PHPRAY_G(health_path);
    if (!tmpl || !tmpl[0]) {
        tmpl = PHPRAY_HEALTH_FILE;
    }
    flags = phpray_path_expand(tmpl, health_path_eff, sizeof(health_path_eff), geteuid(), getegid());
    if (flags < 0) {
        /* Template too long — proceed without crash recovery */
        strncpy(health_path_eff, tmpl, sizeof(health_path_eff) - 1);
        health_path_eff[sizeof(health_path_eff) - 1] = '\0';
        return 0;
    }
    mode = (flags & PHPRAY_PATH_HAS_UID) ? 0600 : (flags & PHPRAY_PATH_HAS_GID) ? 0660 : 0666;

    fd = open(health_path_eff, O_RDWR | O_CREAT | O_NOFOLLOW | O_CLOEXEC, mode);
    if (fd < 0) {
        /* Can't open health file — proceed without crash recovery */
        return 0;
    }

    /* Use flock for safe initialization (multiple FPM workers starting simultaneously) */
    if (flock(fd, LOCK_EX) < 0) {
        close(fd);
        return 0;
    }

    if (fstat(fd, &st) < 0) {
        flock(fd, LOCK_UN);
        close(fd);
        return 0;
    }

    /* A private (%u) file planted by another user under our name is never used. */
    if ((flags & PHPRAY_PATH_HAS_UID) && (!S_ISREG(st.st_mode) || st.st_uid != geteuid())) {
        flock(fd, LOCK_UN);
        close(fd);
        return 0;
    }

    if ((size_t)st.st_size < sizeof(phpray_health_t)) {
        /* New file or too small — initialize */
        if (ftruncate(fd, sizeof(phpray_health_t)) < 0) {
            flock(fd, LOCK_UN);
            close(fd);
            return 0;
        }
        created = 1;
    }

    map = (phpray_health_t *)mmap(NULL, sizeof(phpray_health_t),
                                   PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    if (map == MAP_FAILED) {
        flock(fd, LOCK_UN);
        close(fd);
        return 0;
    }

    /* Initialize if new file or bad magic */
    if (created || map->magic != PHPRAY_HEALTH_MAGIC) {
        memset(map, 0, sizeof(phpray_health_t));
        map->magic   = PHPRAY_HEALTH_MAGIC;
        map->version = 1;
        clock_gettime(CLOCK_REALTIME, &ts);
        map->last_reset = (int64_t)ts.tv_sec;
    }

    /* Count recent crashes within the time window */
    clock_gettime(CLOCK_REALTIME, &ts);
    now = (int64_t)ts.tv_sec;
    recent_crashes = 0;

    for (int i = 0; i < PHPRAY_MAX_CRASH_SLOTS; i++) {
        int64_t t = __atomic_load_n(&map->crash_times[i], __ATOMIC_SEQ_CST);
        if (t > 0 && (now - t) <= window) {
            recent_crashes++;
        }
    }

    flock(fd, LOCK_UN);

    /* Check threshold */
    uint32_t threshold = (uint32_t)PHPRAY_G(crash_threshold);
    if (threshold == 0) {
        threshold = PHPRAY_CRASH_THRESHOLD;
    }

    if (recent_crashes >= threshold) {
        __atomic_store_n(&map->disabled, 1, __ATOMIC_SEQ_CST);
        health_map = map;
        health_fd  = fd;
        return -1; /* Signal caller to disable */
    }

    /* Not disabled — clear the flag if it was set */
    __atomic_store_n(&map->disabled, 0, __ATOMIC_SEQ_CST);

    health_map = map;
    health_fd  = fd;
    return 0;
}

/* ---- Fast-path check (called in RINIT) ---- */

int phpray_health_check(void) {
    if (!health_map) return 0;
    if (__atomic_load_n(&health_map->disabled, __ATOMIC_RELAXED)) {
        return -1;
    }
    return 0;
}

/* ---- Install signal handlers ---- */

void phpray_health_install_handlers(void) {
    struct sigaction sa;

    if (handlers_installed) return;

    memset(&sa, 0, sizeof(sa));
    sa.sa_handler = phpray_crash_handler;
    sa.sa_flags   = SA_RESETHAND;  /* One-shot: prevents handler loop */
    sigemptyset(&sa.sa_mask);

    sigaction(SIGSEGV, &sa, &orig_sigsegv);
    sigaction(SIGBUS,  &sa, &orig_sigbus);
    sigaction(SIGABRT, &sa, &orig_sigabrt);

    handlers_installed = 1;
}

/* ---- Shutdown (called in MSHUTDOWN) ---- */

void phpray_health_shutdown(void) {
    /* Restore original signal handlers */
    if (handlers_installed) {
        sigaction(SIGSEGV, &orig_sigsegv, NULL);
        sigaction(SIGBUS,  &orig_sigbus,  NULL);
        sigaction(SIGABRT, &orig_sigabrt, NULL);
        handlers_installed = 0;
    }

    if (health_map) {
        munmap(health_map, sizeof(phpray_health_t));
        health_map = NULL;
    }
    if (health_fd >= 0) {
        close(health_fd);
        health_fd = -1;
    }
}

/* ---- Reset crash counter (for userland phpray_health_reset()) ---- */

void phpray_health_reset(void) {
    struct timespec ts;

    if (!health_map) return;

    __atomic_store_n(&health_map->crash_count, 0, __ATOMIC_SEQ_CST);
    __atomic_store_n(&health_map->disabled, 0, __ATOMIC_SEQ_CST);
    __atomic_store_n(&health_map->crash_slot, 0, __ATOMIC_SEQ_CST);

    for (int i = 0; i < PHPRAY_MAX_CRASH_SLOTS; i++) {
        __atomic_store_n(&health_map->crash_times[i], (int64_t)0, __ATOMIC_SEQ_CST);
    }

    clock_gettime(CLOCK_REALTIME, &ts);
    __atomic_store_n(&health_map->last_reset, (int64_t)ts.tv_sec, __ATOMIC_SEQ_CST);
}

/* ---- Status query (for userland phpray_health_status()) ---- */

void phpray_health_get_status(uint32_t *crashes, uint32_t *disabled,
                               int64_t *last_crash, int64_t *last_reset_out) {
    int64_t latest = 0;

    if (!health_map) {
        *crashes = 0;
        *disabled = 0;
        *last_crash = 0;
        *last_reset_out = 0;
        return;
    }

    *crashes  = __atomic_load_n(&health_map->crash_count, __ATOMIC_SEQ_CST);
    *disabled = __atomic_load_n(&health_map->disabled, __ATOMIC_SEQ_CST);
    *last_reset_out = __atomic_load_n(&health_map->last_reset, __ATOMIC_SEQ_CST);

    /* Find most recent crash timestamp */
    for (int i = 0; i < PHPRAY_MAX_CRASH_SLOTS; i++) {
        int64_t t = __atomic_load_n(&health_map->crash_times[i], __ATOMIC_SEQ_CST);
        if (t > latest) {
            latest = t;
        }
    }
    *last_crash = latest;
}
