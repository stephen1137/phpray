#!/bin/bash
# PHPRay Valgrind Memory Leak Check
#
# Runs PHP under Valgrind with the PHPRay extension loaded.
# Reports any memory leaks originating from PHPRay code.
#
# Exit code: 0 = no PHPRay leaks, 1 = leaks detected

set -e

SUPP_FILE="/usr/src/phpray/php.supp"
TEST_FILE="/var/www/html/valgrind_test.php"
LOG_FILE="/tmp/valgrind_phpray.log"
ITERATIONS="${1:-50}"

echo "╔══════════════════════════════════════════╗"
echo "║   PHPRay Valgrind Memory Leak Check      ║"
echo "╚══════════════════════════════════════════╝"
echo ""

# Verify extension is loaded
php -r "echo extension_loaded('phpray') ? 'Extension: OK' : 'Extension: MISSING'; echo PHP_EOL;" || true
echo ""

# Run under Valgrind
echo "Running Valgrind (${ITERATIONS} iterations)..."
echo "This will take a while..."
echo ""

valgrind \
    --tool=memcheck \
    --leak-check=full \
    --show-leak-kinds=definite,indirect,possible \
    --track-origins=yes \
    --suppressions="${SUPP_FILE}" \
    --log-file="${LOG_FILE}" \
    --num-callers=20 \
    --error-exitcode=99 \
    php "${TEST_FILE}" "${ITERATIONS}" 2>&1

VALGRIND_EXIT=$?

echo ""
echo "════════════════════════════════════════════"
echo "VALGRIND RESULTS"
echo "════════════════════════════════════════════"
echo ""

# Show full Valgrind log
cat "${LOG_FILE}"

echo ""
echo "════════════════════════════════════════════"

# Check for PHPRay-specific leaks
PHPRAY_LEAKS=$(grep -c -E "phpray|ringbuffer|hooks_" "${LOG_FILE}" 2>/dev/null || true)
PHPRAY_LEAKS="${PHPRAY_LEAKS:-0}"
# Ensure it's a clean integer
PHPRAY_LEAKS=$(echo "${PHPRAY_LEAKS}" | head -1 | tr -dc '0-9')
PHPRAY_LEAKS="${PHPRAY_LEAKS:-0}"

echo ""
echo "=== SUMMARY ==="
echo "PHPRay-related references in Valgrind output: ${PHPRAY_LEAKS}"

# Parse the summary line from Valgrind
DEFINITELY_LOST=$(grep "definitely lost:" "${LOG_FILE}" | head -1 | grep -oP '[\d,]+' | head -1 | tr -d ',')
INDIRECTLY_LOST=$(grep "indirectly lost:" "${LOG_FILE}" | head -1 | grep -oP '[\d,]+' | head -1 | tr -d ',')
POSSIBLY_LOST=$(grep "possibly lost:" "${LOG_FILE}" | head -1 | grep -oP '[\d,]+' | head -1 | tr -d ',')

echo "Definitely lost: ${DEFINITELY_LOST:-0} bytes"
echo "Indirectly lost: ${INDIRECTLY_LOST:-0} bytes"
echo "Possibly lost: ${POSSIBLY_LOST:-0} bytes"

if [ "${PHPRAY_LEAKS}" -gt 0 ]; then
    echo ""
    echo "⚠️  PHPRay-related entries found in Valgrind output!"
    echo "Review the log above for details."
    echo ""
    # Extract PHPRay-specific blocks
    grep -B5 -A5 "phpray\|ringbuffer\|hooks_" "${LOG_FILE}" || true
fi

# Determine result
if [ "${DEFINITELY_LOST:-0}" -eq 0 ] && [ "${PHPRAY_LEAKS}" -eq 0 ]; then
    echo ""
    echo "✅ NO MEMORY LEAKS DETECTED IN PHPRAY"
    exit 0
elif [ "${PHPRAY_LEAKS}" -eq 0 ]; then
    echo ""
    echo "✅ No PHPRay-specific leaks (some PHP engine leaks suppressed)"
    exit 0
else
    echo ""
    echo "❌ POTENTIAL MEMORY LEAKS IN PHPRAY — review Valgrind output"
    exit 1
fi
