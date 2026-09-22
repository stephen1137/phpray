/*
 * PHPRay control table — shared-memory table written by the collector (from
 * console / DirectAdmin "profile URL X for N minutes" requests) and read by the
 * extension at RINIT. Keyed by DOCUMENT_ROOT (FNV-1a 64 hash), so a site can be
 * profiled on demand without touching php.ini.
 *
 * Layout (all little-endian, packed; mirrored in src/collector/control_table.go):
 *
 *   [phpray_ctrl_header_t (64 bytes)]
 *   [phpray_ctrl_entry_t × max_entries (160 bytes each)]
 *
 * Single writer (collector), many readers (PHP workers). Consistency: seqlock —
 * the writer increments `seq` to an odd value before writing and to an even value
 * after; a reader takes a snapshot only when `seq` is even and unchanged across
 * the scan. Readers map the file read-only and never modify it.
 */

#ifndef PHPRAY_CONTROL_H
#define PHPRAY_CONTROL_H

#include <stdint.h>
#include <stddef.h>

#define PHPRAY_CTRL_MAGIC        0x50485243u  /* "PHRC" */
#define PHPRAY_CTRL_VERSION      1
#define PHPRAY_CTRL_MAX_ENTRIES  256
#define PHPRAY_CTRL_PREFIX_LEN   136
#define PHPRAY_CTRL_RETRY_SEC    5           /* how often a worker retries mapping a missing table */

typedef struct __attribute__((packed)) {
    uint32_t magic;
    uint32_t version;
    uint32_t seq;           /* seqlock; odd = write in progress */
    uint32_t entry_count;   /* slots in use (scan bound) */
    uint32_t max_entries;
    uint32_t reserved;
    uint64_t updated_at;    /* unix time of the last write */
    uint8_t  _pad[64 - 32];
} phpray_ctrl_header_t;

typedef struct __attribute__((packed)) {
    uint64_t docroot_hash;  /* FNV-1a 64 of DOCUMENT_ROOT */
    uint64_t until;         /* unix seconds; 0 = empty slot */
    uint16_t sample_rate;   /* 0–100 % of matching requests to profile */
    uint16_t flags;         /* bit 0 = active */
    uint32_t reserved;
    char     url_prefix[PHPRAY_CTRL_PREFIX_LEN]; /* NUL-terminated; "" = every URI */
} phpray_ctrl_entry_t;

#define PHPRAY_CTRL_FLAG_ACTIVE  1
#define PHPRAY_CTRL_FILE_SIZE    (sizeof(phpray_ctrl_header_t) + PHPRAY_CTRL_MAX_ENTRIES * sizeof(phpray_ctrl_entry_t))

/* Result of a lookup */
typedef struct {
    char     url_prefix[PHPRAY_CTRL_PREFIX_LEN];
    uint16_t sample_rate;
    uint64_t until;
} phpray_ctrl_match_t;

uint64_t phpray_ctrl_hash(const char *s, size_t len);

/* Map / remap the table if needed (cheap: at most one stat() per PHPRAY_CTRL_RETRY_SEC). */
void phpray_control_maybe_open(const char *path, uint64_t now);
void phpray_control_close(void);
int  phpray_control_mapped(void);

/* 1 if an active, unexpired entry matches docroot; fills *out. */
int  phpray_control_lookup(const char *docroot, size_t docroot_len, uint64_t now, phpray_ctrl_match_t *out);

/* Snapshot for phpray_control_status(); returns number of entries copied. */
int  phpray_control_snapshot(phpray_ctrl_entry_t *entries, int max, uint32_t *seq, uint64_t *updated_at);

#endif /* PHPRAY_CONTROL_H */
