/*
 * PHPRay Ring Buffer — Lock-free MPSC shared memory ring buffer
 *
 * MPSC = Multiple Producers (PHP-FPM workers), Single Consumer (Go collector).
 * Uses an mmap'd file on tmpfs (/dev/shm, /run/phpray) for zero-copy IPC.
 * Lock-free writes via __atomic CAS on write_pos.
 * Variable-length records with wraparound handling.
 *
 * Two ways to get a ring:
 *   phpray_ring_create() — one ring for the whole server, created at MINIT
 *                          (0666 minus umask), removed by its creator on shutdown;
 *   phpray_ring_attach() — one ring per identity (phpray.shm_path with %u / %g),
 *                          opened or built lazily by each worker, never removed.
 */

#ifndef PHPRAY_RINGBUFFER_H
#define PHPRAY_RINGBUFFER_H

#include <stdint.h>
#include <stddef.h>
#include <sys/types.h>

#define PHPRAY_RING_MAGIC   0x50485259  /* "PHRY" */
/* Uwaga: protokol publikacji rekordu (typ RESERVED -> dlugosc -> dane -> typ)
 * NIE zmienia ukladu bajtow, wiec wersja zostaje 5. Podbicie zerwaloby ring
 * z kolektorami, ktore klienci maja juz zainstalowane (OpenRing odrzuca
 * nieznana wersje, czyli caly ruch przestalby byc zapisywany). Stary kolektor
 * z nowym rozszerzeniem najwyzej pominie rekord zlapany w trakcie zapisu. */
#define PHPRAY_RING_VERSION 5   /* v4: app + profiled + incl/self per component; v5: docroot string */

/* Alignment for cache-line avoidance of false sharing */
#define PHPRAY_CACHELINE 64

/* Record types */
#define PHPRAY_RECORD_TRACE   0  /* request trace */
#define PHPRAY_RECORD_PADDING 0xFF  /* padding sentinel for wraparound */
/* Slot zaklepany przez pisarza, dane jeszcze nie zapisane. Czytelnik, ktory to
 * zobaczy, MUSI sie zatrzymac i poczekac — bajty pod spodem sa smieciem po
 * poprzednim okrazeniu bufora. Pisarz nadpisuje ten bajt prawdziwym typem
 * dopiero po skopiowaniu calej zawartosci (patrz phpray_ring_write). */
#define PHPRAY_RECORD_RESERVED 0xFE

/* Minimum useful shm size (header + at least 64KB data) */
#define PHPRAY_RING_MIN_SIZE (sizeof(phpray_ring_header_t) + 65536)

/* Maximum record size (prevent absurd single records) */
#define PHPRAY_RING_MAX_RECORD (1 << 20) /* 1MB */

/*
 * Ring buffer header — lives at the start of the mmap'd region.
 *
 * Layout:  [header (128 bytes)] [data region (capacity bytes)]
 *
 * write_pos and read_pos are absolute byte offsets into the data region,
 * monotonically increasing. Actual offset = pos % capacity.
 * This avoids the "full vs empty" ambiguity of traditional ring buffers.
 */
typedef struct {
    /* Identification — first cache line */
    uint32_t magic;           /* PHPRAY_RING_MAGIC */
    uint32_t version;         /* PHPRAY_RING_VERSION */
    uint64_t capacity;        /* data region size in bytes */
    uint64_t total_size;      /* total mmap size (header + data) */
    uint8_t  _pad0[PHPRAY_CACHELINE - 24];

    /* Writer state — own cache line to avoid false sharing */
    uint64_t write_pos;       /* monotonic write position (use __atomic) */
    uint64_t record_count;    /* total records written (use __atomic) */
    uint64_t drop_count;      /* records dropped due to full buffer (use __atomic) */
    uint8_t  _pad1[PHPRAY_CACHELINE - 24];

    /* Reader state — own cache line */
    uint64_t read_pos;        /* monotonic read position (use __atomic) */
    uint8_t  _pad2[PHPRAY_CACHELINE - 8];
} __attribute__((aligned(PHPRAY_CACHELINE))) phpray_ring_header_t;

