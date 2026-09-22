/*
 * PHPRay Ring Buffer — MPSC lock-free shared memory implementation
 *
 * Algorithm:
 *   Writers (PHP-FPM workers) use __atomic CAS to claim space in the ring.
 *   Reader (Go collector) advances read_pos after consuming records.
 *   write_pos and read_pos are monotonically increasing; actual offsets
 *   are computed as pos % capacity.
 *
 * Wraparound:
 *   If a record doesn't fit at the tail of the data region, a padding
 *   sentinel (PHPRAY_RECORD_PADDING) is written to fill the gap, and the
 *   actual record wraps to offset 0. This avoids splitting record headers
 *   across the boundary.
 *
 * Overflow:
 *   If the buffer is full (write_pos - read_pos >= capacity), the writer
 *   increments drop_count and returns immediately. PHP is never blocked.
 */

#include "phpray.h"
#include "ringbuffer.h"

#include <sys/mman.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <unistd.h>
#include <string.h>
#include <stdlib.h>
#include <stdio.h>
#include <errno.h>
#include <time.h>
#include <limits.h>

#ifndef PATH_MAX
#define PATH_MAX 4096
#endif

/* ---- Atomic helpers (using GCC __atomic builtins for portability) ---- */

#define ATOMIC_LOAD(ptr)         __atomic_load_n((ptr), __ATOMIC_ACQUIRE)
#define ATOMIC_STORE(ptr, val)   __atomic_store_n((ptr), (val), __ATOMIC_RELEASE)
#define ATOMIC_ADD(ptr, val)     __atomic_fetch_add((ptr), (val), __ATOMIC_RELAXED)
#define ATOMIC_CAS(ptr, expected_ptr, desired) \
    __atomic_compare_exchange_n((ptr), (expected_ptr), (desired), \
                                0 /* strong */, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE)

/* ---- Internal helpers ---- */

/* Align size up to cache line */
static size_t align_up(size_t val, size_t alignment) {
    return (val + alignment - 1) & ~(alignment - 1);
}

/* ---- Path templates (%u / %g) ---- */

/*
 * Expand "%u" (effective uid) and "%g" (effective gid) in a path template;
 * "%%" yields a literal '%', any other "%x" is copied unchanged. Returns a
 * bitmask of PHPRAY_PATH_HAS_UID / PHPRAY_PATH_HAS_GID (0 = plain path),
 * or -1 when the result does not fit in out.
 */
int phpray_path_expand(const char *tmpl, char *out, size_t outsz, uid_t uid, gid_t gid) {
    size_t o = 0;
    int flags = 0;

    if (!tmpl || !out || outsz == 0) return -1;

    for (const char *p = tmpl; *p; p++) {
        char num[24];
        const char *ins = NULL;
        size_t n = 1;

        if (p[0] == '%' && p[1] == 'u') {
            n = (size_t)snprintf(num, sizeof(num), "%lu", (unsigned long)uid);
            ins = num;
            flags |= PHPRAY_PATH_HAS_UID;
            p++;
        } else if (p[0] == '%' && p[1] == 'g') {
            n = (size_t)snprintf(num, sizeof(num), "%lu", (unsigned long)gid);
            ins = num;
            flags |= PHPRAY_PATH_HAS_GID;
            p++;
        } else if (p[0] == '%' && p[1] == '%') {
            ins = "%";
            p++;
        } else {
            ins = p;
        }

        if (o + n >= outsz) {
            out[0] = '\0';
            return -1;
        }
        memcpy(out + o, ins, n);
        o += n;
    }
    out[o] = '\0';
    return flags;
}

/* ---- Create / Attach / Open / Close ---- */

static size_t ring_header_size(void) {
    return align_up(sizeof(phpray_ring_header_t), PHPRAY_CACHELINE);
}

/* Initialise a freshly mapped region as an empty ring. */
static void ring_init_header(void *map, size_t header_size, size_t total_size) {
    phpray_ring_header_t *hdr = (phpray_ring_header_t *)map;
    memset(hdr, 0, header_size);
    hdr->magic = PHPRAY_RING_MAGIC;
    hdr->version = PHPRAY_RING_VERSION;
    hdr->capacity = total_size - header_size;
    hdr->total_size = total_size;
    ATOMIC_STORE(&hdr->write_pos, 0);
    ATOMIC_STORE(&hdr->read_pos, 0);
    ATOMIC_STORE(&hdr->record_count, 0);
    ATOMIC_STORE(&hdr->drop_count, 0);

    /* Memory fence to ensure header is visible before anyone reads */
    __atomic_thread_fence(__ATOMIC_RELEASE);
}

