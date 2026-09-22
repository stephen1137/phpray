/*
 * LD_PRELOAD shim for running php + phpray.so under AddressSanitizer.
 *
 * PHP dlopen()s extensions with RTLD_DEEPBIND (Zend/zend_portability.h, unless PHP
 * itself was built with ASan) and the sanitizer runtime refuses such dlopen calls
 * (https://github.com/google/sanitizers/issues/611). Preloading this shim BEFORE
 * libasan strips the flag; libasan's own dlopen interceptor (next in the chain)
 * still sees the call. Use with ASAN_OPTIONS=verify_asan_link_order=0.
 *
 *   gcc -shared -fPIC -o nodeepbind.so nodeepbind.c -ldl
 *   LD_PRELOAD="nodeepbind.so:$(gcc -print-file-name=libasan.so)" php ...
 */
#define _GNU_SOURCE
#include <dlfcn.h>

typedef void *(*dlopen_fn_t)(const char *, int);

void *dlopen(const char *filename, int flags) {
    static dlopen_fn_t real_dlopen;
    if (!real_dlopen) {
        real_dlopen = (dlopen_fn_t)dlsym(RTLD_NEXT, "dlopen");
    }
#ifdef RTLD_DEEPBIND
    flags &= ~RTLD_DEEPBIND;
#endif
    return real_dlopen(filename, flags);
}