/*
 * Record header — prepended to each data payload.
 * Packed to minimize overhead per record.
 */
typedef struct __attribute__((packed)) {
    uint32_t record_len;  /* total length: sizeof(this header) + payload */
    uint8_t  record_type; /* PHPRAY_RECORD_TRACE, etc. */
} phpray_ring_record_t;

/*
 * Binary trace record — serialized form of phpray_request_t for the ring buffer.
 * Fixed-size header followed by variable-length string data.
 *
 * Wire format:
 *   [phpray_ring_record_t (5 bytes)]
 *   [phpray_trace_record_t (fixed header)]
 *   [server_name (server_name_len bytes)]
 *   [request_uri (request_uri_len bytes)]
 *   [request_method (request_method_len bytes)]
 *   [phpray_id (phpray_id_len bytes)]
 *   [php_version (php_version_len bytes)]  (v2+)
 *   [docroot (docroot_len bytes)]          (v5+)
 *   [query records...]
 *   [http records...]
 *   [mark records...]
 *   [error records...]
 *   [component records...]
 */
typedef struct __attribute__((packed)) {
    /* Identity */
    uint64_t request_id;
    uint32_t uid;
    uint32_t pid;

    /* Timing */
    uint64_t timestamp;       /* unix epoch seconds */
    uint64_t duration_ns;
    uint64_t cpu_user_ns;
    uint64_t cpu_sys_ns;

    /* Request info */
    uint16_t response_code;
    uint64_t memory_peak;
    uint16_t query_count;
    uint64_t db_total_ns;
    uint16_t http_call_count;
    uint64_t http_total_ns;
    uint16_t file_op_count;
    uint64_t file_total_ns;
    uint16_t redis_call_count;
    uint64_t redis_total_ns;
    uint8_t  wp_detected;
    uint8_t  trace_level;
    uint16_t mark_count;
    uint16_t error_count;

    /* N+1 detection */
    uint8_t  n_plus_one;     /* 1 if N+1 query pattern detected */

    /* Variable-length string lengths (data follows this struct) */
    uint16_t server_name_len;
    uint16_t request_uri_len;
    uint16_t request_method_len;
    uint16_t phpray_id_len;
    uint8_t  php_version_len;   /* length of PHP version string (e.g. "8.3.12") */

    /* Variable-length section counts (data follows strings) */
    uint16_t serialized_query_count;    /* 0 for summary, top5 for normal, all for full+ */
    uint16_t serialized_http_count;
    uint16_t serialized_mark_count;
    uint16_t serialized_error_count;
    uint16_t serialized_component_count;

    /* v4 */
    uint8_t  app;             /* PHPRAY_APP_* */
    uint8_t  profiled;        /* 1 = function profile collected for this request */
    uint8_t  prof_overflow;   /* 1 = profile frame stack overflowed (self times approximate) */

    /* v5: DOCUMENT_ROOT string follows php_version (docroot_len bytes) */
    uint16_t docroot_len;
} phpray_trace_record_t;

/*
 * Serialized backtrace frame (used within query and HTTP call records).
 * Each frame: [phpray_serial_frame_t][file_text (file_len bytes)]
 */
typedef struct __attribute__((packed)) {
    uint8_t  file_len;
    uint32_t line;
} phpray_serial_frame_t;

/*
 * Serialized query record (follows strings in the wire format).
 * Each query: [phpray_serial_query_t][sql_text (sql_len bytes)][bt_frames...]
 * If bt_count > 0, bt_count frame records follow the sql text.
 */
typedef struct __attribute__((packed)) {
    uint16_t sql_len;
    uint64_t duration_ns;
    uint64_t offset_ns;
    uint32_t affected_rows;
    uint8_t  source;
    uint8_t  bt_count;     /* number of backtrace frames following sql text */
} phpray_serial_query_t;

