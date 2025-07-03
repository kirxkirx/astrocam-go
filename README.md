# AstroCam Go - NMW Image Upload System

This program looks when three files with the predefined naming pattern
appear in the input directory, packs the three files in a ZIP or RAR archive
and sends them via HTTP POST to a server for further processing.

This code is used in the [New Milky Way survey](https://scan.sai.msu.ru/nmw/), 
it runs on the image acquisition computer and transfers images to the
analysis computer that runs [unmw wrapper scripts](https://github.com/kirxkirx/unmw/)
around [VaST](https://github.com/kirxkirx/vast/) to run the image analysis
that ultimately results in an HTML page with candidate transients.

It's a complete rewrite of the original Python astrocam code in Go, with improved reliability, built-in archive formats, and automated testing support.

## Key Features

### ✅ **Automatic Archive Format Detection**
- **RAR**: Uses external `rar` command if available (Windows: WinRAR, Linux: `rar` package)
- **ZIP**: Built-in ZIP support as fallback (no external dependencies)
- **Automatic Detection**: Checks for `rar` availability at startup and chooses format accordingly

### ✅ **Dual Operation Modes**
- **Normal Mode**: Continuous monitoring and processing (production use)
- **Test Mode**: Automated testing with timeout and error handling (CI/testing)

### ✅ **Enhanced Authentication**
- **With Credentials**: Uses HTTP Basic Auth when username/password provided
- **Without Credentials**: Uploads without authentication when credentials empty/missing
- **Flexible Configuration**: Handles whitespace-only credentials properly

### ✅ **Robust Error Handling**
- **File Move Retry**: Automatically retries failed file moves (handles file locks)
- **Upload Throttling**: 120-second delays between uploads to prevent server overload
- **Graceful Degradation**: Continues processing even if some files fail to move

### ✅ **Cross-Platform Compatibility**
- **Windows**: 32-bit and 64-bit versions
- **Linux**: Native support
- **Path Handling**: Automatic Windows/Linux path conversion

## Usage

### **Normal Operation (Production)**
```bash
# Linux
./astrocam-go

# Windows
astrocam-go-win64.exe
```

### **Test Mode (CI/Testing)**
```bash
# Linux
./astrocam-go -test

# Windows  
astrocam-go-win64.exe -test
```

### **Test Mode Behavior**
- ✅ **Automatic Exit**: Exits after 2 minutes if no new images appear
- ✅ **Error Handling**: Exits with non-zero status on any failure
- ✅ **Perfect for CI**: Designed for automated testing pipelines
- ✅ **Quick Validation**: Tests archive creation, upload, and file handling

## Configuration

Same `config.env` format as original Python version:

```bash
SAI_SERVER=https://your-server.com/upload.py
SAI_USERNAME=your_username           # Optional: leave empty for no auth
SAI_PASSWORD=your_password           # Optional: leave empty for no auth  
SAI_CAMERA_DIRECTORY=C:\Camera\Images
SAI_PROCESSED_DIRECTORY=C:\Camera\Processed
SAI_INTERVAL=10
SAI_COUNT=3
SAI_PREFIX=
SAI_POSTFIX=_STL-11000M
```

## Building

### **Quick Build and Test**
```bash
./compile_and_test.sh
```

### **Manual Build**
```bash
# Linux version
go build -o astrocam-go astrocam.go

# Windows 64-bit
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o astrocam-go-win64.exe astrocam.go

# Windows 32-bit  
GOOS=windows GOARCH=386 go build -ldflags="-s -w" -o astrocam-go-win32.exe astrocam.go
```

## Archive Formats

### **RAR (Preferred)**
- **Windows**: Install WinRAR (includes command line tools)
- **Linux**: Install rar package: `sudo apt install rar`
- **Benefits**: Better compression, same format as original Python version

### **ZIP (Fallback)**
- **No Dependencies**: Built into Go executable
- **Universal**: Works everywhere without additional software
- **Automatic**: Used when `rar` command not found

## Terminal Output Examples

### **Normal Mode Startup**
```
=== ASTROCAM STARTING IN NORMAL OPERATION MODE ===
Archive format: RAR (rar command available)
========================================
ASTROCAM NORMAL OPERATION - CONTINUOUS MONITORING
========================================
Configuration:
  Scan interval: 10 seconds
  Files per archive: 3
  Camera directory: test_data/1_semka
  Processed directory: test_data/2_otpravleno
  Temp directory: ./temp
  Archive format: RAR
  Authentication: Enabled (username: nmw)
========================================
```

### **Test Mode Startup**
```
=== ASTROCAM STARTING IN TEST MODE ===
Archive format: ZIP (rar command not found, using built-in ZIP)
========================================
ASTROCAM TEST MODE - AUTOMATED TESTING
Test timeout: 2 minutes
========================================
```

### **Processing Output**
```
Scanning camera directory... 2025-06-29 11:14:48
Processing file: test_data/1_semka/064_2025-6-28_21-23-59_001.fts
Processing file: test_data/1_semka/064_2025-6-28_21-24-52_002.fts
Processing file: test_data/1_semka/064_2025-6-28_21-25-46_003.fts
Creating RAR archive: 2025-06-29_064_111448_STL-11000M.rar
Archive created: 2025-06-29_064_111448_STL-11000M.rar
Uploading to server: 2025-06-29_064_111448_STL-11000M.rar
Using authentication for upload
Successfully uploaded: 2025-06-29_064_111448_STL-11000M.rar
```

## Deployment

### **Linux Deployment**
```bash
# Copy files
cp astrocam-go /opt/astrocam/
cp config.env /opt/astrocam/
cp areas.txt /opt/astrocam/

# Install rar (optional, for RAR format)
sudo apt install rar

# Run
cd /opt/astrocam && ./astrocam-go
```

### **Windows Deployment**
```batch
REM Copy files to C:\AstroCam\
copy astrocam-go-win64.exe C:\AstroCam\astrocam.exe
copy config.env C:\AstroCam\
copy areas.txt C:\AstroCam\

REM Install WinRAR (optional, for RAR format)
REM Download from https://www.rarlab.com/

REM Run
cd C:\AstroCam
astrocam.exe
```

## CI Integration

### **GitHub Actions Example**
```yaml
- name: Test AstroCam
  run: |
    go build -o astrocam-go astrocam.go
    ./astrocam-go -test
```

### **Jenkins Example**
```bash
#!/bin/bash
set -e
go build -o astrocam-go astrocam.go
./astrocam-go -test
echo "AstroCam test passed"
```

## Troubleshooting

### **Archive Format Issues**
- **Error**: "rar command not found"
- **Solution**: System will automatically use ZIP format
- **Optional**: Install rar package for RAR format

### **Authentication Issues**
- **Error**: HTTP 401/403 errors
- **Solution**: Check SAI_USERNAME and SAI_PASSWORD in config.env
- **Note**: Leave empty for servers that don't require authentication

### **File Move Errors**
- **Error**: "Cannot move file" messages
- **Behavior**: System retries once, then continues (archive still uploaded)
- **Cause**: Usually file locks from other programs

### **Test Mode Timeout**
- **Behavior**: Exits after 2 minutes if no files to process
- **Normal**: This is expected behavior in test mode
- **Solution**: Add test files to camera directory before running

## Migration from Python Version

1. **Stop** the Python version
2. **Copy** configuration files (`config.env`, `areas.txt`)
3. **Build** Go version
4. **Test** with `-test` flag
5. **Deploy** Go version
6. **Remove** Python installation

The Go version is a **drop-in replacement** with the same functionality but better reliability and performance.