/* Wrap a mapping in a handle. is_owner=1 means: unlink on close (only by the
 * process that created it — a forked FPM worker inherits the handle but must
 * not remove the file the master and its siblings still use). */
static phpray_ring_t *ring_handle(void *map, size_t total_size, const char *path, int is_owner) {
    phpray_ring_t *ring = malloc(sizeof(phpray_ring_t));
    if (!ring) return NULL;
    ring->header = (phpray_ring_header_t *)map;
    ring->data = (uint8_t *)map + ring_header_size();
    ring->mmap_size = total_size;
    ring->path = strdup(path);
    ring->is_owner = is_owner;
    ring->owner_pid = is_owner ? getpid() : 0;
    return ring;
}

phpray_ring_t *phpray_ring_create(const char *path, size_t size) {
    int fd;
    void *map;
    phpray_ring_t *ring;
    size_t header_size, total_size;

    if (!path || size < PHPRAY_RING_MIN_SIZE) {
        return NULL;
    }

    header_size = ring_header_size();
    total_size = align_up(size, PHPRAY_CACHELINE);

    /* Create/truncate the shared memory file (0666 minus umask: the collector
     * and every worker that did not inherit the mapping must be able to open it) */
    fd = open(path, O_RDWR | O_CREAT | O_TRUNC | O_NOFOLLOW | O_CLOEXEC, 0666);
    if (fd < 0) {
        return NULL;
    }

    if (ftruncate(fd, (off_t)total_size) != 0) {
        close(fd);
        unlink(path);
        return NULL;
    }

    map = mmap(NULL, total_size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    close(fd); /* fd not needed after mmap */

    if (map == MAP_FAILED) {
        unlink(path);
        return NULL;
    }

    ring_init_header(map, header_size, total_size);

    ring = ring_handle(map, total_size, path, 1);
    if (!ring) {
        munmap(map, total_size);
        unlink(path);
        return NULL;
    }
    return ring;
}

/* An existing file is reusable only if it is a regular file owned by us with a
 * valid header of exactly the configured size (a different size or version means
 * the configuration changed: the ring is rebuilt under a new inode; processes
 * still mapping the old one keep it alive until they exit). */
static int ring_file_usable(int fd, size_t total_size, uid_t uid) {
    struct stat st;
    phpray_ring_header_t hdr;

    if (fstat(fd, &st) != 0) return 0;
    if (!S_ISREG(st.st_mode) || st.st_uid != uid) return 0;
    if ((size_t)st.st_size != total_size) return 0;
    if (pread(fd, &hdr, sizeof(hdr), 0) != (ssize_t)sizeof(hdr)) return 0;
    if (hdr.magic != PHPRAY_RING_MAGIC || hdr.version != PHPRAY_RING_VERSION) return 0;
    if (hdr.total_size != total_size || hdr.capacity != total_size - ring_header_size()) return 0;
    return 1;
}

/*
 * Attach to the ring shared by every process of one identity (shm_path with
 * %u / %g): open it when it exists, build it when it does not. Differences to
 * phpray_ring_create():
 *   - nothing is ever truncated in place: a new ring is prepared under a
 *     temporary name and published with link(), so a reader never sees a
 *     half-initialised header and a valid existing ring is reused, not wiped
 *     (several FPM workers of the same user attach to one file);
 *   - the file must be a regular file owned by the caller (O_NOFOLLOW + fstat):
 *     in a 1777 directory another user may have planted a file or symlink under
 *     our name — it is never written to; if we cannot unlink it, we give up;
 *   - the handle is not an owner: closing it never unlinks the file (siblings
 *     and the collector keep using it).
 */
phpray_ring_t *phpray_ring_attach(const char *path, size_t size, mode_t mode) {
    char tmp[PATH_MAX];
    size_t header_size, total_size;
    uid_t uid = geteuid();
    int attempt;

    if (!path || !path[0] || size < PHPRAY_RING_MIN_SIZE) return NULL;

    header_size = ring_header_size();
    total_size = align_up(size, PHPRAY_CACHELINE);

    if (snprintf(tmp, sizeof(tmp), "%s.%ld.tmp", path, (long)getpid()) >= (int)sizeof(tmp)) {
        return NULL;
    }

    for (attempt = 0; attempt < 3; attempt++) {
        int fd;
        void *map;
        struct stat mine, published;
        phpray_ring_t *ring;

        /* 1. Reuse the published ring if it is ours and intact. */
        fd = open(path, O_RDWR | O_NOFOLLOW | O_CLOEXEC);
        if (fd >= 0) {
            if (ring_file_usable(fd, total_size, uid)) {
                map = mmap(NULL, total_size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
                close(fd);
                if (map == MAP_FAILED) return NULL;
                ring = ring_handle(map, total_size, path, 0);
                if (!ring) munmap(map, total_size);
                return ring;
            }
            close(fd);
            /* Stale or foreign: try to make room. A foreign file in a sticky
             * directory cannot be unlinked by us — link() below then fails with
             * EEXIST and we stop rather than write into somebody else's file. */
            unlink(path);
        } else if (errno != ENOENT) {
            return NULL;   /* EACCES (not ours), ELOOP (symlink), ... */
        }

        /* 2. Build a fresh ring under a private name. */
        fd = open(tmp, O_RDWR | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, mode);
        if (fd < 0) return NULL;
        if (fchmod(fd, mode) != 0 || ftruncate(fd, (off_t)total_size) != 0) {
            close(fd);
            unlink(tmp);
            return NULL;
        }
        map = mmap(NULL, total_size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
        if (map == MAP_FAILED) {
            close(fd);
            unlink(tmp);
            return NULL;
        }
        ring_init_header(map, header_size, total_size);

        /* 3. Publish atomically; a sibling may win the race — then use its ring. */
        if (link(tmp, path) != 0) {
            int err = errno;
            munmap(map, total_size);
            close(fd);
            unlink(tmp);
            if (err != EEXIST) return NULL;
            continue;
        }
        unlink(tmp);
        if (fstat(fd, &mine) == 0 && stat(path, &published) == 0 && mine.st_ino != published.st_ino) {
            /* Replaced between link() and now (stale-ring race): attach to the winner. */
            munmap(map, total_size);
            close(fd);
            continue;
        }
        close(fd);
        ring = ring_handle(map, total_size, path, 0);
        if (!ring) munmap(map, total_size);
        return ring;
    }
    return NULL;
}

phpray_ring_t *phpray_ring_open(const char *path) {
    int fd;
    struct stat st;
    void *map;
    phpray_ring_t *ring;
    size_t header_size;

    if (!path) return NULL;

    fd = open(path, O_RDWR);
    if (fd < 0) return NULL;

    if (fstat(fd, &st) != 0 || st.st_size < (off_t)sizeof(phpray_ring_header_t)) {
        close(fd);
        return NULL;
    }

    map = mmap(NULL, (size_t)st.st_size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    close(fd);

    if (map == MAP_FAILED) return NULL;

    phpray_ring_header_t *hdr = (phpray_ring_header_t *)map;

    /* Validate magic and version */
    if (hdr->magic != PHPRAY_RING_MAGIC || hdr->version != PHPRAY_RING_VERSION) {
        munmap(map, (size_t)st.st_size);
        return NULL;
    }

    header_size = align_up(sizeof(phpray_ring_header_t), PHPRAY_CACHELINE);

    ring = malloc(sizeof(phpray_ring_t));
    if (!ring) {
        munmap(map, (size_t)st.st_size);
        return NULL;
    }

    ring->header = hdr;
    ring->data = (uint8_t *)map + header_size;
    ring->mmap_size = (size_t)st.st_size;
    ring->path = strdup(path);
    ring->is_owner = 0;
    ring->owner_pid = 0;

    return ring;
}

void phpray_ring_close(phpray_ring_t *ring) {
    if (!ring) return;

    if (ring->header) {
        munmap(ring->header, ring->mmap_size);
    }

    /* Only the creating process removes the file: a forked worker that inherited
     * the handle (FPM) must leave it for the master, its siblings and the collector. */
    if (ring->is_owner && ring->path && ring->owner_pid == getpid()) {
        unlink(ring->path);
    }

    if (ring->path) {
        free(ring->path);
    }

    free(ring);
}

/* ---- Write (lock-free, MPSC safe) ---- */

/* Oglasza naglowek rekordu w kolejnosci bezpiecznej dla czytelnika: najpierw
 * typ, potem dlugosc ze zwolnieniem bariery. Czytelnik rusza dopiero na
 * niezerowej dlugosci, wiec nigdy nie zobaczy dlugosci bez pasujacego typu. */
static inline void phpray_publish_header(uint8_t *slot, uint32_t len, uint8_t type) {
    __atomic_store_n(slot + offsetof(phpray_ring_record_t, record_type),
                     type, __ATOMIC_RELAXED);
    __atomic_store_n((uint32_t *)(void *)slot, len, __ATOMIC_RELEASE);
}

int phpray_ring_write(phpray_ring_t *ring, const void *data, uint32_t len, uint8_t type) {
    phpray_ring_header_t *hdr;
    phpray_ring_record_t rec_hdr;
    uint32_t total, padded_total;
    uint64_t capacity;
    uint64_t write_pos, read_pos, new_write_pos;
    uint64_t offset, tail_space;

    if (!ring || !ring->header || !data || len == 0) return -1;
    if (len > PHPRAY_RING_MAX_RECORD) return -1;

    hdr = ring->header;
    capacity = hdr->capacity;
    total = sizeof(phpray_ring_record_t) + len;

    /* Align total to 8 bytes for safe atomic access patterns */
    padded_total = (total + 7) & ~(uint32_t)7;

    /* CAS loop to claim space */
    for (;;) {
        write_pos = ATOMIC_LOAD(&hdr->write_pos);
        read_pos = ATOMIC_LOAD(&hdr->read_pos);

        /* Check if buffer has enough free space.
         * Free space = capacity - (write_pos - read_pos).
         * We need padded_total bytes plus room for a potential padding record. */
        uint64_t used = write_pos - read_pos;
        if (used + padded_total + sizeof(phpray_ring_record_t) > capacity) {
            /* Buffer full — drop this record */
            ATOMIC_ADD(&hdr->drop_count, 1);
            return -1;
        }

        offset = write_pos % capacity;
        tail_space = capacity - offset;

        if (tail_space < sizeof(phpray_ring_record_t)) {
            /* Not even a record header fits — write padding and wrap.
             * Claim the tail_space to skip it. */
            new_write_pos = write_pos + tail_space;

            /* Check that wrapping + our record still fits */
            uint64_t used_after_pad = new_write_pos - read_pos;
            if (used_after_pad + padded_total > capacity) {
                ATOMIC_ADD(&hdr->drop_count, 1);
                return -1;
            }

            if (ATOMIC_CAS(&hdr->write_pos, &write_pos, new_write_pos)) {
                /* Write padding sentinel in the gap */
                if (tail_space >= sizeof(phpray_ring_record_t)) {
                    phpray_publish_header(ring->data + offset,
                                          (uint32_t)tail_space, PHPRAY_RECORD_PADDING);
                } else {
                    /* Gap smaller than header — just zero it */
                    memset(ring->data + offset, 0, (size_t)tail_space);
                }
                /* Retry with write_pos now at offset 0 */
                continue;
            }
            /* CAS failed, another writer got there first — retry */
            continue;
        }

        if (tail_space < padded_total) {
            /* Record doesn't fit contiguously — write padding sentinel for the
             * remaining space and wrap to offset 0. */
            uint64_t pad_size = tail_space;
            new_write_pos = write_pos + pad_size;

            uint64_t used_after_pad = new_write_pos - read_pos;
            if (used_after_pad + padded_total > capacity) {
                ATOMIC_ADD(&hdr->drop_count, 1);
                return -1;
            }

            if (ATOMIC_CAS(&hdr->write_pos, &write_pos, new_write_pos)) {
                /* Write padding sentinel */
                phpray_publish_header(ring->data + offset,
                                      (uint32_t)pad_size, PHPRAY_RECORD_PADDING);
                continue;
            }
            continue;
        }

        /* Record fits contiguously at offset. Claim it. */
        new_write_pos = write_pos + padded_total;
        if (ATOMIC_CAS(&hdr->write_pos, &write_pos, new_write_pos)) {
            break; /* Success! */
        }
        /* CAS failed — retry */
    }

    /* We own the region [offset .. offset + padded_total).
     *
     * Kolejnosc zapisow jest tu cala poprawnoscia. CAS powyzej juz przesunal
     * write_pos, wiec od tej chwili czytelnik uwaza, ze w tym slocie cos jest —
     * a lezy tam jeszcze tresc z poprzedniego okrazenia bufora. Publikujemy
     * wiec etapami:
     *
     *   1. typ = RESERVED   — slot zaklepany, czytelnik ma czekac,
     *   2. dlugosc          — od teraz czytelnik zna granice rekordu,
     *   3. dane,
     *   4. typ = prawdziwy  — ze zwolnieniem bariery; to jest publikacja.
     *
     * Czytelnik przepuszcza rekord dopiero, gdy dlugosc jest niezerowa, a typ
     * nie jest RESERVED; po konsumpcji zeruje naglowek, wiec niezapisany slot
     * zawsze ma dlugosc 0. Bez tego mozna bylo odczytac polowe starego rekordu
     * jako nowy: 21.09.2026 trafil tak do bazy slad z tekstem SQL w polu host,
     * statusem 28521 i czasem 8 529 657 644 052 ms.
     */
    (void)rec_hdr;

    phpray_publish_header(ring->data + offset, total, PHPRAY_RECORD_RESERVED);

    memcpy(ring->data + offset + sizeof(phpray_ring_record_t), data, len);

    /* Zero any alignment padding */
    if (padded_total > total) {
        memset(ring->data + offset + total, 0, padded_total - total);
    }

    __atomic_store_n(ring->data + offset + offsetof(phpray_ring_record_t, record_type),
                     type, __ATOMIC_RELEASE);

    ATOMIC_ADD(&hdr->record_count, 1);
    return 0;
}

/* ---- Read (single consumer) ---- */

/* Kasuje naglowek skonsumowanego rekordu. Wywolywane zawsze PRZED przesunieciem
 * read_pos, bo dopiero przesuniecie oddaje to miejsce pisarzowi. */
static inline void phpray_clear_header(uint8_t *slot) {
    __atomic_store_n((uint32_t *)(void *)slot, 0u, __ATOMIC_RELAXED);
    __atomic_store_n(slot + offsetof(phpray_ring_record_t, record_type),
                     (uint8_t)0, __ATOMIC_RELEASE);
}

int phpray_ring_read(phpray_ring_t *ring, void *buf, uint32_t buf_size,
                     uint8_t *type, uint32_t *out_len) {
    phpray_ring_header_t *hdr;
    phpray_ring_record_t rec_hdr;
    uint64_t read_pos, write_pos, offset, capacity;
    uint32_t payload_len, padded_total;

    if (!ring || !ring->header) return -1;

    hdr = ring->header;
    capacity = hdr->capacity;

    /* Acquire fence to see writer's data */
    __atomic_thread_fence(__ATOMIC_ACQUIRE);

    for (;;) {
        read_pos = ATOMIC_LOAD(&hdr->read_pos);
        write_pos = ATOMIC_LOAD(&hdr->write_pos);

        if (read_pos >= write_pos) {
            return -1; /* No data available */
        }

        offset = read_pos % capacity;

        /* Check if we can read a full record header */
        uint64_t tail_space = capacity - offset;
        if (tail_space < sizeof(phpray_ring_record_t)) {
            /* Skip past this gap (was padding) */
            ATOMIC_STORE(&hdr->read_pos, read_pos + tail_space);
            continue;
        }

        /* Read record header. Dlugosc z przejeciem bariery: pisarz zapisuje ja
         * jako ostatnia z pary (typ, dlugosc), wiec niezerowa dlugosc gwarantuje
         * widoczny i pasujacy typ. */
        rec_hdr.record_len = __atomic_load_n((uint32_t *)(void *)(ring->data + offset),
                                             __ATOMIC_ACQUIRE);
        rec_hdr.record_type = __atomic_load_n(
            ring->data + offset + offsetof(phpray_ring_record_t, record_type),
            __ATOMIC_RELAXED);

        /* Dlugosc 0 = slot zaklepany przez pisarza, ale jeszcze nieopisany.
         * Czytelnik zeruje naglowek po konsumpcji, wiec to jedyna przyczyna. */
        if (rec_hdr.record_len == 0) {
            return -1; /* Jeszcze nie teraz — nie wolno tego przeskoczyc */
        }
        if (rec_hdr.record_len > capacity) {
            /* Corrupt record — skip 8 bytes and try again */
            ATOMIC_STORE(&hdr->read_pos, read_pos + 8);
            continue;
        }

        padded_total = (rec_hdr.record_len + 7) & ~(uint32_t)7;

        /* Dane jeszcze w locie — granice rekordu juz znamy, tresci nie. */
        if (rec_hdr.record_type == PHPRAY_RECORD_RESERVED) {
            return -1;
        }

        /* Skip padding sentinels */
        if (rec_hdr.record_type == PHPRAY_RECORD_PADDING) {
            phpray_clear_header(ring->data + offset);
            ATOMIC_STORE(&hdr->read_pos, read_pos + padded_total);
            continue;
        }

        payload_len = rec_hdr.record_len - sizeof(phpray_ring_record_t);

        /* Check caller's buffer size */
        if (buf && buf_size < payload_len) {
            if (out_len) *out_len = payload_len;
            return -2; /* Buffer too small */
        }

        /* Copy payload to caller's buffer */
        if (buf && payload_len > 0) {
            memcpy(buf, ring->data + offset + sizeof(rec_hdr), payload_len);
        }

        if (type) *type = rec_hdr.record_type;
        if (out_len) *out_len = payload_len;

        /* Zerujemy naglowek ZANIM oddamy miejsce pisarzowi — dzieki temu slot,
         * ktory pisarz dopiero zaklepal, ma dlugosc 0, a nie resztke po nas. */
        phpray_clear_header(ring->data + offset);

        /* Advance read position */
        ATOMIC_STORE(&hdr->read_pos, read_pos + padded_total);

        return 0;
    }
}

/* ---- Stats ---- */

uint64_t phpray_ring_records(phpray_ring_t *ring) {
    if (!ring || !ring->header) return 0;
    return ATOMIC_LOAD(&ring->header->record_count);
}

uint64_t phpray_ring_drops(phpray_ring_t *ring) {
    if (!ring || !ring->header) return 0;
    return ATOMIC_LOAD(&ring->header->drop_count);
}

double phpray_ring_fill_pct(phpray_ring_t *ring) {
    uint64_t used, capacity;
    if (!ring || !ring->header) return 0.0;

    capacity = ring->header->capacity;
    if (capacity == 0) return 0.0;

    uint64_t wp = ATOMIC_LOAD(&ring->header->write_pos);
    uint64_t rp = ATOMIC_LOAD(&ring->header->read_pos);
    used = (wp >= rp) ? (wp - rp) : 0;

    return (double)used / (double)capacity * 100.0;
}

uint64_t phpray_ring_capacity(phpray_ring_t *ring) {
    if (!ring || !ring->header) return 0;
    return ring->header->capacity;
}

/* ---- Request serialization ---- */

uint32_t phpray_serialize_request(const void *req_ptr, void *buf, uint32_t buf_size) {
    const phpray_request_t *req = (const phpray_request_t *)req_ptr;
    phpray_trace_record_t rec;
    uint32_t total;
    uint16_t server_name_len, request_uri_len, request_method_len, phpray_id_len;
    uint16_t sq_count, sh_count, sm_count, se_count;
    uint8_t *out;

    if (!req || !buf) return 0;

    /* Measure string lengths */
    server_name_len = (uint16_t)strlen(req->server_name);
    request_uri_len = (uint16_t)strlen(req->request_uri);
    request_method_len = (uint16_t)strlen(req->request_method);
    phpray_id_len = (uint16_t)strlen(req->phpray_id);
    uint8_t php_version_len = (uint8_t)strlen(PHP_VERSION);
    uint16_t docroot_len = (uint16_t)strlen(req->docroot);

    /* Determine how many detail records to serialize based on trace level */
    sq_count = 0;
    sh_count = 0;
    sm_count = req->mark_count;  /* always serialize marks */
    se_count = req->error_count; /* always serialize errors */

    if (req->trace_level >= PHPRAY_TRACE_FULL && req->queries) {
        sq_count = req->query_count < req->queries_alloc ? 
                   req->query_count : req->queries_alloc;
    } else if (req->trace_level >= PHPRAY_TRACE_NORMAL && req->queries) {
        sq_count = req->query_count < req->queries_alloc ?
                   req->query_count : req->queries_alloc;
        if (sq_count > 10) sq_count = 10; /* top 10 for normal level */
    }

    if (req->trace_level >= PHPRAY_TRACE_NORMAL && req->http_calls) {
        sh_count = req->http_call_count < req->http_calls_alloc ?
                   req->http_call_count : req->http_calls_alloc;
    }

    /* Calculate total size */
    total = (uint32_t)sizeof(phpray_trace_record_t) +
            server_name_len + request_uri_len + request_method_len + phpray_id_len +
            php_version_len + docroot_len;

    /* Add query records (including backtrace frames) */
    for (uint16_t i = 0; i < sq_count; i++) {
        total += (uint32_t)sizeof(phpray_serial_query_t) + 
                 (uint32_t)strlen(req->queries[i].sql);
        /* Add backtrace frames */
        for (uint8_t b = 0; b < req->queries[i].bt_count; b++) {
            total += (uint32_t)sizeof(phpray_serial_frame_t) +
                     (uint32_t)strlen(req->queries[i].bt[b].file);
        }
    }

    /* Add HTTP call records (including backtrace frames) */
    for (uint16_t i = 0; i < sh_count; i++) {
        total += (uint32_t)sizeof(phpray_serial_http_t) +
                 (uint32_t)strlen(req->http_calls[i].url);
        /* Add backtrace frames */
        for (uint8_t b = 0; b < req->http_calls[i].bt_count; b++) {
            total += (uint32_t)sizeof(phpray_serial_frame_t) +
                     (uint32_t)strlen(req->http_calls[i].bt[b].file);
        }
    }

    /* Add mark records */
    for (uint16_t i = 0; i < sm_count; i++) {
        total += (uint32_t)sizeof(phpray_serial_mark_t) +
                 (uint32_t)strlen(req->marks[i].name);
    }

    /* Add error records */
    for (uint16_t i = 0; i < se_count; i++) {
        total += (uint32_t)sizeof(phpray_serial_error_t) +
                 (uint32_t)strlen(req->errors[i].message);
    }

    /* Determine component count: the full profile of a profiled request
     * (already sorted by inclusive time in phpray_profiler_request_finish) */
    uint16_t sc_count = 0;
    if (req->profiled && req->prof) {
        sc_count = req->prof->comp_count;
    }

    /* Add component records */
    for (uint16_t i = 0; i < sc_count; i++) {
        total += (uint32_t)sizeof(phpray_serial_component_t) +
                 (uint32_t)strlen(req->prof->comps[i].name);
    }

    if (total > buf_size) return 0;

    /* Fill fixed header */
    memset(&rec, 0, sizeof(rec));
    rec.request_id = req->request_id;
    rec.uid = req->uid;
    rec.pid = req->pid;
    rec.timestamp = (uint64_t)time(NULL);
    rec.duration_ns = req->duration_ns;
    rec.cpu_user_ns = req->cpu_user_ns;
    rec.cpu_sys_ns = req->cpu_sys_ns;
    rec.response_code = req->response_code;
    rec.memory_peak = req->memory_peak;
    rec.query_count = req->query_count;
    rec.db_total_ns = req->db_total_ns;
    rec.http_call_count = req->http_call_count;
    rec.http_total_ns = req->http_total_ns;
    rec.file_op_count = req->file_op_count;
    rec.file_total_ns = req->file_total_ns;
    rec.redis_call_count = req->redis_call_count;
    rec.redis_total_ns = req->redis_total_ns;
    rec.wp_detected = req->wp_detected;
    rec.trace_level = req->trace_level;
    rec.mark_count = req->mark_count;
    rec.error_count = req->error_count;
    rec.n_plus_one = req->n_plus_one;
    rec.server_name_len = server_name_len;
    rec.request_uri_len = request_uri_len;
    rec.request_method_len = request_method_len;
    rec.phpray_id_len = phpray_id_len;
    rec.php_version_len = php_version_len;
    rec.serialized_query_count = sq_count;
    rec.serialized_http_count = sh_count;
    rec.serialized_mark_count = sm_count;
    rec.serialized_error_count = se_count;
    rec.serialized_component_count = sc_count;
    rec.app = req->app;
    rec.profiled = req->profiled ? 1 : 0;
    rec.prof_overflow = (req->prof && req->prof->overflow) ? 1 : 0;
    rec.docroot_len = docroot_len;

    out = (uint8_t *)buf;

    /* Write fixed header */
    memcpy(out, &rec, sizeof(rec));
    out += sizeof(rec);

    /* Write variable strings */
    if (server_name_len > 0) {
        memcpy(out, req->server_name, server_name_len);
        out += server_name_len;
    }
    if (request_uri_len > 0) {
        memcpy(out, req->request_uri, request_uri_len);
        out += request_uri_len;
    }
    if (request_method_len > 0) {
        memcpy(out, req->request_method, request_method_len);
        out += request_method_len;
    }
    if (phpray_id_len > 0) {
        memcpy(out, req->phpray_id, phpray_id_len);
        out += phpray_id_len;
    }
    if (php_version_len > 0) {
        memcpy(out, PHP_VERSION, php_version_len);
        out += php_version_len;
    }
    if (docroot_len > 0) {
        memcpy(out, req->docroot, docroot_len);
        out += docroot_len;
    }

    /* Write query records (with backtrace frames) */
    for (uint16_t i = 0; i < sq_count; i++) {
        phpray_serial_query_t sq;
        uint16_t sql_len = (uint16_t)strlen(req->queries[i].sql);
        sq.sql_len = sql_len;
        sq.duration_ns = req->queries[i].duration_ns;
        sq.offset_ns = req->queries[i].offset_ns;
        sq.affected_rows = req->queries[i].affected_rows;
        sq.source = req->queries[i].source;
        sq.bt_count = req->queries[i].bt_count;
        memcpy(out, &sq, sizeof(sq));
        out += sizeof(sq);
        if (sql_len > 0) {
            memcpy(out, req->queries[i].sql, sql_len);
            out += sql_len;
        }
        /* Write backtrace frames */
        for (uint8_t b = 0; b < sq.bt_count; b++) {
            phpray_serial_frame_t sf;
            uint8_t file_len = (uint8_t)strlen(req->queries[i].bt[b].file);
            sf.file_len = file_len;
            sf.line = req->queries[i].bt[b].line;
            memcpy(out, &sf, sizeof(sf));
            out += sizeof(sf);
            if (file_len > 0) {
                memcpy(out, req->queries[i].bt[b].file, file_len);
                out += file_len;
            }
        }
    }

    /* Write HTTP call records (with backtrace frames) */
    for (uint16_t i = 0; i < sh_count; i++) {
        phpray_serial_http_t sh;
        uint16_t url_len = (uint16_t)strlen(req->http_calls[i].url);
        sh.url_len = url_len;
        sh.duration_ns = req->http_calls[i].duration_ns;
        sh.offset_ns = req->http_calls[i].offset_ns;
        sh.response_code = req->http_calls[i].response_code;
        sh.bt_count = req->http_calls[i].bt_count;
        memcpy(out, &sh, sizeof(sh));
        out += sizeof(sh);
        if (url_len > 0) {
            memcpy(out, req->http_calls[i].url, url_len);
            out += url_len;
        }
        /* Write backtrace frames */
        for (uint8_t b = 0; b < sh.bt_count; b++) {
            phpray_serial_frame_t sf;
            uint8_t file_len = (uint8_t)strlen(req->http_calls[i].bt[b].file);
            sf.file_len = file_len;
            sf.line = req->http_calls[i].bt[b].line;
            memcpy(out, &sf, sizeof(sf));
            out += sizeof(sf);
            if (file_len > 0) {
                memcpy(out, req->http_calls[i].bt[b].file, file_len);
                out += file_len;
            }
        }
    }

    /* Write mark records */
    for (uint16_t i = 0; i < sm_count; i++) {
        phpray_serial_mark_t sm;
        uint8_t name_len = (uint8_t)strlen(req->marks[i].name);
        sm.name_len = name_len;
        sm.offset_ns = req->marks[i].offset_ns;
        memcpy(out, &sm, sizeof(sm));
        out += sizeof(sm);
        if (name_len > 0) {
            memcpy(out, req->marks[i].name, name_len);
            out += name_len;
        }
    }

    /* Write error records */
    for (uint16_t i = 0; i < se_count; i++) {
        phpray_serial_error_t serr;
        uint16_t msg_len = (uint16_t)strlen(req->errors[i].message);
        serr.msg_len = msg_len;
        serr.type = req->errors[i].type;
        serr.offset_ns = req->errors[i].offset_ns;
        memcpy(out, &serr, sizeof(serr));
        out += sizeof(serr);
        if (msg_len > 0) {
            memcpy(out, req->errors[i].message, msg_len);
            out += msg_len;
        }
    }

    /* Write component records (sorted by inclusive time, descending) */
    for (uint16_t i = 0; i < sc_count; i++) {
        phpray_serial_component_t sc;
        const phpray_comp_t *c = &req->prof->comps[i];
        uint8_t name_len = (uint8_t)strlen(c->name);
        sc.name_len = name_len;
        sc.incl_ns = c->incl_ns;
        sc.self_ns = c->self_ns;
        sc.call_count = c->calls;
        memcpy(out, &sc, sizeof(sc));
        out += sizeof(sc);
        if (name_len > 0) {
            memcpy(out, c->name, name_len);
            out += name_len;
        }
    }

    return total;
}
