/*
 * PHPRay control table reader (see control.h). Read-only mmap, seqlock reads.
 */

#include "phpray.h"
#include "control.h"

#include <sys/mman.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <unistd.h>
#include <string.h>

/* Per process (per thread under ZTS would need globals; the table is read-only,
 * so sharing one mapping between threads is safe). */
static phpray_ctrl_header_t *ctrl_hdr = NULL;
static phpray_ctrl_entry_t  *ctrl_entries = NULL;
static size_t   ctrl_map_size = 0;
static ino_t    ctrl_inode = 0;
static uint64_t ctrl_last_try = 0;

uint64_t phpray_ctrl_hash(const char *s, size_t len) {
    uint64_t h = 14695981039346656037ULL;
    size_t i;
    for (i = 0; i < len; i++) {
        h ^= (unsigned char)s[i];
        h *= 1099511628211ULL;
    }
    return h;
}

void phpray_control_close(void) {
    if (ctrl_hdr) {
        munmap(ctrl_hdr, ctrl_map_size);
    }
    ctrl_hdr = NULL;
    ctrl_entries = NULL;
    ctrl_map_size = 0;
    ctrl_inode = 0;
}

int phpray_control_mapped(void) {
    return ctrl_hdr != NULL;
}

static int phpray_control_open(const char *path) {
    int fd;
    struct stat st;
    void *map;
    phpray_ctrl_header_t *hdr;

    fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return -1;
    if (fstat(fd, &st) != 0 || (size_t)st.st_size < PHPRAY_CTRL_FILE_SIZE) {
        close(fd);
        return -1;
    }
    map = mmap(NULL, PHPRAY_CTRL_FILE_SIZE, PROT_READ, MAP_SHARED, fd, 0);
    close(fd);
    if (map == MAP_FAILED) return -1;
    hdr = (phpray_ctrl_header_t *)map;
    if (hdr->magic != PHPRAY_CTRL_MAGIC || hdr->version != PHPRAY_CTRL_VERSION ||
        hdr->max_entries == 0 || hdr->max_entries > PHPRAY_CTRL_MAX_ENTRIES) {
        munmap(map, PHPRAY_CTRL_FILE_SIZE);
        return -1;
    }
    phpray_control_close();
    ctrl_hdr = hdr;
    ctrl_entries = (phpray_ctrl_entry_t *)((uint8_t *)map + sizeof(phpray_ctrl_header_t));
    ctrl_map_size = PHPRAY_CTRL_FILE_SIZE;
    ctrl_inode = st.st_ino;
    return 0;
}

void phpray_control_maybe_open(const char *path, uint64_t now) {
    struct stat st;
    if (!path || !path[0]) return;
    if (now < ctrl_last_try + PHPRAY_CTRL_RETRY_SEC && (ctrl_hdr || ctrl_last_try != 0)) {
        return;   /* mapped, or a recent failed attempt */
    }
    ctrl_last_try = now;
    if (stat(path, &st) != 0) {
        if (ctrl_hdr) phpray_control_close();   /* table removed */
        return;
    }
    if (ctrl_hdr && st.st_ino == ctrl_inode) {
        return;   /* same file still mapped */
    }
    phpray_control_open(path);   /* new or recreated file */
}

int phpray_control_lookup(const char *docroot, size_t docroot_len, uint64_t now, phpray_ctrl_match_t *out) {
    int attempt;
    uint64_t h;

    if (!ctrl_hdr || !docroot || docroot_len == 0) return 0;
    h = phpray_ctrl_hash(docroot, docroot_len);

    for (attempt = 0; attempt < 3; attempt++) {
        uint32_t s1 = __atomic_load_n(&ctrl_hdr->seq, __ATOMIC_ACQUIRE);
        uint32_t n, i, s2;
        int found = 0;
        phpray_ctrl_entry_t e;

        if (s1 & 1) continue;                    /* writer busy: try again */
        n = ctrl_hdr->entry_count;
        if (n > ctrl_hdr->max_entries) n = ctrl_hdr->max_entries;
        for (i = 0; i < n; i++) {
            const phpray_ctrl_entry_t *p = &ctrl_entries[i];
            if (p->docroot_hash != h || p->until == 0) continue;
            memcpy(&e, p, sizeof(e));
            if ((e.flags & PHPRAY_CTRL_FLAG_ACTIVE) && e.until > now) {
                found = 1;
                break;
            }
        }
        s2 = __atomic_load_n(&ctrl_hdr->seq, __ATOMIC_ACQUIRE);
        if (s1 != s2) continue;                  /* torn read: retry */
        if (!found) return 0;
        memcpy(out->url_prefix, e.url_prefix, PHPRAY_CTRL_PREFIX_LEN);
        out->url_prefix[PHPRAY_CTRL_PREFIX_LEN - 1] = '\0';
        out->sample_rate = e.sample_rate > 100 ? 100 : e.sample_rate;
        out->until = e.until;
        return 1;
    }
    return 0;
}

int phpray_control_snapshot(phpray_ctrl_entry_t *entries, int max, uint32_t *seq, uint64_t *updated_at) {
    int attempt;
    if (!ctrl_hdr) return -1;
    for (attempt = 0; attempt < 3; attempt++) {
        uint32_t s1 = __atomic_load_n(&ctrl_hdr->seq, __ATOMIC_ACQUIRE);
        uint32_t n, s2;
        if (s1 & 1) continue;
        n = ctrl_hdr->entry_count;
        if (n > ctrl_hdr->max_entries) n = ctrl_hdr->max_entries;
        if ((int)n > max) n = (uint32_t)max;
        memcpy(entries, ctrl_entries, n * sizeof(phpray_ctrl_entry_t));
        if (updated_at) *updated_at = ctrl_hdr->updated_at;
        s2 = __atomic_load_n(&ctrl_hdr->seq, __ATOMIC_ACQUIRE);
        if (s1 != s2) continue;
        if (seq) *seq = s1;
        return (int)n;
    }
    return -1;
}
