# PHPRay Makefile

.PHONY: all extension collector agent api dashboard clean test benchmark

# Directories
EXT_DIR = src/extension
COLLECTOR_DIR = src/collector
AGENT_DIR = src/agent
API_DIR = src/api
DASHBOARD_DIR = src/dashboard

# PHP config
PHP_CONFIG ?= php-config
PHPIZE ?= phpize

all: extension collector agent api dashboard

## PHP Extension
extension:
	cd $(EXT_DIR) && \
	$(PHPIZE) && \
	./configure --with-php-config=$(PHP_CONFIG) && \
	make -j$$(nproc)

extension-install: extension
	cd $(EXT_DIR) && make install

extension-clean:
	cd $(EXT_DIR) && ([ -f Makefile ] && make distclean || true) && \
	rm -rf autom4te.cache configure.ac

## Go Collector
collector:
	cd $(COLLECTOR_DIR) && go build -o ../../bin/phpray-collector .

## eBPF Agent  
agent:
	cd $(AGENT_DIR) && go generate ./... && go build -o ../../bin/phpray-agent .

## API Server
api:
	cd $(API_DIR) && go build -o ../../bin/phpray-api .

## Dashboard
dashboard:
	cd $(DASHBOARD_DIR) && npm install && npm run build

## Tests
test-extension:
	cd $(EXT_DIR) && make test

test-collector:
	cd $(COLLECTOR_DIR) && go test ./...

test-agent:
	cd $(AGENT_DIR) && go test ./...

test-api:
	cd $(API_DIR) && go test ./...

test-mcp:
	cd src/mcp && go test ./...

test: test-extension test-collector test-agent test-api test-mcp

## Benchmark
benchmark:
	./scripts/benchmark.sh

## Clean
clean:
	rm -rf bin/
	cd $(EXT_DIR) && ([ -f Makefile ] && make distclean || true)
	cd $(COLLECTOR_DIR) && go clean
	cd $(AGENT_DIR) && go clean  
	cd $(API_DIR) && go clean
	cd $(DASHBOARD_DIR) && rm -rf node_modules dist