/*
 * Serialized HTTP call record.
 * Each HTTP call: [phpray_serial_http_t][url_text (url_len bytes)][bt_frames...]
 * If bt_count > 0, bt_count frame records follow the url text.
 */
typedef struct __attribute__((packed)) {
    uint16_t url_len;
    uint64_t duration_ns;
    uint64_t offset_ns;
    uint16_t response_code;
    uint8_t  bt_count;     /* number of backtrace frames following url text */
} phpray_serial_http_t;

/*
 * Serialized mark record.
 */
typedef struct __attribute__((packed)) {
    uint8_t  name_len;
    uint64_t offset_ns;
} phpray_serial_mark_t;

/*
 * Serialized error record.
 */
typedef struct __attribute__((packed)) {
    uint16_t msg_len;
    int32_t  type;
    uint64_t offset_ns;
} phpray_serial_error_t;

/*
 * Serialized component record (function profile, v4).
 * Each component: [phpray_serial_component_t][name_text (name_len bytes)]
 * v3 layout was {name_len u8, total_ns u64, call_count u32}.
 */
typedef struct __attribute__((packed)) {
    uint8_t  name_len;
    uint64_t incl_ns;      /* inclusive wall time at the component boundary */
    uint64_t self_ns;      /* time in the component's own frames (nested observed frames excluded) */
    uint32_t call_count;
} phpray_serial_component_t;

/* Opaque handle returned by create/open */
typedef struct {
    phpray_ring_header_t *header;
    uint8_t              *data;     /* points to header + sizeof(header) */
    size_t                mmap_size;
    char                 *path;     /* strdup'd path for unlink */
    int                   is_owner; /* 1 if we created it (responsible for unlink) */
    pid_t                 owner_pid; /* creator pid: only that process unlinks on close */
} phpray_ring_t;

/* ---- API ---- */

/* Create a new ring buffer (producer/owner side), truncating any existing file.
 * Returns NULL on failure. */
phpray_ring_t *phpray_ring_create(const char *path, size_t size);

/* Open the ring at path if it exists and is ours (regular file owned by the
 * effective uid, valid header, exactly `size` bytes), otherwise build it with
 * the given mode (e.g. 0600 for a private per-user ring) and publish it
 * atomically. Safe to call concurrently from several workers of one user.
 * The returned handle never unlinks the file on close. Returns NULL on failure. */
phpray_ring_t *phpray_ring_attach(const char *path, size_t size, mode_t mode);

/* Open an existing ring buffer (consumer side).
 * Returns NULL on failure. */
phpray_ring_t *phpray_ring_open(const char *path);

/* Write data to the ring buffer. Lock-free, safe from multiple processes.
 * Returns 0 on success, -1 if buffer full (record dropped). */
int phpray_ring_write(phpray_ring_t *ring, const void *data, uint32_t len, uint8_t type);

/* Read one record from the ring buffer. Single consumer only.
 * Returns 0 on success, -1 if no data available, -2 if buf_size too small. */
int phpray_ring_read(phpray_ring_t *ring, void *buf, uint32_t buf_size,
                     uint8_t *type, uint32_t *out_len);

/* Close and optionally unlink the ring buffer. */
void phpray_ring_close(phpray_ring_t *ring);

/* ---- Stats ---- */
uint64_t phpray_ring_records(phpray_ring_t *ring);
uint64_t phpray_ring_drops(phpray_ring_t *ring);
double   phpray_ring_fill_pct(phpray_ring_t *ring);
uint64_t phpray_ring_capacity(phpray_ring_t *ring);

/* ---- Serialization ---- */

/* Serialize a phpray_request_t into a binary trace record.
 * Caller must provide a buffer of sufficient size.
 * Returns the number of bytes written, or 0 on error. */
uint32_t phpray_serialize_request(const void *request, void *buf, uint32_t buf_size);

#endif /* PHPRAY_RINGBUFFER_H */
