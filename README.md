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
- **File Move Retry**: Automatically retries failed file moves (handles file locks). A frame is never deleted just because a file of the same name is already in the processed directory
- **Atomic Archives**: archives are built in `temp/incoming` and moved into `temp/` only after passing an integrity test, so a crash or a full disk cannot leave a half-written archive to be uploaded
- **Upload Throttling**: 120-second delays between uploads to prevent server overload
- **Graceful Degradation**: Continues processing even if some files fail to move

### ✅ **Fallback Upload Server**
- **Automatic Fail-Over**: If the primary server (`SAI_SERVER`) is unreachable, the archive is uploaded to an optional fallback server (`SAI_FALLBACK_SERVER`) instead
- **Only on Unreachability**: Fail-over happens only for network-level failures (DNS failure, timeout, connection refused). A server that *is* reachable but reports "out of disk space" or "high system load" is **not** failed over — uploads pause and later retry the primary, exactly as before
- **Shared Credentials**: The fallback server uses the same `SAI_USERNAME` / `SAI_PASSWORD`
- **Optional**: Leave `SAI_FALLBACK_SERVER` empty (or omit it) to disable

### ✅ **NINA Date-Folder Auto-Detection**
- **Rolling Date Folders**: [NINA](https://nighttime-imaging.eu/) stores images in `.../<YYYY-MM-DD>/LIGHT` folders named after the current observing night. With `$$DATEMINUS12$$` that is the date at local noon, so a whole night shares one folder
- **Every Night Is Scanned, Not Just Tonight**: when `SAI_CAMERA_DIRECTORY` sits inside a dated folder, *every* dated folder beside it is scanned, newest first, plus the folder for the current night even before it exists. There is deliberately no age limit: a night's folder leaves the scan set when it runs out of images to pack, never because the clock moved on. Images left over from an earlier night are therefore still picked up, however late
- **Configured Directory Always Scanned**: the directory set in `SAI_CAMERA_DIRECTORY` is always scanned too, so non-NINA setups and explicitly chosen paths keep working
- **Case-Insensitive Leaf**: the `LIGHT` part is matched against the folder that is really on disk, so a lower-case spelling in the config still works and a folder is never scanned twice under two spellings
- **Start-Early Safe**: AstroCam can be started before NINA creates the folder — a not-yet-created folder is simply skipped and re-checked next cycle, then picked up as soon as it appears

### ✅ **One Visit Per Archive**
- **Visits, Not Just Counts**: the frames of one field are grouped into visits by the time in their file names; an archive holds `SAI_COUNT` frames from a *single* visit and never mixes two visits of the same field
- **Boundary-Safe**: a visit whose frames straddle midnight (or any other folder boundary) is still one visit, so it is still packed
- **Leftovers**: a visit that ends with fewer than `SAI_COUNT` frames is left alone rather than being padded with frames from another visit
- **Finished Files Only**: a frame is packed only once it is a complete FITS file and has stopped changing since the previous scan, so a frame still being written is never archived. Completeness is judged from the file's own FITS header, which states how many records must follow it, so a frame cut short by a crash or a full disk is caught even when its length happens to land on a record boundary

### ✅ **Not Tied To One Camera**
- **Standard FITS Only**: frames are validated with the FITS standard itself, the `SIMPLE` keyword plus the 2880 byte record structure and the length implied by `BITPIX` and `NAXIS`. No vendor keyword is required
- **Frame Times From `DATE-OBS`**: visits are grouped using the standard `DATE-OBS` keyword when the camera writes one. If it is missing, a timestamp in the file name is used, and failing that the file modification time. One source is chosen per field and never mixed, because `DATE-OBS` is UTC while the other two are usually local
- **Any Extension Case**: `.fits`, `.FITS`, `.fts` and `.fit` are all recognised. The field name in front of the extension stays case-sensitive
- **Any Folder Layout**: the dated-folder scanning keys on a `YYYY-MM-DD` component anywhere in the configured path, not on one program's template

### ✅ **Packing Never Waits For The Server**
- **Pack First, Upload Second**: each cycle packs everything that is ready and only then delivers. An archive in the temp directory is safe, so getting images into it is never delayed by the server being slow, full, or down
- **Bounded Delivery**: the upload part of a cycle has a time budget, so a long backlog cannot stop new images from being packed. Whatever is left is retried next cycle
- **Never Given Up On**: an archive is deleted only after the server confirms it. Both `.zip` and `.rar` archives are retried whatever format the current run is using, so a backlog packed in the other format is still delivered

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
SAI_FALLBACK_SERVER=                 # Optional: used only if SAI_SERVER is unreachable
SAI_USERNAME=your_username           # Optional: leave empty for no auth
SAI_PASSWORD=your_password           # Optional: leave empty for no auth  
SAI_CAMERA_DIRECTORY=C:\Camera\Images
SAI_PROCESSED_DIRECTORY=C:\Camera\Processed
SAI_INTERVAL=10
SAI_COUNT=3
SAI_PREFIX=
SAI_POSTFIX=_STL-11000M
```

- **`SAI_FALLBACK_SERVER`** (optional): a second upload URL used only when `SAI_SERVER` is unreachable at the network level. It reuses `SAI_USERNAME` / `SAI_PASSWORD`. See *Fallback Upload Server* above.
- **`SAI_CAMERA_DIRECTORY`**: if this sits inside a `.../<YYYY-MM-DD>/...` folder, AstroCam also scans every other dated folder beside it, newest first, plus the folder for the current night. No extra option is needed — see *NINA Date-Folder Auto-Detection* above.
- **`SAI_COUNT`**: the number of frames in one visit to a field. Only a single visit with at least this many frames is packed; the server requires at least two frames per archive.

### **Required NINA file name pattern**

AstroCam matches images by field name, so the NINA file name pattern must **start with `$$TARGETNAME$$`** followed by an underscore, and the NINA target names must match the entries in `areas.txt`. A pattern such as

```
$$DATEMINUS12$$\$$IMAGETYPE$$\$$TARGETNAME$$_$$DATETIME$$_$$EXPOSURETIME$$sec_$$SENSORTEMP$$C_$$IMAGETYPE$$_$$FRAMENR$$
```

produces `Cep-01-Q1b1x1_2026-09-03_23-06-45_20.00sec_-0.00C_LIGHT_0152.fits`, which is what AstroCam expects. NINA's stock pattern does **not** include `$$TARGETNAME$$`, and with it nothing is ever uploaded.

## Building

### **Quick Build and Test**
```bash
./compile_and_test.sh
```

### **Manual Build**
```bash
# Linux version
go build -o astrocam-go

# Windows 64-bit
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o astrocam-go-win64.exe

# Windows 32-bit  
GOOS=windows GOARCH=386 go build -ldflags="-s -w" -o astrocam-go-win32.exe
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
    go build -o astrocam-go
    ./astrocam-go -test
```

### **Jenkins Example**
```bash
#!/bin/bash
set -e
go build -o astrocam-go
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

### **Upload Fail-Over**
- **Behavior**: When the primary server is unreachable, uploads go to `SAI_FALLBACK_SERVER` (if set); the log shows `Primary server unreachable - trying fallback server: ...`
- **Note**: A server that reports "out of disk space" or "high system load" is reachable, so it pauses/retries the primary instead of failing over
- **All Unreachable**: If no server can be reached, the archive is kept locally and retried on the next cycle

### **NINA Folder Not Detected**
- **Check**: `SAI_CAMERA_DIRECTORY` must end with `.../<YYYY-MM-DD>/LIGHT` (the final `LIGHT` and a valid `YYYY-MM-DD` parent are both required)
- **Behavior**: The guessed folder uses `now − 12 hours`; before local noon this is the previous calendar date, matching NINA's own convention
- **Log**: When both the configured and guessed folders exist, the log shows `Scanning N image directories: [...]`

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
