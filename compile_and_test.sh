#!/usr/bin/env bash

set -e  # Exit on any error

echo "========================================="
echo "ASTROCAM BUILD AND TEST SCRIPT"
echo "========================================="

# Build executables
echo "Building astrocam-go for Linux..."
go build -o astrocam-go astrocam.go
if [ $? -ne 0 ]; then
    echo "ERROR: Linux build failed"
    exit 1
fi

echo "Building astrocam-go for Windows (64-bit)..."
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o astrocam-go-win64.exe astrocam.go
if [ $? -ne 0 ]; then
    echo "ERROR: Windows 64-bit build failed"
    exit 1
fi

echo "Building astrocam-go for Windows (32-bit)..."
GOOS=windows GOARCH=386 go build -ldflags="-s -w" -o astrocam-go-win32.exe astrocam.go
if [ $? -ne 0 ]; then
    echo "ERROR: Windows 32-bit build failed"
    exit 1
fi

echo "========================================="
echo "BUILD SUCCESSFUL"
echo "Files created:"
ls -lh astrocam-go astrocam-go-win*.exe
echo "========================================="

# Prepare test environment
echo "Preparing test environment..."

# Clean directories
echo "Cleaning test directories..."
rm -f test_data/1_semka/* test_data/2_otpravleno/* temp/* 2>/dev/null || true

# Create directories if they don't exist
mkdir -p test_data/1_semka test_data/2_otpravleno temp

# Copy test data if available
if [ -d "test_data/input_data_for_test" ]; then
    echo "Copying test data..."
    cp -v test_data/input_data_for_test/*.fts test_data/1_semka/ 2>/dev/null || true
else
    echo "Creating mock test data..."
    # Create some test files for different areas
    for area in 064 091 092; do
        for i in {1..3}; do
            timestamp=$(date +"%Y-%m-%d_%H-%M-%S" -d "+$i seconds")
            filename="${area}_${timestamp}_STL-11000M.fts"
            echo "Mock FITS data for testing area $area file $i" > "test_data/1_semka/$filename"
        done
    done
fi

echo "Test files created:"
ls -la test_data/1_semka/

echo "========================================="
echo "RUNNING AUTOMATED TEST"
echo "========================================="

# Run in test mode (will exit automatically)
echo "Starting astrocam-go in test mode..."
echo "This will:"
echo "  - Process available files"
echo "  - Create archives (RAR or ZIP depending on availability)"
echo "  - Attempt uploads"
echo "  - Exit automatically after 2 minutes if no new files"
echo "  - Exit with error code if any step fails"
echo ""

# Run the test
if ./astrocam-go -test; then
    echo ""
    echo "========================================="
    echo "TEST PASSED"
    echo "========================================="
    echo "Check results:"
    echo ""
    echo "Files moved to processed directory:"
    ls -la test_data/2_otpravleno/ 2>/dev/null || echo "  (none)"
    echo ""
    echo "Archives in temp directory:"
    ls -la temp/ 2>/dev/null || echo "  (none)"
    echo ""
    echo "Remaining files in camera directory:"
    ls -la test_data/1_semka/ 2>/dev/null || echo "  (none)"
    echo ""
    echo "========================================="
    echo "READY FOR DEPLOYMENT"
    echo "========================================="
    echo "Usage:"
    echo "  Normal operation: ./astrocam-go"
    echo "  Test mode:        ./astrocam-go -test"
    echo ""
    echo "Windows deployment files:"
    echo "  astrocam-go-win64.exe (for modern 64-bit Windows)"
    echo "  astrocam-go-win32.exe (for legacy 32-bit Windows)"
    echo ""
else
    echo ""
    echo "========================================="
    echo "TEST FAILED"
    echo "========================================="
    echo "Check the error messages above for details."
    echo "Common issues:"
    echo "  - Network connectivity problems"
    echo "  - Server authentication issues"
    echo "  - Missing rar command (will fallback to ZIP)"
    echo "  - File permission problems"
    exit 1
fi
