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
- **Upload Throttling**: uploads start at least 120 seconds apart to prevent server overload. The gap is not slept through: scanning and packing go on every cycle, and the status lines say when the next upload starts
- **Slow Links**: one upload attempt may take up to 15 minutes, and progress is printed while it runs. The log distinguishes a transfer that is moving slowly, one that has stalled, and one where the whole archive has arrived and the server has not yet answered
- **A Timeout Is Not An Unreachable Server**: running out of time is reported as a slow or stalled transfer, never as an unreachable server, and does not fail over to the fallback. Note that each retry restarts from the beginning, so an archive that cannot be delivered within the timeout will never be delivered until the link is faster or the archive is smaller
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
- **Leftovers**: a visit that ends with fewer than `SAI_COUNT` frames is left alone rather than being padded with frames from another visit. The console says so when it happens (`Field X: only 1 of 2 images arrived, and 10 minutes have passed`), and incomplete sets that are already old when AstroCam starts are summed up in one line
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

### ✅ **The Console Says What Is Going On**
- **Status Lines Every Cycle**: every scan cycle ends with two lines that say whether the server is taking uploads and when it last said so, how many archives have been uploaded and when the last one went, what is still queued and when the next upload starts, and when the camera last produced an image. They are plain text that is simply printed again each cycle and fit an 80 column console - see *Terminal Output Examples*
- **Problems Stay On Screen**: a problem that lasts (server out of disk space, busy or unreachable, a refused login, a wrong upload address, no camera directory, an archive the server keeps refusing) is named in those lines on every cycle. A box with the server's own words and what to do about it is printed when the problem starts and every 20 cycles after that, and its end is announced with a `RESOLVED` line
- **The Right Thing Gets The Blame**: HTTP 401/403 (login), 404 (address), a redirect and 5xx errors of a proxy are reported as problems of the server or of `config.env`, and pause the uploads without sending archive after archive into them. An archive is called the culprit only when other archives are accepted while it keeps being refused
- **Redirects Are Never Followed**: an upload answered with a redirect is an error (`SAI_SERVER` has to be the final address, e.g. `https://` if the server redirects `http://`). A redirected upload does not carry the archive, and must never be taken for a delivered one
- **Images That Are Not Uploaded Are Explained**: a set that was not completed in time, images taken too far apart to be one visit, and FITS files whose names match no field in `areas.txt` are each pointed out, with the reason
- **Version On Screen**: the version is printed at startup and at the start of the second status line, so a screenshot of the console shows which release produced it
- **Ctrl+C Works At Once**: a request to stop ends a wait or an upload in progress immediately. Archives not yet uploaded stay in `temp/` and are sent on the next start

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

Stop it with Ctrl+C. Archives that have not been uploaded yet stay in `temp/` and are sent the next time it runs.

### **Which Version Is This?**
```bash
astrocam-go-win64.exe -version
```

A running AstroCam also shows its version, at startup (`Version: ...`) and at the start of the second status line of every cycle. A binary built by hand rather than taken from a release shows `dev`, followed by the source revision when it was built in a git checkout (`dev-0236ae8`, with a `+` if the files differed from that revision).

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

### **Startup**
```
=== ASTROCAM STARTING IN NORMAL OPERATION MODE ===
Version: v0.6.0
Archive mode: auto
Archive format: RAR (using C:\Program Files\WinRAR\rar.exe)
========================================
ASTROCAM NORMAL OPERATION - CONTINUOUS MONITORING
========================================
Configuration:
  Scan interval: 15 seconds (minimum: 15)
  Upload server: https://your-server.com/cgi-bin/upload.py
  Files per archive: 2
  Camera directory: C:\NINA\2026-11-22\LIGHT
  Processed directory: C:\AstroCam\processed
  Temp directory: C:\AstroCam\temp
  Archive mode: auto
  Archive format: RAR (using C:\Program Files\WinRAR\rar.exe)
  FITS file extensions: .fts, .fits, .fit
  Authentication: Enabled (username: nmw)
========================================
Watching 5 folders for images:
  C:\NINA\2026-11-22\LIGHT
  C:\NINA\2026-11-21\LIGHT
  C:\NINA\2026-11-20\LIGHT
  ... and 2 older ones beside them
```

In test mode (`-test`) the heading reads `ASTROCAM TEST MODE - AUTOMATED TESTING` and is followed by `Test timeout: 2 minutes`.

### **The Status Lines**

Every scan cycle ends with these two lines, whatever else it printed:

