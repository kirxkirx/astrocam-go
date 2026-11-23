# astrocam-go

astrocam-go sends a telescope's images to the transient search of the
[New Milky Way (NMW) survey](https://scan.sai.msu.ru/nmw/) while the night is
still going on.

It runs on the computer that controls the camera, next to the imaging software
(usually [NINA](https://nighttime-imaging.eu/)). Each time the telescope has
finished a visit to a sky field, astrocam-go packs the images of that visit into
one archive and uploads it to the survey's analysis server. There the
[unmw](https://github.com/kirxkirx/unmw/) scripts run
[VaST](https://github.com/kirxkirx/vast/), which compares the new images with
reference images of the same field and puts the new objects it finds - a nova,
a flaring star, a comet - on a web page for a person to check. A visit usually
shows up there a few minutes after its last image was taken; when the server is
busy it can take an hour or more.

At the telescope? [The console](#the-console) explains what the window says.

## How the pieces fit together

```
 camera computer                              analysis server (unmw + VaST)
 +-------------------------------------+      +----------------------------------+
 | NINA saves each image as a FITS     |      | upload.py checks the archive     |
 | file named after the field:         |      | and starts the processing        |
 |   Cep-01-Q1b1x1_..._0152.fits       |      |                |                 |
 |                |                    |      |                v                 |
 |                v                    | HTTP | VaST compares the new images     |
 | astrocam-go packs the images of one |----->| with the field's reference       |
 | visit into an archive and uploads   |      | images                           |
 | it:                                 |      |                |                 |
 |   2026-11-23_CI_Cep-01-Q1b1x1_      |      |                v                 |
 |   052206_TTUQ1b1x1.zip              |      | web page of transient candidates |
 +-------------------------------------+      +----------------------------------+
```

The server learns two things from an upload, and both have to be agreed with
whoever runs it:

- **which field** the images show. Every image file name starts with the field
  name (`Cep-01-Q1b1x1`), and the server compares the new images with its
  reference images stored under that same name;
- **which camera** took them. astrocam-go puts the camera tag from `config.env`
  (`SAI_POSTFIX=_TTUQ1b1x1`) at the end of every archive name, and the server
  uses it to pick the camera's darks, flats and settings and to put the results
  on that camera's list of candidates. With a tag the server does not know, the
  images are processed with the wrong settings and appear on no list.

## Why it works the way it does

- **Fast.** A transient is worth most in its first hours, so a visit is packed
  within a minute of its last image and sent within a few minutes, not at the
  end of the night.
- **One visit per archive.** The search believes a new object only when it is
  seen on two new images of the field, and it takes both from the same upload.
  So the images of a visit travel together, and two visits are never put into
  one archive. A visit ends when more than 10 minutes pass between one image of
  the field and the next.
- **Nothing is lost.** Original images are moved aside, never deleted, and an
  archive is deleted only after the server has confirmed that it has it. While
  the server is down, full or busy, astrocam-go keeps packing new images, keeps
  the archives, and delivers them once the server takes them again - an hour
  later or a week later.
- **Unattended.** It is one program file that needs nothing else installed. It
  runs for months, and its console says in plain words what it is doing and
  whether anything needs a person.

## What it does, step by step

Every 15 seconds (`SAI_INTERVAL`) astrocam-go goes through one cycle:

1. **It looks for new images** in the camera folder (`SAI_CAMERA_DIRECTORY`).
   With NINA's folder per night (`...\2026-11-22\LIGHT`) it looks into that
   folder of every night, so a new night's folder is found without editing
   anything, and a complete visit left over from an earlier night is still
   sent.
2. **It takes the images it is meant to send**: FITS files (`.fits`, `.fit`,
   `.fts`) whose names start with a field name from `areas.txt` followed by
   `_`, once they have been written completely. Everything else in the folder
   is left alone.
3. **It waits for a complete visit**: `SAI_COUNT` images of one field, each
   taken no more than 10 minutes after the one before.
4. **It packs** exactly those images into one archive in the `temp` folder next
   to the program, checks the archive, and **moves** the original images into
   the processed folder (`SAI_PROCESSED_DIRECTORY`).
5. **It uploads** the waiting archives, oldest first and at most one every 2
   minutes, which is 30 an hour. Before uploading it asks the server whether it
   is taking uploads. An archive is deleted only once the server has confirmed
   it.
6. **It prints the status lines**, two when all is well, that say how things
   stand (see [The console](#the-console)).

When something goes wrong - the server cannot be reached, is out of disk space
or busy, or refuses the login - packing goes on, the archives wait in `temp`,
and the problem stays on the screen until it is over. Afterwards the backlog
goes out oldest first, 30 archives an hour at most. A telescope that finishes
nearly as many visits an hour has little to spare, so new visits can reach the
server late for hours after a long outage.

## Setting up a camera computer

### What you need from the server side

Ask whoever runs the analysis server for:

- the **upload address** (`SAI_SERVER`) and, if the server asks for one, a
  **user name and password**;
- a **camera tag** for your camera, such as `TTUQ1b1x1`;
- the **field names** for your camera. The server can only search a field it
  has reference images of, stored under exactly the name your images carry;
- the address of the **web page** with your results, so that you can look at
  the first uploads yourself.

The server also needs things that astrocam-go never sends, so agree with the
administrator how they get there:

- **darks and flats** of your camera. The server uses only a dark taken with
  the same exposure time, image size (binning) and cooling temperature as the
  image; without one it searches the images uncorrected. Tell the
  administrator before you change any of these in NINA;
- **reference images** of every field.

### Installing

1. Download the program from the
   [Releases page](https://github.com/kirxkirx/astrocam-go/releases):
   `astrocam-go-win64.exe` for 64-bit Windows or `astrocam-go-win32.exe` for
   32-bit Windows and Windows on ARM (Windows 10 or newer), `astrocam-go` for
   Linux on a 64-bit PC (make it executable with `chmod +x astrocam-go`). The `-complete` packages
   also hold `config.env.example` and `areas.txt`.
2. Make a folder for it that the user running it can write to, for example
   `C:\AstroCam` (not under `C:\Program Files`), and put the program there
   together with
   - `config.env`: copy `config.env.example` and fill it in (see
     [config.env](#configenv));
   - `areas.txt`: your field names, one per line (see [areas.txt](#areastxt)).

   astrocam-go looks for both files next to itself first, then in the folder it
   is started from. It keeps its archives in a `temp` folder that it creates
   next to itself. The install scripts in the `-complete` packages do steps 1
   and 2: they put the program into `C:\AstroCam\astrocam.exe` (the 32-bit or
   the 64-bit one, whichever fits) or `/opt/astrocam/astrocam`, and the example
   `config.env` and `areas.txt` next to it unless these are there already.
3. Set up the imaging software to name and save the images the way astrocam-go
   expects (see [NINA](#nina)).
4. astrocam-go uploads every complete visit it finds in the night folders
   beside `SAI_CAMERA_DIRECTORY`, however old, oldest first. Before the first
   start, move the folders of earlier nights out of the image folder unless
   they are meant for the server: otherwise they go out first, and the new
   images wait behind them.
5. Start the program and leave its window open (see
   [Starting and stopping](#starting-and-stopping)).

### config.env

```
SAI_SERVER=https://your-server.com/cgi-bin/unmw/upload.py
SAI_USERNAME=
SAI_PASSWORD=
SAI_FALLBACK_SERVER=
SAI_CAMERA_DIRECTORY=C:\Users\observer\Documents\N.I.N.A\2026-11-22\LIGHT
SAI_PROCESSED_DIRECTORY=C:\AstroCam\processed
SAI_COUNT=2
SAI_PREFIX=CI_
SAI_POSTFIX=_TTUQ1b1x1
SAI_INTERVAL=15
```

| Setting | What it is |
|---------|------------|
| `SAI_SERVER` | The upload address. It has to be the final address: an upload cannot follow a redirect, so if the server redirects `http://` to `https://`, give the `https://` one. |
| `SAI_USERNAME`, `SAI_PASSWORD` | The login for the upload server. Set both, or leave both empty if the server needs no login: with only one of them set, no login is sent. |
| `SAI_FALLBACK_SERVER` | Optional. A second upload address, used only while `SAI_SERVER` does not answer at all (its network is down, or it is switched off). It does not help when this computer itself is offline. A server that answers, even with an error (busy, full, login refused), is waited for instead. The fallback uses the same login, and it has to be set up for your camera and fields like the main server; its results appear on its own web pages. |
| `SAI_CAMERA_DIRECTORY` | The folder the imaging software saves the images in, as a full path. Only files directly in it are read, not subfolders. If the path has a date folder (`2026-11-22`) with a folder below it (`LIGHT`), the same folder of every other date beside it is watched too, including dates still to come: give tonight's folder once and never edit it again. If the images are saved directly into the date folder, only that one night is ever watched. |
| `SAI_PROCESSED_DIRECTORY` | Where the original images go once they are packed, as a full path. They are moved there. To a folder on another drive they are copied, and each original is deleted from the camera folder only once its copy is complete; that takes a little longer. It must not be the camera folder itself. If left empty, a `processed` folder next to the program is used. It grows by every night's images (see [Disk space](#disk-space)). |
| `SAI_COUNT` | How many images the telescope takes of a field in one visit. Every archive holds exactly this many images of one visit. At least 2; 3 if not set. |
| `SAI_POSTFIX` | Your camera tag, with a `_` in front of it (`_TTUQ1b1x1`). It goes at the end of every archive name. Use only the tag you were given: the tag of another camera puts your images under that camera. |
| `SAI_PREFIX` | Text in front of the field name in the archive name. Use `CI_` unless whoever runs the server says otherwise: the source monitoring of the unmw server looks for `<date>_CI_<field>_` in archive names, and the NMW-TexasTech cameras all use it. |
| `SAI_INTERVAL` | Seconds between two cycles (two looks at the camera folder). At least 15, which is also the default. |

A `#` starts a comment, also after a value, so no value - not even a password -
can contain a `#`. Do not put quotes around values: they would become part of
the value. Save `config.env` and `areas.txt` as plain text (ANSI, or UTF-8
without BOM); a file saved as "Unicode" is not read correctly. astrocam-go
reads both files only when it starts: restart it after changing them.

### areas.txt

One field name per line, written exactly as it begins the image file names:
the line `Cep-01-Q1b1x1` takes `Cep-01-Q1b1x1_2026-11-23_05-21-10_..._0152.fits`.
Upper and lower case matter. Empty lines are skipped; there are no comments.

A field name may hold only letters, digits, `-` and `.`, and has to be at
least 3 characters long. astrocam-go uploads any name, but the server reads the
field name of an image up to the first `_`, and it refuses an archive whose
image names hold a space or another character.

The `areas.txt` in this repository is only an example: replace it with the
field names of your camera.

### NINA

- Save the images as FITS (NINA writes `.fits`; the server does not find
  upper-case extensions such as `.FITS`).
- The file name itself, the part of NINA's file pattern after the last `\`,
  has to start with `$$TARGETNAME$$_`, and the target names in the sequence
  have to be the field names from `areas.txt`. NINA's default pattern names
  the files after the date and time, and with it nothing is ever uploaded. A
  pattern that works:

  ```
  $$DATEMINUS12$$\$$IMAGETYPE$$\$$TARGETNAME$$_$$DATETIME$$_$$EXPOSURETIME$$sec_$$SENSORTEMP$$C_$$IMAGETYPE$$_$$FRAMENR$$
  ```

  It saves `Cep-01-Q1b1x1_2026-09-03_23-06-45_20.00sec_-0.00C_LIGHT_0152.fits`
  in `<NINA image folder>\2026-09-03\LIGHT\`, a new folder for every night. Set
  `SAI_CAMERA_DIRECTORY` to tonight's `...\LIGHT` folder.
- Keep `$$DATETIME$$` in the pattern. Every image needs a name that is never
  used again: an image whose name is already in the processed folder is taken
  for one that was sent, and skipped without a word. The server also needs at
  least two `_`-separated parts after the field name, which this pattern gives.
- Take the `SAI_COUNT` images of a field one after the other, none more than
  10 minutes after the one before.

### Trying it out

The first complete visit shows whether everything fits together: the console
prints `Successfully uploaded: <archive name>`, and usually a few minutes later
the results of that field are on the server's web page. Ask the server
administrator to check that they were processed with your camera's settings and
with matching darks and flats, and that the server found reference images for
the field.

While a new setup is being tried out, add `_test` to the camera tag
(`SAI_POSTFIX=_TTUQ1b1x1_test`): the server processes those uploads as usual,
but sends no alerts about them and keeps them off the candidate lists. Remove
it once everything works.

Archives smaller than 2 MB are refused by the server, so a try with tiny dummy
images gets `outside allowed range` back. That is expected, but such an archive
can never be delivered: it stays in `temp`, is tried again and again, and after
3 refusals a warning stays on the screen. Delete it from `temp` when the try is
over.

### Replacing the Python astrocam script

Stop the old script and start astrocam-go in its place: it reads the same
`config.env` and `areas.txt`. A `SAI_INTERVAL` below 15 is raised to 15.

### Upgrading

Stop astrocam-go with Ctrl+C, replace the program file with the new one under
the same name (the install scripts named it `astrocam.exe` or `astrocam`), and
start it again; or, if it was installed with an install script (into
`C:\AstroCam` or `/opt/astrocam`), run the install script of the new release:
it replaces the program there and keeps `config.env` and `areas.txt`. Check
that the second status line shows the new version. Keep `config.env`,
`areas.txt` and the `temp` folder: `temp` may hold archives that have not been
uploaded yet, and they are sent after the restart.

A window without the status lines described below runs a release older than
v0.6.0; `astrocam-go -version` names it. Releases up to v0.3.0 watched only
the configured folder and tonight's, so after an upgrade from one of them,
complete visits left in older night folders are sent too.

## Starting and stopping

Start the program by double-clicking it, or from a terminal:

```
C:\AstroCam\astrocam-go-win64.exe
./astrocam-go
```

astrocam-go does not start by itself when the computer starts. Start it again
after every restart of the camera computer (Windows updates restart it too), or
put a shortcut to it into the Startup folder (Win+R, then `shell:startup`) so
that it starts at logon. Images taken while it was not running are packed and
sent once it runs.

At startup it says which files, server and folders it is using. Check them:

```
Windows QuickEdit mode disabled (text selection will not freeze the program)
2026/11/22 18:02:11 Using config file: C:\AstroCam\config.env
2026/11/22 18:02:11 Using areas file: C:\AstroCam\areas.txt
=== ASTROCAM STARTING IN NORMAL OPERATION MODE ===
Version: v0.6.0
...
Configuration:
  Scan interval: 15 seconds (minimum: 15)
  Upload server: https://your-server.com/cgi-bin/unmw/upload.py
  Files per archive: 2
  Camera directory: C:\Users\observer\Documents\N.I.N.A\2026-11-22\LIGHT
  Processed directory: C:\AstroCam\processed
  Temp directory: C:\AstroCam\temp
  ...
  Authentication: Enabled (username: nmw)
========================================
Watching 5 folders for images:
  C:\Users\observer\Documents\N.I.N.A\2026-11-22\LIGHT
  C:\Users\observer\Documents\N.I.N.A\2026-11-21\LIGHT
  C:\Users\observer\Documents\N.I.N.A\2026-11-20\LIGHT
  ... and 2 older ones beside them
```

- The first line, on Windows only, means that a click into the window no longer
  pauses the program.
- If `Using config file:` is missing and there is a
  `Warning: Could not find config.env` instead, astrocam-go runs without
  settings and cannot upload anything.
- The folders of past nights stay on the `Watching` list until they are moved
  or deleted, so the list grows by one folder each night.
- Only one copy can run from a folder: a second one stops at once with
  `another instance of astrocam-go is already running`. Copies in different
  folders, each with its own `config.env` and camera folder, do not disturb
  each other (one per camera, for example).
- **Ctrl+C** stops it at once, also in the middle of an upload. Archives that
  have not been uploaded stay in `temp` and are sent the next time it runs:

  ```
  Stop requested (interrupt). Stopping...
  astrocam-go stopped. 2 archives (66.8 MB) not yet uploaded: kept in
    C:\AstroCam\temp
    and sent the next time astrocam-go runs.
  ```

- `astrocam-go -version` prints the version and exits. A release prints its
  tag (`v0.6.0`); a program built by hand prints `dev` and the source revision.
- `astrocam-go -test` is only for the automated tests of the program. It is not
  a dry run: it packs, moves and uploads real images like a normal run. To try
  out a new setup, see [Trying it out](#trying-it-out).

## The console

All is well when no line near the bottom of the window starts with `!!!`, the
first status line says `Server OK at ...` (or `Server not contacted yet` if
nothing has been ready to upload since astrocam-go was started),
`Last image at` moves on while the telescope is taking images, and the number
of queued archives does not keep growing. A problem that lasts is marked with
`!!!` and stays on the screen until it is over; the one exception is a camera
folder that exists but cannot be read, which gets a `Warning:` line on every
cycle. A few things that happen once are said in a single `Warning:` line, for
example an image whose copy reached the processed folder while its original
could not be deleted.

### The status lines

Every cycle ends with the status lines, two when all is well:

```
05:23:13 Server OK at 05:22:58. 38 archives uploaded, last at 05:22:58.
  v0.6.0 Queue empty. Last image at 05:23:02. 1 field waits for its 2nd image.
```

| It says | It means |
|---------|----------|
| `05:23:13` | when this cycle ended |
| `Server OK at 05:22:58` | the last time the server said it is taking uploads. The server is asked only when there is an archive to upload, so on a quiet night this time can be hours old; before the first archive it reads `Server not contacted yet`. It reads `Fallback server OK at ...` when `SAI_FALLBACK_SERVER` took the uploads |
| `..., but it is LOW ON DISK SPACE` | the server takes uploads but warns that its disk is filling up |
| `Server answered at 05:22:58 but gave no status` | the server answers, but not in the way an NMW upload server does: check `SAI_SERVER` |
| `38 archives uploaded, last at 05:22:58` | archives the server has confirmed since astrocam-go was started |
| `v0.6.0` | the version that is running |
| `Queue empty` | nothing is waiting to be uploaded. Otherwise `3 archives queued, next upload at 05:24:06` |
| `Last image at 05:23:02` | the newest image of a field from `areas.txt` that astrocam-go has seen. If this time stands still while the telescope is working, the camera software is not saving where, or not naming the files the way, astrocam-go expects |
| `1 field waits for its 2nd image` | a field has some of its `SAI_COUNT` images, the newest less than 10 minutes old |

A time more than 12 hours ago is shown with its date (`last at Nov 22 05:14`).

### Packing and uploading

```
Processing file: C:\Users\observer\Documents\N.I.N.A\2026-11-22\LIGHT\Gem-02-Q2b1x1_2026-11-23_05-20-39_20.00sec_-0.00C_LIGHT_0152.fits
Processing file: C:\Users\observer\Documents\N.I.N.A\2026-11-22\LIGHT\Gem-02-Q2b1x1_2026-11-23_05-21-20_20.00sec_-0.00C_LIGHT_0153.fits
Creating ZIP archive: 2026-11-23_CI_Gem-02-Q2b1x1_052206_TTUQ2b1x1.zip (area Gem-02-Q2b1x1, 2 frames from C:\Users\observer\Documents\N.I.N.A\2026-11-22\LIGHT)
Archive created: 2026-11-23_CI_Gem-02-Q2b1x1_052206_TTUQ2b1x1.zip
Uploading 2026-11-23_CI_Gem-02-Q2b1x1_052206_TTUQ2b1x1.zip to server: https://your-server.com/cgi-bin/unmw/upload.py
Using authentication for upload
Sending 33.4 MB to https://your-server.com/cgi-bin/unmw/upload.py (giving up after 15m0s)
  upload: 13.8 MB of 33.4 MB (41%) at 705.0 kB/s, about 29s left
  upload: 27.5 MB of 33.4 MB (82%) at 705.0 kB/s, about 9s left
  upload: 33.4 MB delivered and answered in 52s
Successfully uploaded: 2026-11-23_CI_Gem-02-Q2b1x1_052206_TTUQ2b1x1.zip
```

The archive name is `<date>_<SAI_PREFIX><field>_<time><SAI_POSTFIX>`, with the
date and time of packing. Only `Successfully uploaded:` means that the server
has the archive: the `delivered and answered` line is printed for every
answer, a refusal too.

On a slow link the `upload:` lines show how the transfer is going. One upload
may take up to 15 minutes. An upload that cannot finish in that time starts
again from the beginning on every try, so it keeps failing until the link is
faster.

### When something is wrong

A problem with the server is named in the first status line for as long as it
lasts, and a box with the details, the server's own words and what to do is
printed when it starts and every 20 cycles (5 minutes at the default 15
seconds) after that:

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
!!! 2026-11-23_CI_Leo-01-Q2b1x1_005110_TTUQ2b1x1.zip
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!

00:51:12 !!! SERVER IS OUT OF DISK SPACE for 0s. Next try at 01:51:12.
         12 archives uploaded, last at 00:49:03.
  v0.6.0 1 archive queued. Last image at 00:50:41.
```

Hours later it still says:

```
05:23:19 !!! SERVER IS OUT OF DISK SPACE for 4h 32m. Next try at 05:51:40.
         12 archives uploaded, last at 00:49:03.
  v0.6.0 50 archives queued. Last image at 05:22:41.
```

and when it is over:

```
=== RESOLVED: Server is out of disk space - the upload server answers
=== its status check normally again (the problem lasted 5h 00m).
```

| First status line | What it means | What to do |
|-------------------|---------------|------------|
| `SERVER IS OUT OF DISK SPACE` | The server has no room for uploads. Tried again every hour. | Tell whoever runs the server: disk space has to be freed there. Nothing to do on the camera computer. |
| `SERVER IS BUSY: SYSTEM LOAD TOO HIGH` | The server is overloaded. Tried again after 10 minutes. | Nothing, unless it lasts for hours. |
| `SERVER REPORTED A TEMPORARY ERROR` | The server refused for another reason, given in the box. Tried again after 10 minutes. | Tell whoever runs the server if it lasts. |
| `UPLOAD SERVER IS UNREACHABLE` | No connection to the server at all. Tried again every cycle, and on `SAI_FALLBACK_SERVER` if there is one. | Check the network, that the server is up, and that `SAI_SERVER` is spelled right (a wrong host name looks the same). |
| `SERVER REFUSES THE LOGIN (HTTP 401)`, `SERVER REFUSES ACCESS (HTTP 403)` | The server does not accept `SAI_USERNAME` and `SAI_PASSWORD`. Tried again after 10 minutes. | Correct them in `config.env` and restart astrocam-go. |
| `UPLOAD ADDRESS NOT FOUND ON THE SERVER (HTTP 404)` | There is no upload script at `SAI_SERVER`. | Correct `SAI_SERVER` and restart. |
| `SERVER REDIRECTS UPLOADS ELSEWHERE (HTTP 301)` | `SAI_SERVER` is an old address, or `http://` where the server wants `https://`. | Put the address shown in the box into `SAI_SERVER` and restart. |
| `SERVER-SIDE ERROR (HTTP 502)` (or another 5xx) | The server, or a proxy in front of it, is broken. Tried again after 10 minutes. | Tell whoever runs the server. Nothing needs changing on the camera computer. |

Restarting astrocam-go always tries again at once. Other problems get a line of
their own under the status lines, and a box when they start and every 20
cycles, for example:

```
!!! ERROR 5m 00s: No camera image directory can be found
!!! ERROR 2m 15s: Images cannot be packed into archives
!!! ERROR 32m 10s: Uploads do not finish within 15 minutes
!!! WARNING 12m 30s: The server keeps refusing one archive
```

See [Troubleshooting](#troubleshooting) for what each of them means.

### Images that are not uploaded

An image that is not uploaded has a reason, and in most cases the console gives
it:

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

While there are such files, a further status line counts them:
`NOTE: 4 recent FITS files match no field in areas.txt (ignored).` It goes away
by itself an hour after the last such file was written. If the telescope is
also taking images of targets that are not survey fields, this note is expected
and can be ignored.

Images of an incomplete visit stay in the camera folder. astrocam-go never
deletes them; clean them up by hand.

## Disk space

astrocam-go deletes no image. Every image it packs is moved into the processed
folder, so the drive that holds it fills by the size of each night's images -
tens of gigabytes a night for a large camera.
While the server does not take uploads, the archives in `temp` pile up as well.
Check the free space every few days:

- the images in the processed folder have all been packed, and once the second
  status line says `Queue empty`, their archives have reached the server: move
  or delete them as you see fit;
- never delete anything in `temp`: it holds archives that have not been
  uploaded yet.

When the drive with the `temp` folder is full,
`Images cannot be packed into archives` appears under the status lines. When
the processed folder is on another drive than the camera folder and that drive
is full, `Packed images cannot be moved into the processed folder` appears, and
nothing is uploaded until there is room. When the drive the camera software
saves to is full, the camera software cannot save images, and `Last image at`
stands still.

## Troubleshooting

**The window closes right after the start.** astrocam-go stopped because it
cannot run: `areas.txt` is missing, the program's folder is not writable, the
folder in `SAI_PROCESSED_DIRECTORY` cannot be created (a drive that does not
exist, no permission), or another copy is already running from the same folder.
Start it from a Command Prompt to read the message.

**`No camera image directory can be found`.** None of the watched folders
exists. On a new installation, or after the folders of past nights have been
moved away, this is normal until the camera software saves the first image of
the night, which creates tonight's folder; the message then ends with a
`RESOLVED` line. If it stays while images are being saved, compare
`SAI_CAMERA_DIRECTORY` with the folder the images are saved in.

**`No images seen yet`, or `Last image at` stands still while the telescope
is working.** astrocam-go does not see the images. Compare `Camera directory`
and the `Watching ... folders for images` list printed at startup with where
the camera software really saves. The images have to be FITS files lying
directly in a watched folder, not in a subfolder of it. If `SAI_CAMERA_DIRECTORY`
ends in the date folder itself (`...\2026-11-22`), only that one night is
watched.

**`NOTE: N recent FITS files match no field in areas.txt`.** The images are
there, but their names do not start with a field name from `areas.txt`
followed by `_`. The note shows one of the names. Check the NINA file pattern
and that the target names are spelled exactly like the lines in `areas.txt`,
including upper and lower case.

**`Field X: only 1 of 2 images arrived`.** Fewer than `SAI_COUNT` images of
the field were taken, each within 10 minutes of the one before. Check that
`SAI_COUNT` is the number of images the sequence takes per field. If a visit
has more images than `SAI_COUNT`, they are packed `SAI_COUNT` at a time, and
what is left over gets this message: of 5 images with `SAI_COUNT=2`, 4 are
uploaded and 1 stays.

**`Packed images cannot be moved into the processed folder`.** Another program
(an image viewer, a virus scanner) has the images open, or the processed folder
is full or cannot be written to; the `Error: Cannot move file` lines above it
give the reason. Until the images can be moved, their archive is thrown away
again on every cycle and nothing of them is uploaded; nothing is lost.

**`Uploaded images are still in the camera folder`.** Only some images of an
archive could be moved, and the archive was uploaded anyway; the box names the
images left behind. Move them into the processed folder by hand, or they may
be sent a second time or reported as an incomplete visit. The warning ends when
they are gone from the camera folder.

**`Images cannot be packed into archives`.** The archive could not be built in
the `temp` folder, usually because its drive is full; the box shows the error.
The images stay in the camera folder and are packed as soon as there is room.

**An image is never uploaded and nothing is said.** An image whose name is
already in the processed folder is treated as sent and skipped. This happens
when the file name pattern repeats names (keep `$$DATETIME$$` in it), and to
every image when `SAI_PROCESSED_DIRECTORY` is the camera folder itself. To send
images again, move them (do not copy them) from the processed folder back into
the camera folder.

**`INFO: ... is not a complete FITS file`.** The image was cut short (a crash,
a full disk), or it is not a standard FITS file. It is not uploaded. If every
image gets this message, the camera software does not write standard FITS
files.

**`Archive ... is damaged and can never be delivered`.** astrocam-go could not
read back an archive in `temp` at startup and moved it to `temp\bad`; nothing
was deleted. Images from it that are still in the camera folder are packed
again by themselves. Its images in the processed folder may already have gone
out in another archive: move them back into the camera folder only if the
server never got them. If in doubt, ask the server administrator whether that
field arrived that night.

**`Uploads do not finish within 15 minutes`.** The server answers its status
check, but an upload ran out of time; the box says whether the link was too
slow, nothing could be sent, or the server did not answer in time. Every try
starts again from the beginning, so a link too slow for one archive in 15
minutes never gets it through, and the archives behind it wait too. Check the
network link; if the whole archive reached the server but no answer came, the
server is too slow: tell whoever runs it. The problem ends when an upload is
answered again, or when no archive is waiting any more.

**The queue grows while the first status line says `Server OK`.** If there are
no `Upload error` lines, the uploads cannot keep up with the camera (30 an hour
at most, fewer on a slow link); the queue empties when the camera stops.
Otherwise the `Upload error` lines say why each upload failed: the server's
reason is in the line, and after 3 refusals of one archive a warning appears
under the status lines (`The server refuses every archive it is sent` means
that the fault is on the server).

**`The server keeps refusing one archive`.** The server accepts other archives
but not this one; the box shows the server's reason. The archive stays in
`temp` and is tried again after the others. Move it out of `temp` by hand to
stop that.

**`SERVER REFUSES THE LOGIN` although the password is right.** A `#` in the
password starts a comment in `config.env` and cuts the password short, and
quotes around it become part of it. A password with a `#` cannot be used: ask
for another one.

**`UPLOAD SERVER IS UNREACHABLE` although the computer is online.** The box
shows the network error. astrocam-go does not use the Windows proxy settings:
on a network that needs a proxy, set the environment variable `HTTPS_PROXY` to
the proxy's address (for an `http://` address in `SAI_SERVER` it is
`HTTP_PROXY`), for example with `setx HTTPS_PROXY http://proxy.example.org:3128`
in a Command Prompt. Then stop astrocam-go and start it again from a new
window: a running copy does not see the new setting. A certificate error
(`x509: ...`) in the box means that the computer's clock is wrong, that its
root certificates are out of date, or that the server's certificate has
expired.

**Reporting a problem.** Send a photo or a screenshot of the console window to
whoever runs the server; the second status line shows the version. Faults in
astrocam-go itself go to
[its issue tracker](https://github.com/kirxkirx/astrocam-go/issues).

## For developers

astrocam-go is written in Go (1.21 or later) and uses only the standard
library. The Linux build is made without cgo, as for a release, so that it also
runs on older Linux systems.

```
go vet ./... && go test ./...
CGO_ENABLED=0 go build -o astrocam-go
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o astrocam-go-win64.exe
GOOS=windows GOARCH=386   go build -ldflags="-s -w" -o astrocam-go-win32.exe
```

- **CI** (`.github/workflows/ci.yml`) runs the unit tests, builds for Linux and
  Windows, and runs the program on both with an upload address where nothing
  answers. On Linux it also runs it against a mock server that reports a full
  disk. On pushes to `main` it uploads a tiny archive to a real server, set by
  the repository secret `TEST_SERVER_URL`; that server refuses it for its size,
  which proves that it was reached.
- **Releases** (`.github/workflows/release.yml`): pushing a tag such as `v0.6.0`
  builds the programs with that version and publishes a GitHub release. It does
  not run the tests, so push `main` and wait for CI to pass first.
- `compile_and_test.sh` runs the unit tests, builds everything and runs the
  program on test images in the folder it is started in. Its first test run
  ends in `TEST FAILED`, because nothing answers at its test address; only the
  disk-space test after it means something. It overwrites `config.env` and
  empties `temp` in that folder, so never run it in an installation folder.
