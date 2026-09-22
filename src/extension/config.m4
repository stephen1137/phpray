dnl config.m4 for extension phpray

PHP_ARG_ENABLE([phpray],
  [whether to enable phpray support],
  [AS_HELP_STRING([--enable-phpray],
    [Enable phpray — PHP request tracing for shared hosting])])

if test "$PHP_PHPRAY" != "no"; then
  PHP_NEW_EXTENSION(phpray, phpray.c hooks_mysql.c hooks_curl.c hooks_file.c hooks_profiler.c hooks_redis.c ringbuffer.c crash_recovery.c control.c, $ext_shared)
fi
