FROM php:8.3-cli

# Build dependencies + MySQL client for testing
RUN apt-get update && apt-get install -y \
    build-essential \
    autoconf \
    apache2-utils \
    procps \
    jq \
    libcurl4-openssl-dev \
    default-libmysqlclient-dev \
    && rm -rf /var/lib/apt/lists/*

# Install PHP extensions needed for hook testing
RUN docker-php-ext-install mysqli pdo pdo_mysql curl

# Copy extension source
COPY src/extension/ /usr/src/phpray/

# Build the extension
WORKDIR /usr/src/phpray
RUN phpize \
    && ./configure --enable-phpray \
    && make -j$(nproc) \
    && make install \
    && echo "extension=phpray.so" > /usr/local/etc/php/conf.d/99-phpray.ini

# Add PHPRay configuration
RUN printf '\n\
phpray.enabled = 1\n\
phpray.mode = smart\n\
phpray.smart_threshold_normal = 200\n\
phpray.smart_threshold_full = 1000\n\
phpray.smart_threshold_alert = 3000\n\
phpray.always_trace_errors = 1\n\
phpray.output_path = /tmp/phpray.jsonl\n\
phpray.ignore_uris = /favicon.ico,/robots.txt\n\
phpray.trace_cli = 0\n\
phpray.emit_header = 1\n\
phpray.capture_errors = 1\n\
phpray.output_mode = both\n\
phpray.shm_path = /dev/shm/phpray\n\
phpray.shm_size = 33554432\n\
' >> /usr/local/etc/php/conf.d/99-phpray.ini

# Copy test files
COPY docker/test/ /var/www/html/

WORKDIR /var/www/html

# Expose port for PHP built-in server
EXPOSE 8080

# Run PHP built-in web server
CMD ["php", "-S", "0.0.0.0:8080", "-t", "/var/www/html"]