```
05:23:13 Server OK at 05:22:58. 38 archives uploaded, last at 05:22:58.
  v0.6.0 Queue empty. Last image at 05:23:02. 1 field waits for its 2nd image.
```

| It says | It means |
|---------|----------|
| `05:23:13` | when this cycle ended |
| `Server OK at 05:22:58` | the last time the server said it is taking uploads. The server is contacted only when there is an archive to upload, so on a quiet night this time can be hours old; until the first archive it reads `Server not contacted yet` |
| `38 archives uploaded, last at 05:22:58` | archives the server has confirmed since AstroCam was started |
| `v0.6.0` | the version of AstroCam that is running |
| `Queue empty` | nothing is waiting to be uploaded. Otherwise `3 archives queued, next upload at 05:24:58` |
| `Last image at 05:23:02` | the newest image of a field from `areas.txt` that AstroCam has seen. If this time stands still while the telescope is working, the camera software is not writing where (or what) AstroCam expects |
| `1 field waits for its 2nd image` | a field has some of its `SAI_COUNT` images, the newest less than 10 minutes old |

A moment more than 12 hours ago is shown with its date (`last at Nov 22 05:14`).

### **Packing And Uploading**
```
Processing file: C:\NINA\2026-11-22\LIGHT\Gem-02-Q2b1x1_2026-11-23_05-21-10_20.00sec_-0.00C_LIGHT_0152.fits
Processing file: C:\NINA\2026-11-22\LIGHT\Gem-02-Q2b1x1_2026-11-23_05-21-51_20.00sec_-0.00C_LIGHT_0153.fits
Creating RAR archive: 2026-11-23_Gem-02-Q2b1x1_052206_TTUQ2b1x1.rar (area Gem-02-Q2b1x1, 2 frames from C:\NINA\2026-11-22\LIGHT)
Archive created: 2026-11-23_Gem-02-Q2b1x1_052206_TTUQ2b1x1.rar
Uploading 2026-11-23_Gem-02-Q2b1x1_052206_TTUQ2b1x1.rar to server: https://your-server.com/cgi-bin/upload.py
Using authentication for upload
Sending 33.4 MB to https://your-server.com/cgi-bin/upload.py (giving up after 15m0s)
  upload: 14.1 MB of 33.4 MB (42%) at 705.0 kB/s, about 27s left
  upload: 33.4 MB delivered and answered in 52s
Successfully uploaded: 2026-11-23_Gem-02-Q2b1x1_052206_TTUQ2b1x1.rar
05:22:58 Server OK at 05:22:58. 38 archives uploaded, last at 05:22:58.
  v0.6.0 Queue empty. Last image at 05:21:51.
```

### **When Something Is Wrong**

The first status line names a problem with the server for as long as it lasts, and a box with the details is printed when it starts and again every 20 cycles:

```
Server is out of disk space. Pausing uploads for 1 hour, will retry after 01:51:12.

!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!!! ERROR: Server is out of disk space
!!! Started 2026-11-23 00:51:12 (0s ago).
!!!   Server response: UNMW_STATUS:ERROR ERROR: server myserver is out
!!!   of disk space, only 4752 MB free at uploads
!!! What astrocam-go is doing: waiting 1 hour before trying again, and
!!! packing new images in the meantime. Nothing is lost: images keep
!!! being packed here and every archive is retried until the server
!!! confirms it. To fix it, free disk space on the upload server -
!!! astrocam-go cannot do that from here. Once there is room it
!!! carries on by itself at the next retry; restarting it retries at
!!! once.
!!! Next retry at 01:51:12 (in 59m 59s).
!!! 1 archive waiting to be delivered (33.4 MB), oldest is
!!! 2026-11-23_Leo-01-Q2b1x1_005110_TTUQ2b1x1.rar
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!

00:51:12 !!! SERVER IS OUT OF DISK SPACE for 0s. Next try at 01:51:12.
         12 archives uploaded, last at 00:49:03.
  v0.6.0 1 archive queued. Last image at 00:50:41.
```

and, hours later, still:

```
05:23:19 !!! SERVER IS OUT OF DISK SPACE for 4h 32m. Next try at 05:51:40.
         12 archives uploaded, last at 00:49:03.
  v0.6.0 50 archives queued. Last image at 05:22:41.
```

When it is over:

```
=== RESOLVED: Server is out of disk space - the upload server answers
=== its status check normally again (the problem lasted 5h 00m).
```

The same goes for `SERVER IS BUSY: SYSTEM LOAD TOO HIGH`, `UPLOAD SERVER IS UNREACHABLE`, `SERVER REFUSES THE LOGIN (HTTP 401)`, `UPLOAD ADDRESS NOT FOUND ON THE SERVER (HTTP 404)`, `SERVER REDIRECTS UPLOADS ELSEWHERE (HTTP 301)` and `SERVER-SIDE ERROR (HTTP 502)`. A problem that is not about the server gets a line of its own under the status lines, for example `!!! ERROR 5m 00s: No camera image directory can be found` or `!!! WARNING 12m 30s: The server keeps refusing one archive`.

### **Images That Are Not Uploaded**

An image that is not being uploaded always has a reason, and the console gives it:

```
Field Sex-01-Q2b1x1: only 1 of 2 images arrived, and 10 minutes have passed.
  The set is NOT complete: nothing is uploaded, the image stays where it is.
```

```
Field Hya-01-Q2b1x1: its 2 images were taken 47 minutes apart.
  More than 10 minutes means two separate visits: they are NOT combined.
```

```
Left in the camera folders: 3 older images of 2 fields that never made a
  complete set (2 images within 10 minutes). They are not uploaded.
```

```
NOTE: 4 FITS files written in the last hour match no field in areas.txt
  and are ignored. The newest one:
    2026-11-23_05-13-00_LIGHT_20.00s_0003.fits
  A file is taken only when its name starts with a field name from areas.txt
  followed by "_" (in NINA the file pattern must start with $$TARGETNAME$$).
```

While there are such files, a third status line counts them: `NOTE: 4 recent FITS files match no field in areas.txt (ignored).` It goes away by itself an hour after the last such file was written.

### **Stopping**

Ctrl+C stops AstroCam at once, also in the middle of an upload or of the wait between two uploads:

```
Stop requested (interrupt). Stopping...
astrocam-go stopped. 2 archives (66.8 MB) not yet uploaded: kept in
  C:\AstroCam\temp
  and sent the next time astrocam-go runs.
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
- **Error**: the status lines show `!!! SERVER REFUSES THE LOGIN (HTTP 401)` or `!!! SERVER REFUSES ACCESS (HTTP 403)`
- **Solution**: Check SAI_USERNAME and SAI_PASSWORD in config.env, then restart AstroCam
- **Note**: Leave empty for servers that don't require authentication
- **Behavior**: Nothing is lost and nothing is sent while the login is refused; the server is asked again every 10 minutes

### **Wrong Upload Address**
- **Error**: `!!! UPLOAD ADDRESS NOT FOUND ON THE SERVER (HTTP 404)` or `!!! SERVER REDIRECTS UPLOADS ELSEWHERE (HTTP 301)`
- **Solution**: Correct `SAI_SERVER` in config.env (it is printed at startup as `Upload server:`), then restart AstroCam. For a redirect, the box on the console shows the address the server redirects to; that address is what `SAI_SERVER` has to be

### **Images Are Taken But Nothing Is Uploaded**
- **Check `Last image at`** in the status lines. `No images seen yet`, or a time that stands still, means AstroCam does not see the images: compare `Camera directory:` and the `Watching ... folders for images` list printed at startup with where the camera software writes
- **`NOTE: N recent FITS files match no field in areas.txt`**: the files are there but their names do not start with a field name from `areas.txt` followed by `_`. The console shows one of the names; see *Required NINA file name pattern*
- **`Field X: only 1 of 2 images arrived`**: the visit ended (or was interrupted) before all `SAI_COUNT` images were taken. Images taken more than 10 minutes apart are different visits and are never combined

### **Upload Fail-Over**
- **Behavior**: When the primary server is unreachable, uploads go to `SAI_FALLBACK_SERVER` (if set); the log shows `Primary server unreachable - trying fallback server: ...`
- **Note**: A server that reports "out of disk space" or "high system load" is reachable, so it pauses/retries the primary instead of failing over
- **All Unreachable**: If no server can be reached, the archive is kept locally and retried on the next cycle. The status lines show `!!! UPLOAD SERVER IS UNREACHABLE`; the network error itself is printed when it first occurs and in the box that is repeated every 20 cycles, not on every cycle

### **NINA Folder Not Detected**
- **Check**: `SAI_CAMERA_DIRECTORY` must end with `.../<YYYY-MM-DD>/LIGHT` (the final `LIGHT` and a valid `YYYY-MM-DD` parent are both required)
- **Behavior**: The guessed folder uses `now − 12 hours`; before local noon this is the previous calendar date, matching NINA's own convention
- **Log**: The folders that are scanned are listed at startup, and again whenever the set changes, under `Watching N folders for images:` (the first three are named, older ones are counted)

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
