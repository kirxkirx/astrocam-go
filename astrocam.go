package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Constants matching Python version
const (
	ERROR = "ERROR"
	EMPTY = "EMPTY"

	// Interval configuration constants
	MIN_INTERVAL     = 15    // Minimum allowed interval in seconds
	DEFAULT_INTERVAL = 15    // Default interval if not specified/invalid
	MAX_INTERVAL     = 86400 // Maximum allowed interval in seconds (24 hours)

	// Frames per archive. The server refuses an archive holding fewer than two
	// images, so a smaller count would only produce archives that can never be
	// delivered; the upper bound just catches a typing mistake.
	MIN_COUNT     = 2
	MAX_COUNT     = 1000
	DEFAULT_COUNT = 3

	// How long to pause uploads after a server-side rejection, by cause.
	HIGH_LOAD_PAUSE  = 10 * time.Minute // server reported high system load
	DISK_SPACE_PAUSE = 1 * time.Hour    // server reported out of disk space

	// ninaDateFolderOffset is subtracted from the current time to derive the
	// date component of NINA's default output folder (.../<YYYY-MM-DD>/LIGHT).
	// NINA rolls that folder at local noon, i.e. it names the folder after
	// (now - 12h), so a whole night's images share one folder.
	ninaDateFolderOffset = 12 * time.Hour

	// visitGap bounds the time between consecutive frames of one visit to a
	// field. Frames further apart than this belong to different visits and
	// must never share an archive, because the server checks the frames of a
	// visit against each other. This matches VISIT_GROUP_MAX_GAP_DAYS = 0.007
	// (10.1 minutes) in the unmw monitoring code; keep the two in step.
	visitGap = 10 * time.Minute

	// fitsBlockSize is the FITS record length. A complete FITS file is a whole
	// number of these records, which is how a half-written frame is spotted.
	fitsBlockSize = 2880

	// uploadPhaseBudget bounds how long one cycle may spend uploading, so that
	// a long retry queue can never starve the packing of new images.
	uploadPhaseBudget = 10 * time.Minute

	// How often the upload prints how far it has got.
	uploadProgressInterval = 20 * time.Second

	// uploadThrottleDelay is the shortest time between the starts of two
	// uploads. Every upload starts a processing run on the server, so they are
	// spaced out rather than sent back to back.
	uploadThrottleDelay = 120 * time.Second

	// recentFileWindow is how far back a FITS file that matches no field is
	// still worth mentioning. While the camera software is naming its images
	// in a way this program cannot use, such files keep arriving and the note
	// about them stays on screen; an hour after the naming has been put right,
	// or after a few frames of some other target, it goes away by itself.
	recentFileWindow = time.Hour

	// consoleWidth is the longest line the status report prints. The telescope
	// machines run an 80 column console, and a line that reaches the last
	// column wraps and leaves an empty row behind it.
	consoleWidth = 79

	// statusIndent starts every status line after the first. It is as wide as
	// the time of day that starts the first one, so the text lines up.
	statusIndent = "         "
)

// uploadTimeout bounds one whole upload attempt. Go counts the CONNECTION,
// the time spent handing the archive to the network, the server's processing
// and the reply against this single figure, so it has to cover a big archive
// on a slow uplink: a 35 MB archive needs about 1 Mbit/s to finish inside
// 300 s, which a poor link does not always have. An attempt that runs out of
// time here is retried from the beginning on the next cycle, so a link that
// cannot deliver the archive within this limit never delivers it at all -
// hence the generous value. It is a variable only so that the tests can make
// an upload run out of time without waiting 15 minutes.
var uploadTimeout = 900 * time.Second

// errStopping marks an operation that was abandoned because the operator asked
// the program to stop. Nothing is wrong with the archive or with the server.
var errStopping = errors.New("stop requested")

// errLocalFailure marks an upload that failed before anything was sent,
// because the archive could not be read from this computer's disk. The server
// has not seen the archive, and must not be said to have refused it.
var errLocalFailure = errors.New("could not read the archive")

// errServerUnreachable marks a preflight/upload failure caused by the server
// being unreachable at the network level (DNS failure, timeout, connection
// refused) - as opposed to a server-side rejection (out of disk space, high
// load). Only an unreachable primary server triggers fail-over to the optional
// SAI_FALLBACK_SERVER; a reachable server that rejects the upload is handled by
// the existing pause-and-retry logic instead.
var errServerUnreachable = errors.New("server unreachable")

// errUploadTimeout marks an upload that ran out of time. The server answered
// the preflight and accepted the connection, so it is NOT unreachable: either
// the link is too slow to deliver the archive, or the server is taking too
// long to reply. Keeping this apart from errServerUnreachable is what stops
// the log from blaming a perfectly reachable server.
var errUploadTimeout = errors.New("upload timed out")

type Config struct {
	Server             string
	FallbackServer     string
	Username           string
	Password           string
	CameraDirectory    string
	ProcessedDirectory string
	Interval           int
	RequestedInterval  int // Store the original requested interval
	Count              int
	Prefix             string
	Postfix            string
	ArchiveMode        string // "auto", "rar", "zip", "zip-uncompressed"
}

type AstroCam struct {
	config           *Config
	areas            []string
	tempDirectory    string
	currentDir       string
	lastUploadTime   time.Time
	useRAR           bool   // Whether to use RAR (true) or ZIP (false)
	archiveExt       string // ".rar" or ".zip"
	zipCompressed    bool   // Whether to compress ZIP files
	rarPath          string // Path to rar executable (if found)
	testMode         bool   // Whether running in test mode
	testStartTime    time.Time
	fitsExtPattern   string                       // Regex pattern matching all FITS file extensions (.fts, .fits, .fit)
	uploadPauseUntil time.Time                    // Skip uploads until this time after a server-side rejection (high load or out of disk space)
	areaPatterns     []*regexp.Regexp             // one compiled matcher per areas entry, same order
	stamps           map[string]fileStamp         // size and mtime of each candidate on the previous cycle
	rejects          map[string]int               // per-archive count of server content rejections
	refusedAfter     map[string]int               // uploadedCount when each archive still waiting was first refused
	lastScanSet      string                       // last scan set printed, so it is logged only when it changes
	conditions       map[string]*problemCondition // standing problems, by kind; empty when all is well
	queueSummary     string                       // one-line description of what is waiting to be delivered
	queuedCount      int                          // how many archives are waiting, for the status lines
	queuedBytes      int64                        // their total size

	// Everything below feeds the status lines printed at the end of each cycle.
	// The zero value of each field means "nothing known yet", so an AstroCam
	// built by hand in a test needs none of it set.
	ctx            context.Context // cancelled when the operator asks the program to stop
	label          string          // release tag or build id of this binary
	cycle          int             // scan cycles started so far, for pacing repeated notes
	uploadedCount  int             // archives the server has confirmed during this run
	lastUploadOK   time.Time       // when the most recent of them was confirmed
	serverAskedAt  time.Time       // when a server last answered at all, whatever it said
	serverOKAt     time.Time       // when a server last said it is taking uploads
	serverOKName   string          // what to call that server; empty means "Server"
	serverWarning  string          // the server's own warning in a few words; empty when none
	lastImageAt    time.Time       // modification time of the newest field image any scan has seen
	waitingFields  int             // fields that have some, but not yet all, images of a set
	openFields     map[string]bool // fields that had new images within visitGap on the previous scan
	leftoversTold  bool            // the summary of old incomplete sets has been printed
	strayCount     int             // recent FITS files that match no field in areas.txt
	stranded       map[string]bool // images of uploaded archives that could not leave the camera folder
	strayTold      int             // how many there were when they were last explained
	strayToldCycle int             // the cycle in which they were last explained
}

// problemCondition is a fault that outlasts the moment it happened - the server
// being out of disk space, an unreachable network, and so on.
//
// The program prints plenty of one-shot messages, but an operator does not read
// a log, they GLANCE at a terminal. Anything announced once scrolls out of sight
// within a minute and the window then shows nothing but cheerful scanning
// chatter - which is exactly how an hour-long upload outage went unnoticed on
// 2026-09-12. So a condition is held in memory and restated on EVERY cycle for
// as long as it lasts, and its end is announced too: the operator should never
// have to wonder whether the problem went away or merely stopped being
// mentioned.
//
// An empty (or nil) AstroCam.conditions map means "healthy", so an AstroCam
// built by hand in a test needs no new initialisation.
type problemCondition struct {
	severity string    // "ERROR" or "WARNING"
	reason   string    // short cause, e.g. "server is out of disk space"
	detail   string    // the server's own words, or the underlying error text
	action   string    // what astrocam-go is doing about it, in plain language
	since    time.Time // when it was first seen
	until    time.Time // when a timed pause expires; zero if not time-based
	cycles   int       // how many scan cycles it has lasted

	// fromStatusCheck is set when the server's status check (the preflight)
	// raised the problem. Such a problem is over as soon as that same check
	// comes back clean. A problem raised by the answer to an upload is not: the
	// status check may pass while every upload is still turned away.
	fromStatusCheck bool
}

type FileGroup struct {
	FilesToArchive []string
	FilesToDelete  []string
}

// findConfigFile looks for a config file in multiple locations:
// 1. Next to the executable (preferred)
// 2. Current working directory (fallback)
func findConfigFile(filename string) (string, error) {
	// Get executable directory
	execPath, err := os.Executable()
	if err == nil {
		execDir := filepath.Dir(execPath)
		configPath := filepath.Join(execDir, filename)
		if _, err := os.Stat(configPath); err == nil {
			return configPath, nil
		}
	}

	// Fall back to current directory
	if _, err := os.Stat(filename); err == nil {
		return filename, nil
	}

	return "", fmt.Errorf("config file %s not found in executable directory or current directory", filename)
}

func loadConfig() *Config {
	config := &Config{
		Interval:          DEFAULT_INTERVAL, // Use default instead of hardcoded 180
		RequestedInterval: DEFAULT_INTERVAL, // Initialize both to default
		Count:             DEFAULT_COUNT,
		ArchiveMode:       "auto", // default
	}

	// Look for config.env in executable directory first, then current directory
	configPath, err := findConfigFile("config.env")
	if err != nil {
		log.Printf("Warning: Could not find config.env: %v", err)
		return config
	}

	file, err := os.Open(configPath)
	if err != nil {
		log.Printf("Warning: Could not read config.env: %v", err)
		return config
	}
	defer file.Close()

	log.Printf("Using config file: %s", configPath)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])

		// Remove inline comments (everything after # character)
		if commentPos := strings.Index(value, "#"); commentPos != -1 {
			value = strings.TrimSpace(value[:commentPos])
		}

		switch key {
		case "SAI_SERVER":
			config.Server = value
		case "SAI_FALLBACK_SERVER":
			config.FallbackServer = value
		case "SAI_USERNAME":
			config.Username = strings.TrimSpace(value)
		case "SAI_PASSWORD":
			config.Password = strings.TrimSpace(value)
		case "SAI_CAMERA_DIRECTORY":
			config.CameraDirectory = value
		case "SAI_PROCESSED_DIRECTORY":
			config.ProcessedDirectory = value
		case "SAI_INTERVAL":
			// Handle interval with validation and fallback
			if value == "" {
				// Empty value - use default
				config.RequestedInterval = DEFAULT_INTERVAL
				config.Interval = DEFAULT_INTERVAL
			} else if val, err := strconv.Atoi(value); err != nil {
				// Invalid value - use default
				fmt.Printf("Warning: Invalid SAI_INTERVAL '%s', using default %d seconds\n", value, DEFAULT_INTERVAL)
				config.RequestedInterval = DEFAULT_INTERVAL
				config.Interval = DEFAULT_INTERVAL
			} else if val > MAX_INTERVAL {
				// Too large - use default
				fmt.Printf("Warning: SAI_INTERVAL %d exceeds maximum %d seconds, using default %d seconds\n",
					val, MAX_INTERVAL, DEFAULT_INTERVAL)
				config.RequestedInterval = val // Store what was requested
				config.Interval = DEFAULT_INTERVAL
			} else {
				// Valid value - store it (will be enforced to minimum later)
				config.RequestedInterval = val
				config.Interval = val
			}
		case "SAI_COUNT":
			if val, err := strconv.Atoi(value); err != nil {
				fmt.Printf("Warning: Invalid SAI_COUNT '%s', using %d\n", value, config.Count)
			} else if val < MIN_COUNT || val > MAX_COUNT {
				// A negative count used to reach make([]string, negative) and
				// crash the program on the first field of the night.
				fmt.Printf("Warning: SAI_COUNT %d is outside %d..%d, using %d\n",
					val, MIN_COUNT, MAX_COUNT, config.Count)
			} else {
				config.Count = val
			}
		case "SAI_PREFIX":
			config.Prefix = value
		case "SAI_POSTFIX":
			config.Postfix = value
		case "SAI_ARCHIVE_MODE":
			mode := strings.TrimSpace(strings.ToLower(value))
			if mode != "" {
				config.ArchiveMode = mode
			}
		}
	}

	return config
}

func loadAreas() ([]string, error) {
	// Look for areas.txt in executable directory first, then current directory
	areasPath, err := findConfigFile("areas.txt")
	if err != nil {
		return nil, fmt.Errorf("could not find areas.txt: %w", err)
	}

	file, err := os.Open(areasPath)
	if err != nil {
		return nil, fmt.Errorf("could not open areas.txt: %w", err)
	}
	defer file.Close()

	log.Printf("Using areas file: %s", areasPath)

	var areas []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			areas = append(areas, line)
		}
	}
	return areas, scanner.Err()
}

// findRARExecutable checks for rar command in PATH and Windows default locations
func findRARExecutable() (string, bool) {
	// First try PATH (works on Linux and Windows if rar is in PATH)
	if rarPath, err := exec.LookPath("rar"); err == nil {
		return rarPath, true
	}

	// On Windows, also check common WinRAR installation locations
	if runtime.GOOS == "windows" {
		commonPaths := []string{
			`C:\Program Files\WinRAR\rar.exe`,
			`C:\Program Files (x86)\WinRAR\rar.exe`,
		}

		for _, path := range commonPaths {
			if _, err := os.Stat(path); err == nil {
				return path, true
			}
		}
	}

	return "", false
}

// fitsExtensionPattern returns a regex fragment matching all supported FITS file extensions.
// The extension match is case-insensitive because cameras differ over .fits
// and .FITS; the area name in front of it stays case-sensitive.
const fitsExtensionPattern = `\.(?i:fts|fits|fit)`

// fitsNamePattern matches any file name with a FITS extension, whatever comes
// before it. It is what tells an image that belongs to no field apart from the
// other things an imaging program leaves in its folders.
var fitsNamePattern = regexp.MustCompile(fitsExtensionPattern + `$`)

// determineArchiveSettings determines archive format based on config and availability
func determineArchiveSettings(config *Config) (useRAR bool, zipCompressed bool, archiveExt string, rarPath string) {
	rarPath, rarAvailable := findRARExecutable()

	// Set defaults
	useRAR = false
	zipCompressed = true
	archiveExt = ".zip"

	switch config.ArchiveMode {
	case "rar":
		if rarAvailable {
			useRAR = true
			archiveExt = ".rar"
		} else {
			fmt.Printf("Warning: RAR mode requested but rar command not found, falling back to compressed ZIP\n")
		}
	case "zip":
		useRAR = false
		zipCompressed = true
		archiveExt = ".zip"
	case "zip-uncompressed":
		useRAR = false
		zipCompressed = false
		archiveExt = ".zip"
	default:
		if config.ArchiveMode != "auto" {
			fmt.Printf("Warning: unknown SAI_ARCHIVE_MODE '%s' (use auto, rar, zip or zip-uncompressed), using auto\n",
				config.ArchiveMode)
			config.ArchiveMode = "auto"
		}
		// Auto mode: prefer RAR if available, otherwise compressed ZIP
		if rarAvailable {
			useRAR = true
			archiveExt = ".rar"
		} else {
			useRAR = false
			zipCompressed = true
			archiveExt = ".zip"
		}
	}

	return useRAR, zipCompressed, archiveExt, rarPath
}

func NewAstroCam(testMode bool) (*AstroCam, error) {
	config := loadConfig()
	areas, err := loadAreas()
	if err != nil {
		return nil, err
	}

	// Determine archive settings based on config
	useRAR, zipCompressed, archiveExt, rarPath := determineArchiveSettings(config)

	modeStr := "NORMAL OPERATION"
	if testMode {
		modeStr = "TEST"
	}

	fmt.Printf("=== ASTROCAM STARTING IN %s MODE ===\n", modeStr)
	fmt.Printf("Version: %s\n", buildLabel())

	// Determine executable directory (matching Python logic)
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("could not get executable path: %w", err)
	}

	baseDir := filepath.Dir(execPath)
	tempDir := filepath.Join(baseDir, "temp")

	// Create temp directory if it doesn't exist
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("could not create temp directory: %w", err)
	}

	// Clear out anything an unclean shutdown left in the staging directory. A
	// half-written archive there is never uploaded, but it would otherwise sit
	// on the disk for ever, and these files are the size of a whole visit.
	incomingDir := filepath.Join(tempDir, "incoming")
	if leftovers, err := os.ReadDir(incomingDir); err == nil {
		for _, e := range leftovers {
			if e.IsDir() {
				continue
			}
			if err := os.Remove(filepath.Join(incomingDir, e.Name())); err == nil {
				fmt.Printf("Removed a partial archive left by an earlier run: %s\n", e.Name())
			}
		}
	}

	// Set default directories if not specified
	if config.CameraDirectory == "" {
		config.CameraDirectory = filepath.Join(baseDir, "data")
	}
	if config.ProcessedDirectory == "" {
		config.ProcessedDirectory = filepath.Join(baseDir, "processed")
	}

	// Create processed directory if it doesn't exist
	if err := os.MkdirAll(config.ProcessedDirectory, 0755); err != nil {
		return nil, fmt.Errorf("could not create processed directory: %w", err)
	}

	currentDir, _ := os.Getwd()

	ac := &AstroCam{
		config:         config,
		areas:          areas,
		tempDirectory:  tempDir,
		currentDir:     currentDir,
		lastUploadTime: time.Time{},
		useRAR:         useRAR,
		archiveExt:     archiveExt,
		zipCompressed:  zipCompressed,
		rarPath:        rarPath,
		testMode:       testMode,
		testStartTime:  time.Now(),
	}

	ac.fitsExtPattern = fitsExtensionPattern
	ac.stamps = make(map[string]fileStamp)
	ac.rejects = make(map[string]int)
	ac.refusedAfter = make(map[string]int)
	ac.conditions = make(map[string]*problemCondition)
	ac.label = buildLabel()

	// Compile one matcher per area once here, rather than rebuilding them on
	// every directory scan. QuoteMeta keeps an area name that happens to
	// contain a regular expression character matching itself literally.
	ac.areaPatterns = make([]*regexp.Regexp, 0, len(areas))
	for _, area := range areas {
		pattern := fmt.Sprintf("^%s(_|-SF_).*%s$", regexp.QuoteMeta(area), fitsExtensionPattern)
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("could not build a file name matcher for area %q: %w", area, err)
		}
		ac.areaPatterns = append(ac.areaPatterns, re)
	}

	ac.finishPartialCopies()
	return ac, nil
}

// scanDirs returns every directory that may hold images this cycle.
//
// The configured directory is always included. When it sits inside a dated
// folder (the .../<YYYY-MM-DD>/LIGHT layout that imaging software creates for
// each night) every other dated folder beside it is included as well, together
// with the folder for the current night even when it does not exist yet.
//
// There is deliberately no age limit here. A night's folder leaves the scan
// set when it runs out of images to pack, never because the clock moved on;
// that is what stops a complete set from becoming invisible at local noon.
func (ac *AstroCam) scanDirs() []string {
	dirs := appendUniqueDir(nil, ac.config.CameraDirectory)

	base, suffix, ok := splitDatedPath(ac.config.CameraDirectory)
	if !ok {
		return dirs
	}

	// The folder for the current night is listed even when it is missing, so
	// it is picked up on the first cycle after the imaging software makes it.
	current := append([]string{base, time.Now().Add(-ninaDateFolderOffset).Format(dateFolderLayout)}, suffix...)
	dirs = appendUniqueDir(dirs, filepath.Join(current...))

	// The enumerated folders are compared by cleaned path only. Checking each
	// one against every other with os.SameFile would cost a number of stat
	// calls that grows with the square of the number of nights, and the number
	// of nights grows for the life of the installation.
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		seen[d] = true
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		return dirs
	}

	var dated []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := time.Parse(dateFolderLayout, e.Name()); err != nil {
			continue
		}
		dated = append(dated, e.Name())
	}
	// Newest night first, so tonight's images are packed before old leftovers.
	sort.Sort(sort.Reverse(sort.StringSlice(dated)))

	for _, night := range dated {
		leaf := filepath.Join(base, night)
		resolved := true
		for _, part := range suffix {
			child, ok := resolveChild(leaf, part)
			if !ok {
				resolved = false
				break
			}
			leaf = child
		}
		if resolved {
			leaf = filepath.Clean(leaf)
			if !seen[leaf] {
				seen[leaf] = true
				dirs = append(dirs, leaf)
			}
		}
	}
	return dirs
}

// areaForFile returns the area a file name belongs to, or an empty string when
// it belongs to none of them.
func (ac *AstroCam) areaForFile(name string) string {
	for i, re := range ac.areaPatterns {
		if re.MatchString(name) {
			return ac.areas[i]
		}
	}
	return ""
}

// pendingFrame is an image of a known field that cannot be packed yet: the
// camera software may still be writing it, or it is not a complete FITS file.
type pendingFrame struct {
	area  string
	name  string
	mtime time.Time
}

// strayFile is a FITS file whose name matches no field in areas.txt.
type strayFile struct {
	name  string
	mtime time.Time
}

// dirScan is what one pass over one image directory found.
type dirScan struct {
	ready   map[string][]candidate // finished frames that may be packed, by field
	pending []pendingFrame         // frames of a known field that are not ready yet
	strays  []strayFile            // recent FITS files that belong to no field
}

// scanDir reads one directory once and sorts what it finds.
//
// A file is ready to be packed only when it matches an area, looks like a
// finished FITS file, has not changed since the previous cycle, and is not
// already sitting in the processed directory. Every one of those is a
// statement about the file itself, so a file that qualifies stays qualified
// until it is packed.
//
// Files of a known field that do not qualify yet, and recent FITS files that
// belong to no field at all, are returned as well. Neither is ever packed;
// they are there so the status lines can say what the camera is producing
// instead of leaving the operator to guess why nothing is being uploaded.
//
// The stamps map is filled in with what every matching file looks like now; it
// becomes the comparison point for the next cycle.
func (ac *AstroCam) scanDir(dir string, stamps map[string]fileStamp) dirScan {
	found := dirScan{ready: make(map[string][]candidate)}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("Warning: cannot read image directory %s: %v\n", dir, err)
		}
		return found
	}

	notBefore := time.Now().Add(-recentFileWindow)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		area := ac.areaForFile(name)
		if area == "" {
			if fitsNamePattern.MatchString(name) {
				if info, err := entry.Info(); err == nil && info.ModTime().After(notBefore) {
					found.strays = append(found.strays, strayFile{name: name, mtime: info.ModTime()})
				}
			}
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, name)
		now := fileStamp{size: info.Size(), mtime: info.ModTime()}
		stamps[path] = now

		// Unchanged since the previous cycle. A frame the camera software is
		// still writing fails this test and waits one more cycle.
		before, seen := ac.stamps[path]
		if !seen || before.size != now.size || !before.mtime.Equal(now.mtime) {
			found.pending = append(found.pending, pendingFrame{area: area, name: name, mtime: now.mtime})
			continue
		}
		obs, hasObs, complete := isCompleteFITS(path, now.size)
		if !complete {
			if !before.reported {
				fmt.Printf("INFO: %s is not a complete FITS file (%d bytes); it will not be packed until it is rewritten\n",
					path, now.size)
			}
			now.reported = true
			stamps[path] = now
			found.pending = append(found.pending, pendingFrame{area: area, name: name, mtime: now.mtime})
			continue
		}
		// Already delivered under this name; never pack it a second time.
		if _, err := os.Stat(filepath.Join(ac.config.ProcessedDirectory, name)); err == nil {
			continue
		}

		named, hasNamed := parseFrameTime(name)
		found.ready[area] = append(found.ready[area], candidate{
			path:     path,
			name:     name,
			obs:      obs,
			hasObs:   hasObs,
			named:    named,
			hasNamed: hasNamed,
			size:     now.size,
			mtime:    now.mtime,
		})
	}
	return found
}

// sortByArchiveName matches Python _sortByArchiveName method
func (ac *AstroCam) sortByArchiveName(archiveFileName string) string {
	filename := filepath.Base(archiveFileName)

	// Remove the archive extension, whichever of the two it is. A backlog
	// packed as .rar must still sort correctly after a run switches to zip.
	if ext := strings.ToLower(filepath.Ext(filename)); ext == ".zip" || ext == ".rar" {
		filename = filename[:len(filename)-len(ext)]
	}

	var pos int

	// Remove postfix if present
	if ac.config.Postfix != "" {
		pos = strings.LastIndex(filename, ac.config.Postfix)
		if pos != -1 {
			filename = filename[:pos]
		}
	}

	// Extract date and time parts
	pos = strings.Index(filename, "_")
	if pos == -1 {
		return filename
	}
	strDate := filename[:pos]

	pos = strings.LastIndex(filename, "_")
	if pos == -1 {
		return strDate
	}
	strTime := filename[pos:]

	// Create sort criteria
	criteria := strings.ReplaceAll(strings.ReplaceAll(strDate+strTime, "-", ""), "_", "")
	return criteria
}

// getArchiveFiles lists the archives waiting in the temp directory, oldest
// first. Both archive formats are listed whatever the current mode is,
// because a backlog packed as .rar still has to be delivered after a run
// switches to zip and the server accepts either. A listing is used rather
// than a glob so that a bracket in the install path cannot silently match
// nothing. Archives the server keeps refusing are tried last, so one bad
// archive cannot hold up the rest of the queue.
func (ac *AstroCam) getArchiveFiles() ([]string, error) {
	entries, err := os.ReadDir(ac.tempDirectory)
	if err != nil {
		return nil, fmt.Errorf("error scanning for archive files: %w", err)
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext == ".zip" || ext == ".rar" {
			files = append(files, filepath.Join(ac.tempDirectory, entry.Name()))
		}
	}

	sort.Slice(files, func(i, j int) bool {
		if ri, rj := ac.rejects[files[i]], ac.rejects[files[j]]; ri != rj {
			return ri < rj
		}
		if ki, kj := ac.sortByArchiveName(files[i]), ac.sortByArchiveName(files[j]); ki != kj {
			return ki < kj
		}
		// Archives packed in the same second sort by name, so the queue is
		// listed in the same order on every cycle.
		return files[i] < files[j]
	})

	return files, nil
}

// refreshQueue lists the archives waiting in the temp directory and records
// how many there are and how big, for the status lines and the banner.
//
// It also forgets the refusals of archives that are no longer there. An
// operator who moves a refused archive away has dealt with it, and the warning
// about it must not outlive the archive.
func (ac *AstroCam) refreshQueue() ([]string, error) {
	archives, err := ac.getArchiveFiles()
	if err != nil {
		ac.queueSummary, ac.queuedCount, ac.queuedBytes = "", 0, 0
		return nil, err
	}

	var total int64
	oldest := ""
	queued := make(map[string]bool, len(archives))
	for _, a := range archives {
		queued[a] = true
		if info, err := os.Stat(a); err == nil {
			total += info.Size()
		}
		if oldest == "" || ac.sortByArchiveName(a) < ac.sortByArchiveName(oldest) {
			oldest = a
		}
	}
	ac.queuedCount, ac.queuedBytes = len(archives), total
	if len(archives) == 0 {
		ac.queueSummary = ""
	} else {
		ac.queueSummary = fmt.Sprintf("%s waiting to be delivered (%s), oldest is %s",
			plural(len(archives), "archive", "archives"), humanBytes(total), filepath.Base(oldest))
	}

	for a := range ac.rejects {
		if !queued[a] {
			delete(ac.rejects, a)
			delete(ac.refusedAfter, a)
		}
	}
	if ac.hasCondition(conditionArchive) && !ac.anyArchiveRefusedOften() {
		ac.clearCondition(conditionArchive, "no refused archive is waiting any more")
	}
	return archives, nil
}

// anyArchiveRefusedOften reports whether the server has turned some waiting
// archive away often enough for it to count as a standing problem.
func (ac *AstroCam) anyArchiveRefusedOften() bool {
	for _, n := range ac.rejects {
		if n >= rejectsBeforeWarning {
			return true
		}
	}
	return false
}

// plural renders a count with the right form of its noun: "1 archive",
// "3 archives".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// moveImages moves the frames of a packed archive into the processed
// directory. A frame left in the camera directory can be packed again on a
// later cycle, and the same images are then delivered twice.
//
// A frame is never deleted just because a file of the same name already exists
// in the processed directory: that would destroy an image which was never
// delivered under that name. Such a file is left alone for a human to sort out.
//
// It returns the frames that did not get there, together with the last error.
// Such a frame is usually still in the camera directory, but not always: one
// that another program took away fails too, and so does one whose rename went
// through but reported an error. Whether the archive may then be thrown away
// is for the caller to decide (see allOfferedAgain).
func (ac *AstroCam) moveImages(files []string) ([]string, error) {
	const maxRetries = 2

	var failed []string
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		failed = nil

		for _, file := range files {
			basename := filepath.Base(file)
			targetPath := filepath.Join(ac.config.ProcessedDirectory, basename)

			if _, err := os.Stat(targetPath); err == nil {
				fmt.Printf("Error: %s already exists in the processed directory; leaving the camera copy in place\n",
					basename)
				lastErr = fmt.Errorf("%s already exists in the processed directory", basename)
				failed = append(failed, file)
				continue
			}

			err := renameImage(file, targetPath)
			if errors.Is(err, errCrossDevice) {
				// The processed folder is on another drive, where a rename
				// cannot go. That used to fail every move, throw away every
				// archive and upload nothing at all.
				var copied bool
				copied, err = moveAcrossDrives(file, targetPath)
				if copied {
					// The image is safely in the processed folder; its name
					// there keeps a leftover original from being packed again.
					if err != nil {
						fmt.Printf("Warning: %s was copied into the processed directory, but the original could not be deleted (%v). It is not packed again; delete it by hand.\n",
							basename, err)
					}
					continue
				}
			}
			if err != nil {
				fmt.Printf("Error: Cannot move file %s (attempt %d/%d): %v\n",
					basename, attempt, maxRetries, err)
				lastErr = err
				failed = append(failed, file)
			}
		}

		if len(failed) == 0 {
			return nil, nil
		}
		if attempt < maxRetries {
			fmt.Printf("Waiting %v before retry...\n", moveRetryDelay)
			time.Sleep(moveRetryDelay)
			files = failed
		}
	}

	if ac.testMode {
		fmt.Printf("FATAL ERROR (Test Mode): Failed to move %d files after %d attempts\n", len(failed), maxRetries)
		os.Exit(1)
	}
	return failed, lastErr
}

// moveRetryDelay is how long moveImages waits before it tries a failed move
// once more: long enough for a program that briefly held the file to let go.
var moveRetryDelay = 3 * time.Second

// renameImage moves an image into the processed directory. It is a variable
// so that the tests can make it fail the way a rename to another drive does.
var renameImage = os.Rename

// moveAcrossDrives moves an image to another drive or filesystem, where a
// rename cannot take it: it copies the file next to its destination under a
// temporary name, makes sure every byte is on the disk, renames the copy into
// place and only then deletes the original. Until the rename the original is
// untouched and no partial file is left behind.
//
// copied reports whether the destination now holds a complete copy. It can be
// true together with an error, when the original could not be deleted.
func moveAcrossDrives(src, dst string) (copied bool, err error) {
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	info, err := in.Stat()
	if err != nil {
		in.Close()
		return false, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*"+partialCopySuffix)
	if err != nil {
		in.Close()
		return false, err
	}
	n, err := io.Copy(tmp, in)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	// The original must be closed before it can be deleted on Windows.
	in.Close()
	if err == nil && n != info.Size() {
		err = fmt.Errorf("copied %d of %d bytes", n, info.Size())
	}
	if err != nil {
		os.Remove(tmp.Name())
		return false, fmt.Errorf("could not copy %s to %s: %w", filepath.Base(src), filepath.Dir(dst), err)
	}
	// Keep the permissions and the time the image was taken, as a move would:
	// the temporary file was created readable by this user alone.
	os.Chmod(tmp.Name(), info.Mode().Perm())
	os.Chtimes(tmp.Name(), info.ModTime(), info.ModTime())
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return false, err
	}
	// The renamed copy should survive a power cut before the original goes.
	// Best effort: some filesystems cannot sync a folder, and on Windows this
	// does nothing; finishPartialCopies at the next start covers the rest.
	syncDir(filepath.Dir(dst))
	if err := os.Remove(src); err != nil {
		return true, err
	}
	return true, nil
}

// partialCopySuffix ends the name of a copy that moveAcrossDrives has not
// renamed into place yet. One left behind by a crash is dealt with at the next
// start, by finishPartialCopies.
const partialCopySuffix = ".partial"

// partialCopyName matches the names moveAcrossDrives gives its copies: a dot,
// the image name, a dot and the random digits of os.CreateTemp, the suffix.
var partialCopyName = regexp.MustCompile(`^\.(.+)\.[0-9]+` + regexp.QuoteMeta(partialCopySuffix) + `$`)

// finishPartialCopies deals with the copies that a crash in the middle of
// moveAcrossDrives left in the processed directory. When the image is in place
// already, or its original is still in a camera folder, the leftover copy is
// not needed and is deleted. Otherwise it may be the only copy left - the
// original can be gone from the camera folder when a power cut came before the
// rename into place reached the disk: a complete one is renamed into place,
// which finishes the move, and an incomplete one is left for a person to look
// at. A possible only copy is never deleted.
func (ac *AstroCam) finishPartialCopies() {
	dir := ac.config.ProcessedDirectory
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var cameraDirs []string
	for _, e := range entries {
		m := partialCopyName.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil || !fitsNamePattern.MatchString(m[1]) {
			continue
		}
		image := m[1]
		leftover := filepath.Join(dir, e.Name())
		final := filepath.Join(dir, image)

		_, finalErr := os.Stat(final)
		inCamera := false
		if finalErr != nil {
			if cameraDirs == nil {
				cameraDirs = ac.scanDirs()
			}
			for _, d := range cameraDirs {
				if _, err := os.Stat(filepath.Join(d, image)); err == nil {
					inCamera = true
					break
				}
			}
		}
		if finalErr == nil || inCamera {
			if os.Remove(leftover) == nil {
				fmt.Printf("Removed an unfinished copy of %s left by an earlier run\n", image)
			}
			continue
		}
		if info, err := e.Info(); err == nil {
			if _, _, complete := isCompleteFITS(leftover, info.Size()); complete && os.Rename(leftover, final) == nil {
				fmt.Printf("Finished moving %s into the processed directory (the move was interrupted)\n", image)
				continue
			}
		}
		fmt.Printf("Warning: %s may be the only copy of %s, but it is not complete; it is left for you to check\n",
			leftover, image)
	}
}

// createZipArchive creates ZIP archive using Go's built-in zip library
func (ac *AstroCam) createZipArchive(archiveFileName string, files []string) error {
	outFile, err := os.Create(archiveFileName)
	if err != nil {
		return fmt.Errorf("failed to create archive file: %w", err)
	}
	defer outFile.Close()

	zipWriter := zip.NewWriter(outFile)
	defer zipWriter.Close()

	for _, filename := range files {
		if err := ac.addFileToZip(zipWriter, filename); err != nil {
			return fmt.Errorf("failed to add file %s to archive: %w", filename, err)
		}
	}

	return nil
}

// addFileToZip adds a single file to the zip archive
func (ac *AstroCam) addFileToZip(zipWriter *zip.Writer, filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}

	header.Name = filepath.Base(filename)

	// Set compression method based on configuration
	if ac.zipCompressed {
		header.Method = zip.Deflate
	} else {
		header.Method = zip.Store // No compression
	}

	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(writer, file)
	return err
}

// testZipArchive tests ZIP archive integrity
func (ac *AstroCam) testZipArchive(archiveFileName string) error {
	reader, err := zip.OpenReader(archiveFileName)
	if err != nil {
		return fmt.Errorf("failed to open ZIP file for testing: %w", err)
	}
	defer reader.Close()

	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			return fmt.Errorf("failed to open file %s in archive: %w", file.Name, err)
		}

		// Read every byte, so archive/zip verifies the checksum. Reading only
		// the first bytes accepts a member that is corrupt further in, which is
		// exactly what a half-written archive looks like.
		_, err = io.Copy(io.Discard, rc)
		closeErr := rc.Close()

		if err != nil {
			return fmt.Errorf("failed to read file %s in archive: %w", file.Name, err)
		}
		if closeErr != nil {
			return fmt.Errorf("file %s in archive failed its checksum: %w", file.Name, closeErr)
		}
	}

	return nil
}

// createRARArchive creates RAR archive using external rar command
func (ac *AstroCam) createRARArchive(archiveFileName string, files []string) error {
	args := []string{"a", "-ep1", archiveFileName}
	args = append(args, files...)

	cmd := exec.Command(ac.rarPath, args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rar creation failed: %w, output: %s", err, string(output))
	}

	return nil
}

// testRARArchive tests RAR archive integrity
func (ac *AstroCam) testRARArchive(archiveFileName string) error {
	cmd := exec.Command(ac.rarPath, "t", archiveFileName)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rar test failed: %w, output: %s", err, string(output))
	}

	return nil
}

// createArchive creates archive using available method (RAR or ZIP)
func (ac *AstroCam) createArchive(archiveFileName string, files []string) error {
	if ac.useRAR {
		return ac.createRARArchive(archiveFileName, files)
	} else {
		return ac.createZipArchive(archiveFileName, files)
	}
}

// testArchive tests archive integrity using available method
func (ac *AstroCam) testArchive(archiveFileName string) error {
	// Test by the archive's own format, not by the mode this run happens to be
	// using, so a leftover .rar is not handed to the zip reader.
	if strings.EqualFold(filepath.Ext(archiveFileName), ".rar") {
		if ac.rarPath == "" {
			// A leftover .rar on a machine with no rar command cannot be
			// checked here. The server validates it anyway, so let it go
			// rather than condemn an archive that may be perfectly good.
			return nil
		}
		return ac.testRARArchive(archiveFileName)
	}
	return ac.testZipArchive(archiveFileName)
}

// context returns the context that is cancelled when the operator asks the
// program to stop. An AstroCam built by hand in a test has none, and is never
// stopped.
func (ac *AstroCam) context() context.Context {
	if ac.ctx == nil {
		return context.Background()
	}
	return ac.ctx
}

// stopping reports whether the operator has asked the program to stop.
func (ac *AstroCam) stopping() bool {
	return ac.context().Err() != nil
}

// sleep waits for d, or until the operator asks the program to stop, whichever
// comes first. It reports whether the whole wait was served.
//
// Every wait of more than a moment goes through here. A plain time.Sleep is
// deaf to Ctrl+C, and the program used to sit out a two minute pause between
// uploads before it noticed that it had been told to stop.
func (ac *AstroCam) sleep(d time.Duration) bool {
	if d <= 0 {
		return !ac.stopping()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ac.context().Done():
		return false
	}
}

// cycleInterval is the time between two scans of the camera directories.
func (ac *AstroCam) cycleInterval() time.Duration {
	interval := ac.config.Interval
	if interval < MIN_INTERVAL {
		interval = MIN_INTERVAL
	}
	return time.Duration(interval) * time.Second
}

// nextUploadAt returns the earliest moment after now at which the next upload
// may start: uploads are spaced uploadThrottleDelay apart, counted from the
// start of the previous one. The zero time means "right now".
func (ac *AstroCam) nextUploadAt(now time.Time) time.Time {
	if ac.lastUploadTime.IsZero() {
		return time.Time{}
	}
	next := ac.lastUploadTime.Add(uploadThrottleDelay)
	if !next.After(now) {
		return time.Time{}
	}
	return next
}

// waitForUploadSlot reports whether an upload may start now.
//
// While the next upload is still some way off it returns false at once, and
// the upload is taken up again on a later cycle. The program used to sleep
// through that gap instead, and for up to two minutes it neither looked for new
// images nor printed anything, which is no way to treat images that are waiting
// to be packed. Only a last stretch of less than half a scan interval is waited
// out here: short enough that this cycle is over before the next one is due,
// and long enough that an upload is never put off by a whole cycle for the
// sake of a few seconds.
//
// With stay set the whole gap is waited out here whatever its length. That is
// for the one case where coming back later would lose the thread: an archive
// about to be sent again to the fallback server.
func (ac *AstroCam) waitForUploadSlot(stay bool) bool {
	next := ac.nextUploadAt(time.Now())
	if next.IsZero() {
		return !ac.stopping()
	}
	wait := time.Until(next)
	if wait > ac.cycleInterval()/2 {
		if !stay {
			return false
		}
		fmt.Printf("Next upload at %s (uploads are spaced %s apart).\n",
			next.Format("15:04:05"), formatPauseDuration(uploadThrottleDelay))
	}
	return ac.sleep(wait)
}

// neverRedirect stops the HTTP client from following a redirect.
//
// A redirect must never be followed with an upload. Go repeats a redirected
// POST as a GET with no body; the server's status check then answers that GET
// with UNMW_STATUS:OK, and the archive would be deleted here as delivered
// although the server never saw it. A redirect is reported as what it is
// instead: SAI_SERVER points at the wrong address.
func neverRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

// serverAnswer is what a server said when it was asked for its status.
type serverAnswer struct {
	// status is one of: "ok", "warning", "error" (the upload script itself
	// says it cannot take uploads), "http" (the web server in front of it
	// answered with an error that says the same), "unreachable" (no answer at
	// all - the only case that fails over to the fallback server), "unknown"
	// (an answer that says nothing either way) or "stopped" (the operator is
	// stopping the program, so there is no answer to act on).
	status   string
	message  string // the server's own words, or the network error
	code     int    // HTTP status; 0 when no answer arrived
	location string // where a redirect points
}

// checkServerDiskSpace sends a GET preflight request to the given server to
// check its disk space and load, and reports what came back.
func (ac *AstroCam) checkServerDiskSpace(server string) serverAnswer {
	req, err := http.NewRequestWithContext(ac.context(), "GET", server, nil)
	if err != nil {
		// Not an address at all. No request can be sent to it, which for every
		// practical purpose is a server that cannot be reached - and it is
		// reported that way, with SAI_SERVER named as a thing to check, rather
		// than passed on to fail again as an upload.
		return serverAnswer{status: "unreachable", message: fmt.Sprintf("not a usable address: %v", err)}
	}

	if ac.hasCredentials() {
		req.SetBasicAuth(ac.config.Username, ac.config.Password)
	}

	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: neverRedirect}
	resp, err := client.Do(req)
	if err != nil {
		if ac.stopping() {
			return serverAnswer{status: "stopped"}
		}
		// Transport-level failure -> the server is unreachable (as opposed to a
		// server that answers with an error). This is what triggers fail-over.
		// Only the network error itself is kept: the address it is wrapped in
		// is printed by the caller anyway.
		var wrapped *url.Error
		if errors.As(err, &wrapped) {
			err = wrapped.Err
		}
		return serverAnswer{status: "unreachable", message: err.Error()}
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	body := string(bodyBytes)
	answer := serverAnswer{
		status:   "unknown",
		message:  body,
		code:     resp.StatusCode,
		location: resp.Header.Get("Location"),
	}

	switch {
	case strings.Contains(body, "UNMW_STATUS:ERROR"):
		answer.status = "error"
	case strings.Contains(body, "UNMW_STATUS:WARNING"):
		answer.status = "warning"
	case strings.Contains(body, "UNMW_STATUS:OK"):
		answer.status = "ok"
	case resp.StatusCode == 507:
		// No UNMW_STATUS marker, but the status code alone says it.
		answer.status = "error"
		answer.message = fmt.Sprintf("server is out of disk space (HTTP 507): %s", body)
	default:
		// No UNMW_STATUS marker: the upload script did not write this answer.
		// When the status code says the request never reached it, there is no
		// point in sending an archive after it. Anything else may be an old
		// server that knows nothing of status checks, so the upload is tried.
		if _, scope := classifyHTTPStatus(resp.StatusCode); scope != scopeUnclear {
			answer.status = "http"
		}
	}
	return answer
}

// archivePaths returns where to build an archive and where to move it once it
// is finished. The name carries the time the archive was packed and has a
// resolution of one second, so a counter is appended when that name is already
// taken; without it a zip would silently truncate an archive whose frames have
// already left the camera directory, and rar would append to it.
func (ac *AstroCam) archivePaths(area string) (incoming string, final string, err error) {
	incomingDir := filepath.Join(ac.tempDirectory, "incoming")
	if err := os.MkdirAll(incomingDir, 0755); err != nil {
		return "", "", fmt.Errorf("could not create %s: %w", incomingDir, err)
	}

	now := time.Now()
	stem := fmt.Sprintf("%s_%s%s_%s%s",
		now.Format(dateFolderLayout), ac.config.Prefix, area, now.Format("150405"), ac.config.Postfix)

	for i := 0; i < 100; i++ {
		name := stem + ac.archiveExt
		if i > 0 {
			name = fmt.Sprintf("%s_%d%s", stem, i, ac.archiveExt)
		}
		final = filepath.Join(ac.tempDirectory, name)
		incoming = filepath.Join(incomingDir, name)

		_, finalErr := os.Stat(final)
		_, incomingErr := os.Stat(incoming)
		if os.IsNotExist(finalErr) && os.IsNotExist(incomingErr) {
			return incoming, final, nil
		}
	}
	return "", "", fmt.Errorf("could not find an unused archive name for area %s", area)
}

// packVisit builds one archive from the frames of a single visit to one field
// and then moves those frames into the processed directory.
//
// The archive is built inside temp/incoming and is renamed into the temp
// directory only after it has passed its integrity test, so a crash or a full
// disk can never leave a half-written file where the upload loop would find
// it. The frames are moved only after that rename has succeeded, so a failure
// anywhere earlier leaves them in place to be packed again.
func (ac *AstroCam) packVisit(area string, visit []candidate) (string, error) {
	originalDir, err := os.Getwd()
	if err != nil {
		return ERROR, fmt.Errorf("could not determine the current directory: %w", err)
	}
	defer os.Chdir(originalDir)

	// The two checks that keep a short or malformed set out of an archive. The
	// count is checked here, against the frames actually selected, rather than
	// against a directory listing taken earlier that may since have changed.
	if len(visit) != ac.config.Count {
		return ERROR, fmt.Errorf("internal error: %d frames selected for area %s, want %d",
			len(visit), area, ac.config.Count)
	}
	if !distinctNames(visit) {
		return ERROR, fmt.Errorf("area %s has two frames with the same file name; not packing them", area)
	}

	names := make([]string, len(visit))
	paths := make([]string, len(visit))
	for i, c := range visit {
		fmt.Printf("Processing file: %s\n", c.path)
		names[i] = c.name
		paths[i] = c.path
	}

	// Frames of one visit normally share a folder, and then they are archived
	// from inside it under their base names, exactly as before. A visit whose
	// frames straddle a folder boundary is still one visit, so in that case the
	// files are archived by absolute path instead; both archivers store the
	// base name only, and verifyArchiveMembers below checks that they did.
	dir := filepath.Dir(paths[0])
	sameDir := true
	for _, path := range paths[1:] {
		if filepath.Dir(path) != dir {
			sameDir = false
			break
		}
	}

	members := names
	if !sameDir {
		members = make([]string, len(paths))
		for i, path := range paths {
			abs, err := filepath.Abs(path)
			if err != nil {
				return ERROR, fmt.Errorf("could not resolve %s: %w", path, err)
			}
			members[i] = abs
		}
		fmt.Printf("Area %s: this visit spans %d folders, packing it by absolute path\n", area, len(visit))
	}

	incomingPath, finalPath, err := ac.archivePaths(area)
	if err != nil {
		return ERROR, err
	}

	var archiveTypeStr string
	if ac.useRAR {
		archiveTypeStr = "RAR"
	} else if ac.zipCompressed {
		archiveTypeStr = "ZIP"
	} else {
		archiveTypeStr = "ZIP (uncompressed)"
	}
	fmt.Printf("Creating %s archive: %s (area %s, %d frames from %s)\n",
		archiveTypeStr, filepath.Base(finalPath), area, len(visit), dir)

	// Archive the frames under their base names, which is what the server
	// expects to find inside the archive.
	if sameDir {
		if err := os.Chdir(dir); err != nil {
			if ac.testMode {
				fmt.Printf("FATAL ERROR (Test Mode): Cannot change to image directory %s: %v\n", dir, err)
				os.Exit(1)
			}
			return ERROR, fmt.Errorf("could not change to image directory %s: %w", dir, err)
		}
	}

	if err := ac.createArchive(incomingPath, members); err != nil {
		os.Chdir(originalDir)
		os.Remove(incomingPath)
		if ac.testMode {
			fmt.Printf("FATAL ERROR (Test Mode): Archive creation failed: %v\n", err)
			os.Exit(1)
		}
		return ERROR, fmt.Errorf("failed to create archive: %w", err)
	}

	if err := ac.testArchive(incomingPath); err != nil {
		os.Chdir(originalDir)
		os.Remove(incomingPath)
		fmt.Printf("Warning: Archive integrity test failed: %v\n", err)
		if ac.testMode {
			fmt.Printf("FATAL ERROR (Test Mode): Archive integrity test failed\n")
			os.Exit(1)
		}
		return ERROR, err
	}

	if err := os.Chdir(originalDir); err != nil {
		if ac.testMode {
			fmt.Printf("FATAL ERROR (Test Mode): Cannot change back to original directory: %v\n", err)
			os.Exit(1)
		}
		return ERROR, fmt.Errorf("could not change back to original directory: %w", err)
	}

	// The server rejects an archive that carries a directory part or the wrong
	// number of frames, and it does so after this program has already deleted
	// its own copy, so check here instead of finding out afterwards.
	if err := ac.verifyArchiveMembers(incomingPath, names); err != nil {
		os.Remove(incomingPath)
		fmt.Printf("Warning: %v\n", err)
		if ac.testMode {
			fmt.Printf("FATAL ERROR (Test Mode): archive contents are wrong\n")
			os.Exit(1)
		}
		return ERROR, err
	}

	// Only a complete, tested archive becomes visible to the upload loop.
	if err := os.Rename(incomingPath, finalPath); err != nil {
		os.Remove(incomingPath)
		return ERROR, fmt.Errorf("could not move the finished archive into %s: %w", ac.tempDirectory, err)
	}

	// From here on images leave the camera folder, so the archive should be
	// on the disk first: after a power cut it may be their only copy. This is
	// best effort and never a reason to hold back the archive.
	if err := syncFile(finalPath); err != nil {
		fmt.Printf("Warning: could not make sure archive %s is on the disk: %v\n", filepath.Base(finalPath), err)
	}
	syncDir(ac.tempDirectory)

	notMoved, err := ac.moveImages(paths)
	if len(notMoved) == len(paths) && ac.allOfferedAgain(paths) {
		// Every frame is still in the camera directory, and will be offered
		// again by the next scan, so this archive is not the only copy of
		// anything. Throw it away rather than deliver the same visit again on
		// every cycle from now on.
		os.Remove(finalPath)
		return ERROR, fmt.Errorf("%w (%v), so archive %s was thrown away. They stay in the camera folder and are tried again on the next cycle",
			errImagesNotMoved, err, filepath.Base(finalPath))
	}
	var stranded, vanished, doubled []string
	for _, path := range notMoved {
		_, inCamera := os.Stat(path)
		_, inProcessed := os.Stat(filepath.Join(ac.config.ProcessedDirectory, filepath.Base(path)))
		switch {
		case inCamera != nil:
			vanished = append(vanished, path)
		case inProcessed == nil:
			doubled = append(doubled, path)
		default:
			stranded = append(stranded, path)
		}
	}
	if len(vanished) > 0 {
		fmt.Printf("Warning: %s of archive %s disappeared from the camera folder before it could be moved; the archive is kept and uploaded.\n",
			plural(len(vanished), "image", "images"), filepath.Base(finalPath))
	}
	if len(doubled) > 0 {
		fmt.Printf("Warning: %s of archive %s reached the processed directory but also stayed in the camera folder; they are not packed again. Delete them from the camera folder by hand:\n",
			plural(len(doubled), "image", "images"), filepath.Base(finalPath))
		for _, path := range doubled {
			fmt.Printf("  - %s\n", path)
		}
	}
	if len(stranded) > 0 {
		fmt.Printf("Warning: archive %s will be uploaded, but %s could not be moved into the processed directory (%v):\n",
			filepath.Base(finalPath), plural(len(stranded), "of its images", "of its images"), err)
		if ac.stranded == nil {
			ac.stranded = make(map[string]bool)
		}
		for _, path := range stranded {
			fmt.Printf("  - %s\n", path)
			ac.stranded[path] = true
		}
		fmt.Printf("  Move them there by hand, or they may be sent a second time or reported as an incomplete visit.\n")
	}

	return finalPath, nil
}

// allOfferedAgain reports whether every one of these images will be found by
// the next scan: still in its camera folder, and not under its name in the
// processed directory (an image whose name is there is skipped for good).
// Only then may an archive of them be thrown away.
func (ac *AstroCam) allOfferedAgain(paths []string) bool {
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			return false
		}
		if _, err := os.Stat(filepath.Join(ac.config.ProcessedDirectory, filepath.Base(path))); err == nil {
			return false
		}
	}
	return true
}

// syncFile makes sure that everything written to a file is on the disk. The
// file is opened for writing because Windows flushes only through a handle
// that may write.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// errImagesNotMoved marks a set of images that was packed but could not be
// moved into the processed directory at all. It gets its own standing problem,
// because its cause and its cure differ from those of an archive that cannot
// be built.
var errImagesNotMoved = errors.New("none of the images could be moved into the processed directory")

// verifyArchiveMembers checks that the archive holds exactly the expected
// frames, each stored under its bare base name. The server requires bare names
// and refuses an archive with too few frames, so an archive that fails this
// must never leave the machine.
func (ac *AstroCam) verifyArchiveMembers(archivePath string, want []string) error {
	got, err := ac.archiveMemberNames(archivePath)
	if err != nil {
		return fmt.Errorf("could not list the contents of %s: %w", filepath.Base(archivePath), err)
	}
	if len(got) != len(want) {
		return fmt.Errorf("%s holds %d file(s), expected %d", filepath.Base(archivePath), len(got), len(want))
	}

	expected := make(map[string]bool, len(want))
	for _, name := range want {
		expected[name] = true
	}
	for _, name := range got {
		if strings.ContainsAny(name, "/\\") {
			return fmt.Errorf("%s stores %q with a directory part", filepath.Base(archivePath), name)
		}
		if !expected[name] {
			return fmt.Errorf("%s holds an unexpected file %q", filepath.Base(archivePath), name)
		}
		delete(expected, name)
	}
	return nil
}

// archiveMemberNames lists the names stored inside an archive.
func (ac *AstroCam) archiveMemberNames(archivePath string) ([]string, error) {
	if strings.EqualFold(filepath.Ext(archivePath), ".rar") {
		out, err := exec.Command(ac.rarPath, "lb", archivePath).Output()
		if err != nil {
			return nil, err
		}
		var names []string
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				names = append(names, line)
			}
		}
		return names, nil
	}

	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	var names []string
	for _, f := range reader.File {
		names = append(names, f.Name)
	}
	return names, nil
}

// hasCredentials checks if username and password are provided
func (ac *AstroCam) hasCredentials() bool {
	return ac.config.Username != "" && ac.config.Password != ""
}

// uploadResponseIndicatesSuccess reports whether a 2xx upload response body
// actually confirms success. upload.py returns HTTP 200 even for several POST
// failures (it only sets a non-2xx status for out-of-disk-space), so success is
// recognized by a positive marker: the redirect page printed on success
// ("Upload successful") or an explicit UNMW_STATUS:OK from a newer server. An
// explicit UNMW_STATUS:ERROR always counts as failure.
func uploadResponseIndicatesSuccess(body string) bool {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "unmw_status:error") {
		return false
	}
	return strings.Contains(lower, "upload successful") ||
		strings.Contains(lower, "unmw_status:ok")
}

// humanBytes renders a byte count the way an operator reads it.
func humanBytes(n int64) string {
	switch {
	case n >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f kB", float64(n)/1024)
	}
	return fmt.Sprintf("%d bytes", n)
}

// uploadProgress counts how much of the archive has been handed to the
// network. It exists so that the three states an upload can be in are told
// apart in the log: moving slowly, not moving at all, or fully sent and
// waiting on the server.
type uploadProgress struct {
	total    int64
	sent     int64 // atomic
	bodyDone int32 // atomic; 1 once the whole body has been handed over
	doneAt   int64 // atomic; UnixNano when that happened
}

// progressReader feeds the request body and counts what goes out.
type progressReader struct {
	inner *bytes.Reader
	prog  *uploadProgress
}

func (r *progressReader) Read(b []byte) (int, error) {
	n, err := r.inner.Read(b)
	if n > 0 {
		atomic.AddInt64(&r.prog.sent, int64(n))
	}
	if err == io.EOF && atomic.CompareAndSwapInt32(&r.prog.bodyDone, 0, 1) {
		atomic.StoreInt64(&r.prog.doneAt, time.Now().UnixNano())
	}
	return n, err
}

// reportUploadProgress prints one line every uploadProgressInterval until the
// returned stop function is called. Because it runs on its own timer it also
// prints while the transfer is stuck, which is exactly the case that is
// otherwise indistinguishable from a hung program.
func reportUploadProgress(prog *uploadProgress, server string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(uploadProgressInterval)
		defer ticker.Stop()

		started := time.Now()
		lastSent := int64(0)
		lastTime := started
		waitingReported := false

		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				sent := atomic.LoadInt64(&prog.sent)

				if atomic.LoadInt32(&prog.bodyDone) == 1 {
					// The archive is on its way out; the server now has to
					// unpack, validate and answer.
					waited := now.Sub(time.Unix(0, atomic.LoadInt64(&prog.doneAt)))
					if !waitingReported {
						fmt.Printf("  upload: all %s sent in %v, now WAITING FOR THE SERVER to reply\n",
							humanBytes(prog.total), now.Sub(started).Round(time.Second))
						waitingReported = true
					} else {
						fmt.Printf("  upload: still waiting for the server to reply (%v so far)\n",
							waited.Round(time.Second))
					}
					lastSent, lastTime = sent, now
					continue
				}

				delta := sent - lastSent
				elapsed := now.Sub(lastTime).Seconds()
				if elapsed <= 0 {
					elapsed = 1
				}
				rate := float64(delta) / elapsed

				if delta == 0 {
					// A slow link delivers in bursts, so report the average
					// since the start as well: that tells a bursty transfer
					// that is still progressing from a dead one.
					overall := float64(sent) / now.Sub(started).Seconds()
					fmt.Printf("  upload: NOTHING SENT in the last %v - %s of %s so far (%s/s on average over %v)\n",
						now.Sub(lastTime).Round(time.Second), humanBytes(sent), humanBytes(prog.total),
						humanBytes(int64(overall)), now.Sub(started).Round(time.Second))
				} else {
					percent := 100.0 * float64(sent) / float64(prog.total)
					remaining := "unknown"
					if rate > 0 {
						remaining = time.Duration(float64(prog.total-sent) / rate * float64(time.Second)).Round(time.Second).String()
					}
					fmt.Printf("  upload: %s of %s (%.0f%%) at %s/s, about %s left\n",
						humanBytes(sent), humanBytes(prog.total), percent,
						humanBytes(int64(rate)), remaining)
				}
				lastSent, lastTime = sent, now
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

// httpStatusError is an upload that the server answered with an HTTP status
// other than success. It keeps the status code apart from the text, so the
// cause can be told from the code rather than guessed from the wording.
type httpStatusError struct {
	code     int    // 401
	status   string // "401 Unauthorized"
	body     string // what the server sent along
	location string // where a redirect points
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("server returned status %d: %s; %s", e.code, e.status, e.body)
}

// uploadFile matches FileUploader functionality with proper resource management.
// The caller is responsible for the pause between uploads (waitForUploadSlot).
func (ac *AstroCam) uploadFile(filePath, server string) error {
	fmt.Printf("Uploading %s to server: %s\n", filepath.Base(filePath), server)

	// Update last upload time before attempting upload
	ac.lastUploadTime = time.Now()

	// Open file with proper resource management
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("%w: %v", errLocalFailure, err)
	}
	defer file.Close()

	// Create multipart form
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add file to form
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return fmt.Errorf("failed to create form file: %w", err)
	}

	_, err = io.Copy(part, file)
	if err != nil {
		return fmt.Errorf("%w: %v", errLocalFailure, err)
	}

	writer.Close()

	// Feed the body through a counter so the log can say how far the upload
	// has got. The length is set by hand because wrapping the buffer hides it
	// from net/http, and without a Content-Length the request would go out
	// chunked, which the CGI on the other end does not expect.
	prog := &uploadProgress{total: int64(body.Len())}
	reader := &progressReader{inner: bytes.NewReader(body.Bytes()), prog: prog}

	// Create HTTP request. It carries the context that is cancelled when the
	// operator stops the program, so Ctrl+C ends a transfer that is under way
	// instead of waiting up to uploadTimeout for it.
	req, err := http.NewRequestWithContext(ac.context(), "POST", server, reader)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.ContentLength = prog.total

	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Only set authentication if credentials are provided
	if ac.hasCredentials() {
		req.SetBasicAuth(ac.config.Username, ac.config.Password)
		fmt.Printf("Using authentication for upload\n")
	} else {
		fmt.Printf("Uploading without authentication (no credentials provided)\n")
	}

	fmt.Printf("Sending %s to %s (giving up after %v)\n",
		humanBytes(prog.total), server, uploadTimeout)

	// Send request. The timeout covers the whole exchange: connecting, handing
	// over the archive, the server's processing and the reply.
	client := &http.Client{Timeout: uploadTimeout, CheckRedirect: neverRedirect}
	started := time.Now()
	stopProgress := reportUploadProgress(prog, server)
	resp, err := client.Do(req)
	stopProgress()
	if err != nil {
		// The operator stopped the program in mid-transfer. The archive stays
		// where it is and goes out on the next start; this is not a failure.
		if ac.stopping() {
			return errStopping
		}

		sent := atomic.LoadInt64(&prog.sent)
		elapsed := time.Since(started)

		// A timeout is NOT an unreachable server: the connection was made and
		// the server answered the preflight moments ago. Saying which of the
		// three things ran out of time is the whole point of these messages.
		if os.IsTimeout(err) {
			if atomic.LoadInt32(&prog.bodyDone) == 1 {
				return fmt.Errorf("%w: the whole archive (%s) reached %s, but the server did not reply within %v. The server is REACHABLE and has the data; it is taking too long to answer",
					errUploadTimeout, humanBytes(prog.total), server, uploadTimeout)
			}
			if sent == 0 {
				return fmt.Errorf("%w: not one byte of the archive left this machine in %v to %s. The connection was accepted but nothing could be written - suspect a firewall or a dead link, NOT a missing server",
					errUploadTimeout, elapsed.Round(time.Second), server)
			}
			rate := float64(sent) / elapsed.Seconds()
			needed := time.Duration(float64(prog.total) / rate * float64(time.Second))
			return fmt.Errorf("%w: only %s of %s reached %s in %v (%s/s). The server is REACHABLE - the link is too slow: this archive needs about %v at that rate. Every retry starts again from the beginning, so it will keep failing until the link is faster",
				errUploadTimeout, humanBytes(sent), humanBytes(prog.total), server,
				elapsed.Round(time.Second), humanBytes(int64(rate)), needed.Round(time.Second))
		}

		// Anything else at the transport level means we never got a usable
		// connection: that is a genuinely unreachable server.
		return fmt.Errorf("%w: upload to %s failed after %v: %v",
			errServerUnreachable, server, elapsed.Round(time.Second), err)
	}
	defer resp.Body.Close()

	fmt.Printf("  upload: %s delivered and answered in %v\n",
		humanBytes(prog.total), time.Since(started).Round(time.Second))

	// Read response body to detect disk space warnings/errors
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bodyStr := string(bodyBytes)

	// Check response.
	//
	// A 2xx status alone does NOT mean the upload succeeded: upload.py returns
	// HTTP 200 with an HTML error body for several POST failures (high system
	// load, missing upload directory, archive validation failure, processing
	// error) -- it only sets a non-2xx status (507) for out-of-disk-space. So
	// treat the upload as successful ONLY when the body carries a positive
	// success marker; otherwise return an error so the caller keeps the local
	// archive for retry instead of deleting it.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if uploadResponseIndicatesSuccess(bodyStr) {
			if strings.Contains(bodyStr, "UNMW_STATUS:WARNING") {
				fmt.Printf("WARNING from server: %s\n", strings.TrimSpace(bodyStr))
			}
			fmt.Printf("Successfully uploaded: %s\n", filepath.Base(filePath))
			return nil
		}
		// 2xx but no success marker -> the server rejected or failed the upload.
		return fmt.Errorf("upload not confirmed by server (HTTP %d): %s",
			resp.StatusCode, strings.TrimSpace(bodyStr))
	}

	// Specific handling for 507 Insufficient Storage
	if resp.StatusCode == 507 {
		return fmt.Errorf("server out of disk space (status 507): %s", strings.TrimSpace(bodyStr))
	}

	// Include the response body so the caller can classify the cause (e.g. a
	// 503 "system load too high" -> short pause) from the server's message,
	// and the status code so a refused login or a wrong address is not taken
	// for a fault in the archive.
	uploadErr := &httpStatusError{
		code:     resp.StatusCode,
		status:   resp.Status,
		body:     strings.TrimSpace(bodyStr),
		location: resp.Header.Get("Location"),
	}
	if ac.testMode {
		fmt.Printf("FATAL ERROR (Test Mode): %v\n", uploadErr)
		os.Exit(1)
	}
	return uploadErr
}

// deleteFile matches Python deleteFile function
func (ac *AstroCam) deleteFile(filePath string) error {
	if err := os.Remove(filePath); err != nil {
		fmt.Printf("Error: Cannot delete file %s: %v\n", filepath.Base(filePath), err)
		return fmt.Errorf(ERROR)
	}
	return nil
}

// formatPauseDuration renders a pause length in friendly units
// ("10 minutes", "1 hour") for log messages.
func formatPauseDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		h := int(d / time.Hour)
		if h == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", h)
	}
	if d >= time.Minute && d%time.Minute == 0 {
		m := int(d / time.Minute)
		if m == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", m)
	}
	return d.String()
}

// formatElapsedDuration renders an arbitrary age as "3h 07m", "12m 30s" or
// "45s" - short enough for a status line, precise enough to tell at a glance
// whether a problem started a minute or a night ago.
func formatElapsedDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh %02dm", int(d/time.Hour), int(d/time.Minute)%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm %02ds", int(d/time.Minute), int(d/time.Second)%60)
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

// The kinds of standing problem. One condition is kept per kind, so a server
// that is down and a camera directory that has gone missing are both visible at
// the same time instead of overwriting each other.
const (
	conditionDelivery = "delivery" // archives cannot be handed over to the server
	conditionInput    = "input"    // images cannot be read from the camera directories
	conditionPacking  = "packing"  // complete sets of images cannot be packed on this computer
	conditionTransfer = "transfer" // uploads run out of time before the server has answered
	conditionArchive  = "archive"  // one particular archive is being refused over and over
)

// How many times the server has to refuse the same archive before it is called
// out as a standing problem. Two refusals can be a hiccup; three is a pattern.
const rejectsBeforeWarning = 3

// A long banner every 15 seconds would bury everything else, and a message
// printed only once is invisible to an operator who walks up an hour later. So
// the full banner is repeated every conditionFullBannerEveryCycles cycles, and
// on every cycle the status lines that end it name each problem in one line -
// whatever the terminal is showing, the bottom of the screen says what is
// wrong.
const conditionFullBannerEveryCycles = 20

// setCondition records, or refreshes, a standing problem. Re-setting the same
// reason keeps the original start time, which is what lets the banner say how
// long the observatory has been unable to deliver data.
func (ac *AstroCam) setCondition(kind, severity, reason, detail, action string, until time.Time) {
	if ac.conditions == nil {
		ac.conditions = make(map[string]*problemCondition)
	}
	if existing := ac.conditions[kind]; existing != nil && existing.reason == reason {
		existing.severity = severity
		existing.detail = detail
		existing.action = action
		existing.until = until
		return
	}
	ac.conditions[kind] = &problemCondition{
		severity: severity,
		reason:   reason,
		detail:   detail,
		action:   action,
		since:    time.Now(),
		until:    until,
	}
}

// clearCondition announces that a standing problem is over and forgets it.
// Silence is not an acceptable way to report a recovery: the operator has been
// staring at an error for hours and must be told, in words, that it is gone.
func (ac *AstroCam) clearCondition(kind, resolution string) {
	cond := ac.conditions[kind]
	if cond == nil {
		return
	}
	delete(ac.conditions, kind)
	fmt.Println()
	printWrapped("=== ", fmt.Sprintf("RESOLVED: %s - %s (the problem lasted %s).",
		cond.reason, resolution, formatElapsedDuration(time.Since(cond.since))))
	fmt.Println()
}

// hasCondition reports whether a standing problem of this kind is in force.
func (ac *AstroCam) hasCondition(kind string) bool {
	return ac.conditions[kind] != nil
}

// reportStatus ends every cycle, and it is the one place an operator has to
// look: the last lines on the terminal always say whether the server is taking
// uploads, how many archives have gone out and when the last one did, what is
// still waiting, and when the camera last produced an image.
//
// Before this existed a healthy cycle printed two lines that said nothing, and
// a troubled one could bury its one important message under the chatter of
// fifty fields. A window that shows only what happened in the last few seconds
// cannot answer "is it working?" - these lines can, and because they are
// printed again on every cycle they cannot scroll out of sight.
//
// Standing problems are part of it. The full banner of each is printed when it
// starts and then every conditionFullBannerEveryCycles cycles; a problem with
// the server takes the place of the "Server OK" sentence, and any other one
// gets a line of its own below the status lines.
func (ac *AstroCam) reportStatus() {
	var others []*problemCondition
	// Fixed order, so the same problem always appears in the same place.
	for _, kind := range []string{conditionDelivery, conditionInput, conditionPacking, conditionTransfer, conditionArchive} {
		cond := ac.conditions[kind]
		if cond == nil {
			continue
		}
		cond.cycles++
		if cond.cycles == 1 || cond.cycles%conditionFullBannerEveryCycles == 0 {
			ac.printConditionBanner(cond)
		}
		if kind != conditionDelivery {
			others = append(others, cond)
		}
	}

	for _, line := range ac.statusLines(time.Now()) {
		fmt.Println(line)
	}
	for _, cond := range others {
		ac.printConditionReminder(cond)
	}
}

// statusLines builds the status report for one cycle. Every line fits an 80
// column console, and nothing here asks the terminal how wide it is or moves
// the cursor: the report is plain text that is simply printed again each time.
//
// The first line is about the far end (the server, and what has reached it),
// the second about this end (what is waiting here, and what the camera is
// doing). The second starts with the version of this program, so that a
// photograph of the screen always shows which release produced it.
func (ac *AstroCam) statusLines(now time.Time) []string {
	var far []string
	if cond := ac.conditions[conditionDelivery]; cond != nil {
		far = append(far, fmt.Sprintf("!!! %s for %s.",
			strings.ToUpper(cond.reason), formatElapsedDuration(now.Sub(cond.since))))
		if !cond.until.IsZero() {
			far = append(far, fmt.Sprintf("Next try at %s.", cond.until.Format("15:04:05")))
		}
	} else if ac.serverOKAt.IsZero() && ac.serverAskedAt.IsZero() {
		// The server is only ever contacted when there is an archive to hand
		// over, so until then there is honestly nothing to say about it.
		far = append(far, "Server not contacted yet: no archive has been ready to upload.")
	} else if ac.serverOKAt.IsZero() {
		// An old server knows nothing of status checks. It answered, but only
		// an upload that goes through will show that it is taking them.
		far = append(far, fmt.Sprintf("Server answered at %s but gave no status.",
			clockTime(ac.serverAskedAt, now)))
	} else {
		name := ac.serverOKName
		if name == "" {
			name = "Server"
		}
		sentence := fmt.Sprintf("%s OK at %s", name, clockTime(ac.serverOKAt, now))
		if ac.serverWarning != "" {
			sentence += ", but " + ac.serverWarning
		}
		far = append(far, sentence+".")
	}
	if ac.uploadedCount > 0 {
		far = append(far, fmt.Sprintf("%s uploaded, last at %s.",
			plural(ac.uploadedCount, "archive", "archives"), clockTime(ac.lastUploadOK, now)))
	} else if !ac.serverAskedAt.IsZero() || ac.hasCondition(conditionDelivery) {
		// Left out while the server has not even been contacted: that sentence
		// already says that nothing has gone out.
		far = append(far, "No archives uploaded yet.")
	}
	lines := flowLines(now.Format("15:04:05")+" ", statusIndent, far)

	var near []string
	if ac.queuedCount == 0 {
		near = append(near, "Queue empty.")
	} else {
		sentence := plural(ac.queuedCount, "archive", "archives") + " queued"
		// A server problem already says when the next attempt is due.
		if next := ac.nextUploadAt(now); !next.IsZero() && !ac.hasCondition(conditionDelivery) {
			sentence += ", next upload at " + next.Format("15:04:05")
		}
		near = append(near, sentence+".")
	}
	if ac.lastImageAt.IsZero() {
		near = append(near, "No images seen yet.")
	} else {
		near = append(near, fmt.Sprintf("Last image at %s.", clockTime(ac.lastImageAt, now)))
	}
	if ac.waitingFields > 0 {
		verb, whose := "waits", "its"
		if ac.waitingFields > 1 {
			verb, whose = "wait", "their"
		}
		what := "for more images"
		if ac.config.Count == 2 {
			what = "for " + whose + " 2nd image"
		}
		near = append(near, fmt.Sprintf("%s %s %s.",
			plural(ac.waitingFields, "field", "fields"), verb, what))
	}
	lines = append(lines, flowLines(ac.versionGutter(), statusIndent, near)...)

	if ac.strayCount > 0 {
		verb := "match"
		if ac.strayCount == 1 {
			verb = "matches"
		}
		lines = append(lines, fmt.Sprintf("%sNOTE: %s %s no field in areas.txt (ignored).",
			statusIndent, plural(ac.strayCount, "recent FITS file", "recent FITS files"), verb))
	}
	return lines
}

// versionGutter is what starts the second status line: the version of this
// program, set in the space under the time of day.
func (ac *AstroCam) versionGutter() string {
	switch {
	case ac.label == "":
		return statusIndent
	case len(ac.label) < len(statusIndent):
		return fmt.Sprintf("%*s ", len(statusIndent)-1, ac.label)
	}
	return ac.label + " "
}

// flowLines sets sentences into lines no longer than consoleWidth. The first
// line starts with the text given as first and the others with rest. A
// sentence is moved to the next line whole rather than split, and only one too
// long for a line of its own is broken between words.
func flowLines(first, rest string, sentences []string) []string {
	var lines []string
	line, empty := first, true // empty: nothing but its opening text so far

	fits := func(text string) bool {
		if empty {
			return len(line)+len(text) <= consoleWidth
		}
		return len(line)+1+len(text) <= consoleWidth
	}
	put := func(text string) {
		if !empty {
			line += " "
		}
		line += text
		empty = false
	}
	nextLine := func() {
		lines = append(lines, line)
		line, empty = rest, true
	}

	for _, sentence := range sentences {
		switch {
		case fits(sentence):
			put(sentence)
		case !empty && len(rest)+len(sentence) <= consoleWidth:
			nextLine()
			put(sentence)
		default:
			for _, word := range strings.Fields(sentence) {
				if !empty && !fits(word) {
					nextLine()
				}
				put(word)
			}
		}
	}
	if !empty {
		lines = append(lines, line)
	}
	return lines
}

// clockTime renders a moment for the status lines: the time of day, with the
// date in front once the moment is more than half a day away. "Last image at
// 05:14:40" would otherwise look just as healthy the evening after. The date
// does not appear at midnight: that is the middle of an observing night, and a
// time of day is not ambiguous within twelve hours of it.
func clockTime(t, now time.Time) string {
	age := now.Sub(t)
	if age < 0 {
		age = -age
	}
	if age < 12*time.Hour {
		return t.Local().Format("15:04:05")
	}
	return t.Local().Format("Jan 2 15:04")
}

// conditionBannerWidth is the width the banner text is folded to. Narrow enough
// to survive the 80 column console window the telescope machines actually run,
// so an explanation does not turn into an unreadable ribbon of wrapped text.
const conditionBannerWidth = 66

// printWrapped prints one piece of banner text, folded at whole words so it
// stays within conditionBannerWidth columns.
func printWrapped(prefix, text string) {
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= conditionBannerWidth:
			line += " " + word
		default:
			fmt.Printf("%s%s\n", prefix, line)
			line = word
		}
	}
	if line != "" {
		fmt.Printf("%s%s\n", prefix, line)
	}
}

// printConditionBanner prints the full description of a standing problem.
func (ac *AstroCam) printConditionBanner(cond *problemCondition) {
	fmt.Printf("\n!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n")
	printWrapped("!!! ", cond.severity+": "+cond.reason)
	fmt.Printf("!!! Started %s (%s ago).\n",
		cond.since.Format("2006-01-02 15:04:05"), formatElapsedDuration(time.Since(cond.since)))
	if detail := strings.TrimSpace(cond.detail); detail != "" {
		for _, line := range strings.Split(detail, "\n") {
			printWrapped("!!!   ", line)
		}
	}
	printWrapped("!!! ", "What astrocam-go is doing: "+cond.action)
	if !cond.until.IsZero() {
		fmt.Printf("!!! Next retry at %s (in %s).\n",
			cond.until.Format("15:04:05"), formatElapsedDuration(time.Until(cond.until)))
	}
	if ac.queueSummary != "" {
		printWrapped("!!! ", ac.queueSummary)
	}
	fmt.Printf("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n\n")
}

// printConditionReminder prints the single-line version of a standing problem
// that follows the status lines on every cycle. It is kept under 80 characters
// so that it stays one line in the console window and cannot be mistaken for
// ordinary chatter. How much is waiting is not repeated here: the status lines
// right above it say so.
func (ac *AstroCam) printConditionReminder(cond *problemCondition) {
	fmt.Printf("!!! %s %s: %s\n",
		cond.severity, formatElapsedDuration(time.Since(cond.since)), cond.reason)
}

// classifyServerError inspects a server UNMW_STATUS:ERROR body (or an upload
// error string) and returns a human-readable reason and the pause duration to
// apply: out of disk space pauses for DISK_SPACE_PAUSE, high system load for
// HIGH_LOAD_PAUSE. Anything else is treated as a transient error and uses the
// shorter high-load pause so we retry reasonably soon.
func classifyServerError(body string) (string, time.Duration) {
	lower := strings.ToLower(body)
	// Match the words the server actually uses, never the bare digits 507: the
	// error body carries the saved upload path, so a server process id such as
	// web_upload_2507841 used to be read as a full disk and stopped uploads for
	// an hour. The genuine 507 responses are turned into these phrases by
	// checkServerDiskSpace and uploadFile before they reach here.
	if strings.Contains(lower, "out of disk space") ||
		strings.Contains(lower, "insufficient storage") ||
		strings.Contains(lower, "insufficient disk space") {
		return "Server is out of disk space", DISK_SPACE_PAUSE
	}
	if strings.Contains(lower, "load too high") ||
		strings.Contains(lower, "system load") {
		return "Server is busy: system load too high", HIGH_LOAD_PAUSE
	}
	return "Server reported a temporary error", HIGH_LOAD_PAUSE
}

// What an HTTP error status says about where the fault lies.
const (
	scopeUnclear = iota // nothing certain: it may be about this one request
	scopeConfig         // wrong address or login; every request will get it
	scopeServer         // the server side is broken; nothing here can fix it
)

// classifyHTTPStatus says what an HTTP error status means when it comes from
// the web server in front of the upload script rather than from the script
// itself, whose own answers always carry an UNMW_STATUS marker.
//
// These used to be counted against the archive that happened to be on its way,
// and after three tries the operator was told that the archive was "most
// likely damaged" and could be removed by hand - for what was a mistyped
// password, a wrong address in config.env or a proxy with nothing behind it.
func classifyHTTPStatus(code int) (reason string, scope int) {
	switch {
	case code == 401 || code == 407:
		return fmt.Sprintf("Server refuses the login (HTTP %d)", code), scopeConfig
	case code == 403:
		return "Server refuses access (HTTP 403)", scopeConfig
	case code == 404 || code == 410:
		return fmt.Sprintf("Upload address not found on the server (HTTP %d)", code), scopeConfig
	case code >= 300 && code < 400:
		return fmt.Sprintf("Server redirects uploads elsewhere (HTTP %d)", code), scopeConfig
	case code >= 500 && code != 507:
		return fmt.Sprintf("Server-side error (HTTP %d)", code), scopeServer
	}
	return "", scopeUnclear
}

// httpAnswerText describes an HTTP error answer for the banner: the status
// line, whatever text the page carried, and where a redirect points. The
// address comes last, so that nothing runs on after it.
func httpAnswerText(status, location, body string) string {
	text := "HTTP " + status + "."
	if page := plainServerText(body); page != "" {
		text += " " + page
	}
	if location != "" {
		text += " The server redirects to: " + location
	}
	return text
}

// htmlTag matches one HTML tag.
var htmlTag = regexp.MustCompile(`<[^>]*>`)

// htmlNonText matches a style sheet or a script embedded in a page, up to its
// closing tag or, when the answer was cut short at the read limit, to the end.
// A proxy's error page can carry several kilobytes of CSS before its first
// visible word, and that would otherwise be all the console gets to show.
var htmlNonText = regexp.MustCompile(`(?is)<(?:style|script)\b.*?(?:</(?:style|script)\s*>|\z)`)

// plainServerText makes a server response fit for the console. An error page
// arrives as HTML, and its tags bury the one sentence that matters; the text
// is also cut to a length that still leaves room for the rest of a banner.
func plainServerText(body string) string {
	text := htmlNonText.ReplaceAllString(body, " ")
	text = html.UnescapeString(htmlTag.ReplaceAllString(text, " "))
	text = strings.Join(strings.Fields(text), " ")
	const limit = 600
	if runes := []rune(text); len(runes) > limit {
		text = string(runes[:limit]) + " ..."
	}
	return text
}

// operatorAdviceFor turns a machine-side reason into the one thing a human at
// the telescope can actually do about it. Printing "what to do" next to "what
// is wrong" is the difference between a message that gets acted on and one that
// gets ignored.
func operatorAdviceFor(reason string) string {
	const kept = "Nothing is lost: the archives are kept and retried. "
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "disk space"):
		return "Nothing is lost: images keep being packed here and every archive is " +
			"retried until the server confirms it. To fix it, free disk space on the " +
			"upload server - astrocam-go cannot do that from here. Once there is room " +
			"it carries on by itself at the next retry; restarting it retries at once."
	case strings.Contains(lower, "system load"):
		return kept + "The server should recover on its own; no action is needed " +
			"unless this lasts for hours."
	case strings.Contains(lower, "unreachable"):
		return kept + "Check the network connection, that the upload server is up, " +
			"and that SAI_SERVER in config.env is its address."
	case strings.Contains(lower, "refuses the login"), strings.Contains(lower, "refuses access"):
		return kept + "The server does not accept the user name and password it is " +
			"given: check SAI_USERNAME and SAI_PASSWORD in config.env, then restart astrocam-go."
	case strings.Contains(lower, "address not found"):
		return kept + "The server has no upload script at the address it is asked " +
			"for: check SAI_SERVER in config.env, then restart astrocam-go."
	case strings.Contains(lower, "redirects"):
		return kept + "An archive cannot follow a redirect. Set SAI_SERVER in config.env " +
			"to the address the server redirects to, then restart astrocam-go."
	case strings.Contains(lower, "server-side error"):
		return kept + "The fault is on the server or on a proxy in front of it, and " +
			"has to be fixed there; nothing needs changing on this computer."
	}
	return "Nothing is lost: the archives are kept and retried until the server accepts them."
}

// pauseUploads records that uploads must be skipped until a future time and
// raises a standing condition so the pause stays on screen for as long as it
// lasts, instead of scrolling away after one line. fromStatusCheck says whether
// the server's status check found the problem, or the answer to an upload did.
// detail is the server's answer, already made plain text by plainServerText or
// httpAnswerText; it is not cleaned a second time here, because text that the
// page itself showed as "&lt;script" would then be taken for a script block
// and cut off.
// In test mode, every reason to pause is fatal (exit immediately).
func (ac *AstroCam) pauseUploads(reason string, duration time.Duration, detail string, fromStatusCheck bool) {
	if ac.testMode {
		fmt.Print(testModeFatalText(reason, detail))
		os.Exit(1)
	}
	ac.uploadPauseUntil = time.Now().Add(duration)
	// Only the headline here: the banner printed at the end of this same cycle
	// carries the server's own words, the elapsed time and what happens next,
	// and repeating all of that twice in a row only makes it easier to skip.
	fmt.Printf("%s. Pausing uploads for %s, will retry after %s.\n",
		reason, formatPauseDuration(duration), ac.uploadPauseUntil.Format("15:04:05"))
	ac.setCondition(conditionDelivery, "ERROR", reason,
		"Server response: "+detail,
		fmt.Sprintf("waiting %s before trying again, and packing new images in the meantime. %s",
			formatPauseDuration(duration), operatorAdviceFor(reason)),
		ac.uploadPauseUntil)
	ac.conditions[conditionDelivery].fromStatusCheck = fromStatusCheck
}

// testModeFatalText is what a test run prints before it gives up on a server
// that will not take uploads. The server's own words go with the reason: the
// log of an automated run is read after the fact, and without them an upload
// script that refuses (its answers carry UNMW_STATUS) cannot be told from a
// proxy in front of it that cannot reach the script at all. detail is plain
// text already, as for pauseUploads.
func testModeFatalText(reason, detail string) string {
	text := fmt.Sprintf("FATAL ERROR (Test Mode): %s\n", reason)
	if response := strings.TrimSpace(detail); response != "" {
		text += fmt.Sprintf("  Server response: %s\n", response)
	}
	return text
}

// isUploadPaused returns true if we are still within a pause window set after a
// server-side rejection (high load or out of disk space).
//
// When the window runs out the condition is deliberately NOT cleared here: the
// pause ending only means it is time to try again, not that the server has
// room. The condition is cleared when an upload actually succeeds, so a disk
// that stays full stays on screen as one continuous problem rather than being
// declared fixed once an hour.
func (ac *AstroCam) isUploadPaused() bool {
	if ac.uploadPauseUntil.IsZero() {
		return false
	}
	if time.Now().After(ac.uploadPauseUntil) {
		ac.uploadPauseUntil = time.Time{} // Reset
		if cond := ac.conditions[conditionDelivery]; cond != nil {
			cond.until = time.Time{}
			cond.action = "retrying the upload now. " + operatorAdviceFor(cond.reason)
			fmt.Printf("The pause after \"%s\" is over - retrying the upload now.\n", cond.reason)
		}
		return false
	}
	return true
}

// makeJobForArchive matches Python makeJobForArchive function
// uploadServerCandidates returns the servers to try for an upload, in order:
// the primary SAI_SERVER first, then the optional SAI_FALLBACK_SERVER (when set
// and different). Fail-over to the fallback happens only when a server is
// unreachable at the network level - see makeJobForArchive.
func (ac *AstroCam) uploadServerCandidates() []string {
	servers := []string{ac.config.Server}
	fallback := strings.TrimSpace(ac.config.FallbackServer)
	if fallback != "" && fallback != ac.config.Server {
		servers = append(servers, fallback)
	}
	return servers
}

// uploadErrorIsServerRejection reports whether an upload error carries a
// server-side rejection (out of disk space or high load) that warrants a pause.
func uploadErrorIsServerRejection(err error) bool {
	lowerErr := strings.ToLower(err.Error())
	return strings.Contains(lowerErr, "out of disk space") ||
		strings.Contains(lowerErr, "insufficient storage") ||
		strings.Contains(lowerErr, "insufficient disk space") ||
		strings.Contains(lowerErr, "system load") ||
		strings.Contains(lowerErr, "load too high")
}

// pickServer runs the status check (the preflight) before an upload and
// returns the server to upload to. A server that cannot be reached is skipped
// so the fallback is tried; a server that answers with a rejection pauses
// uploads instead, since failing over would only move the same problem to the
// other machine.
func (ac *AstroCam) pickServer(servers []string, from int) (string, int, bool) {
	const unreachable = "Upload server is unreachable"

	// While a server stays unreachable, the same network error comes back on
	// every cycle. It is printed when it is new; after that the status lines
	// name the problem each cycle and the banner repeats the details, which is
	// enough - the same error text every fifteen seconds only pushes
	// everything else off the screen.
	known := ""
	if cond := ac.conditions[conditionDelivery]; cond != nil && cond.reason == unreachable {
		known = cond.detail
	}
	var failures []string
	printed := true // whether the failure of the previous server was printed

	for idx := from; idx < len(servers); idx++ {
		server := servers[idx]
		if idx > 0 && printed {
			fmt.Printf("Primary server unreachable - trying fallback server: %s\n", server)
		}

		answer := ac.checkServerDiskSpace(server)
		if answer.code != 0 {
			ac.serverAskedAt = time.Now()
		}
		switch answer.status {
		case "stopped":
			return "", 0, false
		case "unreachable":
			failure := fmt.Sprintf("%s (%s)", server, answer.message)
			failures = append(failures, failure)
			if printed = !strings.Contains(known, failure); printed {
				fmt.Printf("Server %s is unreachable: %s\n", server, answer.message)
			}
			continue
		case "error":
			reason, pause := classifyServerError(answer.message)
			ac.pauseUploads(reason, pause, plainServerText(answer.message), true)
			return "", 0, false
		case "http":
			// The web server answered for the upload script, and what it said
			// holds for an upload as much as for this question. Sending the
			// archive after it would only collect the same answer the slow way.
			reason, _ := classifyHTTPStatus(answer.code)
			status := fmt.Sprintf("%d %s", answer.code, http.StatusText(answer.code))
			ac.pauseUploads(reason, HIGH_LOAD_PAUSE,
				httpAnswerText(status, answer.location, answer.message), true)
			return "", 0, false
		case "warning":
			fmt.Printf("Server disk space warning: %s\n", strings.TrimSpace(answer.message))
			ac.serverAnswersOK(idx, classifyServerWarning(answer.message))
		case "ok":
			ac.serverAnswersOK(idx, "")
		}
		return server, idx, true
	}

	detail := "Tried: " + strings.Join(failures, "; ")
	if detail != known {
		fmt.Printf("All upload servers are unreachable - keeping every archive for a later retry\n")
	}
	if ac.testMode {
		fmt.Printf("FATAL ERROR (Test Mode): all upload servers are unreachable\n")
		os.Exit(1)
	}
	ac.setCondition(conditionDelivery, "ERROR", unreachable, detail,
		"retrying every cycle, and packing new images in the meantime. "+
			operatorAdviceFor("unreachable"),
		time.Time{})
	ac.conditions[conditionDelivery].fromStatusCheck = true
	return "", 0, false
}

// classifyServerWarning puts the server's warning into the few words that fit
// the status line. The server's full text is printed when it arrives.
func classifyServerWarning(body string) string {
	if strings.Contains(strings.ToLower(body), "disk space") {
		return "it is LOW ON DISK SPACE"
	}
	return "it reports a WARNING"
}

// serverAnswersOK records that the server at position idx in the candidate
// list has just said it is taking uploads, with or without a warning.
//
// A standing problem that the status check itself had found is over at this
// point, and is withdrawn here rather than left on screen until an archive has
// gone through: the status line must not go on saying "out of disk space"
// after the server has said that it has room.
func (ac *AstroCam) serverAnswersOK(idx int, warning string) {
	ac.serverOKAt = time.Now()
	ac.serverOKName = ""
	if idx > 0 {
		ac.serverOKName = "Fallback server"
	}
	ac.serverWarning = warning
	if cond := ac.conditions[conditionDelivery]; cond != nil && cond.fromStatusCheck {
		ac.clearCondition(conditionDelivery, "the upload server answers its status check normally again")
	}
}

// uploadPhase delivers the archives waiting in the temp directory, oldest
// first. Nothing is ever given up on: an archive is deleted only once the
// server has confirmed it, and everything else stays on disk to be tried
// again on the next cycle, for as many nights as that takes.
//
// The phase is bounded in time so a long queue cannot stop new images from
// being packed. Whatever is left over is simply picked up next cycle.
func (ac *AstroCam) uploadPhase() {
	archives, err := ac.refreshQueue()
	if err != nil {
		fmt.Printf("Error scanning archive files: %v\n", err)
		return
	}

	// The pause is checked before the empty-queue shortcut on purpose. An empty
	// queue is not proof that anything was delivered - the very first symptom of
	// a full server is a queue that has not been emptied - and skipping the
	// check here used to leave the standing pause completely unreported.
	if ac.isUploadPaused() {
		// Nothing is printed here on purpose: the status lines at the end of
		// the cycle already say why the pause exists, how long it has lasted
		// and when the next attempt is due.
		return
	}
	if len(archives) == 0 {
		// An upload that ran out of time can only be about an archive that is
		// still waiting; with none left there is nothing that can time out.
		ac.clearCondition(conditionTransfer, "no archive is waiting to be uploaded any more")

		// Nothing to hand over. If the server was known to be in trouble, ask it
		// how it is doing anyway: a standing error must be either confirmed or
		// withdrawn, never left on the screen unchecked.
		if ac.hasCondition(conditionDelivery) {
			if _, _, ok := ac.pickServer(ac.uploadServerCandidates(), 0); ok {
				ac.clearCondition(conditionDelivery, "the upload server answers normally again")
			}
		}
		return
	}

	// Uploads are spaced apart. While the next one is not due yet the server is
	// not asked anything either; the status lines say when it will be.
	if !ac.waitForUploadSlot(false) {
		return
	}

	servers := ac.uploadServerCandidates()
	server, si, ok := ac.pickServer(servers, 0)
	if !ok {
		return
	}

	deadline := time.Now().Add(uploadPhaseBudget)
	changedServer := false
	for i := 0; i < len(archives); i++ {
		archiveFile := archives[i]
		if time.Now().After(deadline) {
			fmt.Printf("Looking for new images before uploading more; %s still queued.\n",
				plural(len(archives)-i, "archive", "archives"))
			return
		}
		if i > 0 || changedServer {
			// After a change of server the same archive is sent again as soon
			// as the spacing allows, rather than on some later cycle that would
			// start over with the server that has just failed.
			if !ac.waitForUploadSlot(changedServer) {
				return
			}
			changedServer = false
		}

		err := ac.uploadFile(archiveFile, server)
		if err == nil {
			delete(ac.rejects, archiveFile)
			delete(ac.refusedAfter, archiveFile)
			ac.uploadedCount++
			ac.lastUploadOK = time.Now()
			ac.serverAskedAt, ac.serverOKAt = ac.lastUploadOK, ac.lastUploadOK
			ac.serverOKName = ""
			if si > 0 {
				ac.serverOKName = "Fallback server"
			}
			// An archive the server has confirmed is the only proof that
			// delivery really works, so this is where a standing delivery
			// problem is declared over.
			ac.clearCondition(conditionDelivery, "an archive was delivered successfully")
			ac.clearCondition(conditionTransfer, "an archive was delivered successfully")
			if !ac.anyArchiveRefusedOften() {
				ac.clearCondition(conditionArchive, "the archive that was being refused has been accepted")
			}
			if delErr := ac.deleteFile(archiveFile); delErr != nil {
				fmt.Printf("Warning: Error deleting file after upload: %v\n", delErr)
			}
			continue
		}

		// The operator is stopping the program. The archive is intact and goes
		// out on the next start; there is nothing to report or to count.
		if errors.Is(err, errStopping) {
			return
		}

		// An error page arrives as HTML; print what it says, not its markup.
		fmt.Printf("Upload error (%s): %s\n", server, plainServerText(err.Error()))

		// A timeout means the server IS reachable, so do not fail over to the
		// other server as if it had vanished, and do not let the queue grind
		// through every archive at uploadTimeout each. Keep the archive and
		// come back next cycle.
		if errors.Is(err, errUploadTimeout) {
			fmt.Printf("The archive is kept and will be retried. Note that the server answered its\n")
			fmt.Printf("preflight check moments ago, so this is a SLOW or STALLED TRANSFER, not an\n")
			fmt.Printf("unreachable server.\n")
			// The status line goes on saying "Server OK", which is true, so
			// the transfer gets a problem of its own: otherwise the only sign
			// of uploads that never finish is a queue that keeps growing.
			ac.setCondition(conditionTransfer, "ERROR",
				"Uploads do not finish within "+formatPauseDuration(uploadTimeout),
				plainServerText(err.Error()),
				"keeping the archive and trying it again; every try starts again from the "+
					"beginning. Nothing is lost. If this goes on, the network link to the server "+
					"is too slow or keeps stopping: check it.",
				time.Time{})
			return
		}
		if !errors.Is(err, errServerUnreachable) && !errors.Is(err, errLocalFailure) {
			// The server got this archive and answered it, even if with a
			// refusal: transfers finish again, and the refusal is handled below.
			ac.clearCondition(conditionTransfer, "uploads reach the server and are answered again")
		}

		if errors.Is(err, errServerUnreachable) {
			// The upload itself died at the network level. Move on to the next
			// candidate server, the same way a failed preflight does, and try
			// this archive again there rather than waiting a whole cycle.
			next, ni, ok := ac.pickServer(servers, si+1)
			if !ok {
				fmt.Printf("No other server to try; the remaining archives are kept for the next cycle.\n")
				return
			}
			server, si = next, ni
			changedServer = true
			i--
			continue
		}
		if uploadErrorIsServerRejection(err) {
			reason, pause := classifyServerError(err.Error())
			ac.pauseUploads(reason, pause, plainServerText(err.Error()), false)
			return
		}

		// A refused login, a wrong address or a redirect is the same for every
		// archive, so it is a problem with the server (or with config.env) and
		// not with the archive that happened to be sent. Sending the others
		// after it would only collect the same answer once per archive.
		var answer *httpStatusError
		if errors.As(err, &answer) {
			if reason, scope := classifyHTTPStatus(answer.code); scope == scopeConfig {
				ac.pauseUploads(reason, HIGH_LOAD_PAUSE,
					httpAnswerText(answer.status, answer.location, answer.body), false)
				return
			}
		}

		// The server refused this particular archive. Keep it and let it fall
		// behind the others, so one archive cannot hold up the whole queue.
		ac.noteRefusal(archiveFile, err)
	}
}

// noteRefusal records that the server answered an upload without accepting the
// archive, and turns that into a standing problem once the same archive has
// been refused rejectsBeforeWarning times.
//
// Whose fault it is gets judged from what else has happened, never assumed.
// The message used to say "most likely damaged; it can be removed from the
// temp directory by hand" in every case, which is an invitation to delete good
// data whenever the trouble is really on the server. Only when other archives
// have been accepted in the meantime does the evidence point at this one.
//
// "In the meantime" is judged by counting the archives the server has
// confirmed, not by comparing clock times: two events moments apart can carry
// the same time on a coarse clock (Windows), and the archive would then be
// said to have nothing to compare it with although others went through.
func (ac *AstroCam) noteRefusal(archiveFile string, err error) {
	if ac.refusedAfter == nil {
		ac.refusedAfter = make(map[string]int)
	}
	if _, seen := ac.refusedAfter[archiveFile]; !seen {
		ac.refusedAfter[archiveFile] = ac.uploadedCount
	}
	ac.rejects[archiveFile]++
	times := ac.rejects[archiveFile]
	if times < rejectsBeforeWarning {
		return
	}

	name := filepath.Base(archiveFile)
	answer := plainServerText(err.Error())
	switch {
	case errors.Is(err, errLocalFailure):
		ac.setCondition(conditionArchive, "WARNING",
			"An archive cannot be read from this computer's disk",
			fmt.Sprintf("%s could not be read %d times: %v", name, times, err),
			"retrying it; nothing is deleted. The server has not been sent this archive, "+
				"so the fault is on this computer: another program may be holding the file "+
				"open, or the disk is failing.",
			time.Time{})
	case ac.uploadedCount > ac.refusedAfter[archiveFile]:
		ac.setCondition(conditionArchive, "WARNING",
			"The server keeps refusing one archive",
			fmt.Sprintf("%s has been refused %d times, while other archives were accepted. Last answer: %s",
				name, times, answer),
			"still delivering the other archives, and retrying this one after them. "+
				"Other archives go through, so the fault is in this archive or in the images "+
				"inside it; the server's answer above says what it objects to. The archive "+
				"stays in the temp directory - move it somewhere else by hand to end the retries.",
			time.Time{})
	case len(ac.rejects) > 1:
		ac.setCondition(conditionArchive, "WARNING",
			"The server refuses every archive it is sent",
			fmt.Sprintf("%d archives have been refused and none accepted in the meantime. Last answer (for %s): %s",
				len(ac.rejects), name, answer),
			"retrying them in turn; nothing is deleted. When no archive at all is accepted "+
				"the cause is usually on the server and not in the archives: show the "+
				"server's answer above to whoever looks after the upload server.",
			time.Time{})
	default:
		ac.setCondition(conditionArchive, "WARNING",
			"The server keeps refusing an archive",
			fmt.Sprintf("%s has been refused %d times. Last answer: %s", name, times, answer),
			"retrying it; nothing is deleted. No other archive has been sent "+
				"in the meantime, so it is not yet clear whether the fault is in this archive "+
				"or on the server - the server's answer above is the place to look.",
			time.Time{})
	}
}

// makeJobForAreas packs every field that has a complete visit waiting, in
// every directory that may hold images.
//
// Packing never depends on the state of the server. An archive in the temp
// directory is safe, because it is retried until it is delivered, so the
// shortest possible path from the camera directory into the temp directory is
// what actually protects a night's data.
//
// Along the way it notes, for the status lines, what could NOT be packed and
// why: fields still waiting for the rest of their images, sets that were given
// up, and image files that belong to no field at all. An image that is not
// being uploaded always has a reason, and the operator is owed that reason in
// words rather than a count of files and a shrug.
func (ac *AstroCam) makeJobForAreas() {
	dirs := ac.scanDirs()

	var existing []string
	for _, d := range dirs {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			existing = append(existing, d)
		}
	}
	if len(existing) == 0 {
		ac.setCondition(conditionInput, "ERROR", "No camera image directory can be found",
			fmt.Sprintf("Looked for: %v", dirs),
			"looking again every cycle. Nothing can be packed or uploaded until one of "+
				"these directories exists - check that the camera software is running and "+
				"writing where SAI_CAMERA_DIRECTORY in config.env says it does.",
			time.Time{})
		// Nothing can be said about folders that are not there.
		ac.waitingFields, ac.openFields = 0, nil
		ac.noteStrays(nil)
		return
	}
	ac.clearCondition(conditionInput, "a camera image directory is readable again")
	ac.reportScanSet(existing)

	// Candidates from every scanned folder are pooled before a visit is chosen,
	// so a visit whose frames straddle a folder boundary is still seen as one
	// visit. Grouping by frame time rather than by folder is what keeps the
	// frames of two different visits out of the same archive.
	stamps := make(map[string]fileStamp)
	ready := make(map[string][]candidate)
	unready := make(map[string][]pendingFrame)
	seenName := make(map[string]bool)
	var strays []strayFile

	for _, dir := range existing {
		found := ac.scanDir(dir, stamps)
		for area, cands := range found.ready {
			for _, c := range cands {
				// The same base name in two folders is one frame as far as an
				// archive is concerned; keep the first copy found.
				if seenName[c.name] {
					continue
				}
				seenName[c.name] = true
				ready[area] = append(ready[area], c)
			}
		}
		for _, p := range found.pending {
			if seenName[p.name] {
				continue
			}
			seenName[p.name] = true
			unready[p.area] = append(unready[p.area], p)
		}
		strays = append(strays, found.strays...)
	}

	now := time.Now()
	open := make(map[string]bool)
	waiting, leftImages, leftFields, explained := 0, 0, 0, 0
	hasNewFiles := false
	packedAny := false
	var packErr error
	var packErrArea string

	for _, area := range ac.areas {
		// Packing one archive takes a moment; an operator who has asked the
		// program to stop should not have to sit through all the others.
		if ac.stopping() {
			break
		}
		cands, pending := ready[area], unready[area]
		if len(cands) == 0 && len(pending) == 0 {
			continue
		}
		// A field listed twice in areas.txt is still one field.
		delete(ready, area)
		delete(unready, area)

		for _, c := range cands {
			if c.mtime.After(ac.lastImageAt) {
				ac.lastImageAt = c.mtime
			}
		}
		for _, p := range pending {
			if p.mtime.After(ac.lastImageAt) {
				ac.lastImageAt = p.mtime
			}
		}

		packed := false
		if len(cands) > 0 {
			// Settle on one time source for this field before any gap between
			// its frames is measured.
			resolveFrameTimes(cands)

			if visit := selectVisit(cands, ac.config.Count, visitGap); visit != nil {
				hasNewFiles = true
				archiveFile, err := ac.packVisit(area, visit)
				if err != nil {
					fmt.Printf("Error processing area %s: %v\n", area, err)
					packErr, packErrArea = err, area
				} else {
					fmt.Printf("Archive created: %s\n", filepath.Base(archiveFile))
					packed = true
					packedAny = true
					cands = framesNotIn(cands, visit)
				}
			}
		}

		// The rest is about what is still lying in the camera folders for this
		// field, and why it was not packed just now.
		if len(cands)+len(pending) == 0 {
			continue
		}
		if selectVisit(cands, ac.config.Count, visitGap) != nil {
			// Another complete set (or the one that could not be packed a
			// moment ago) is waiting; it is taken on the next cycle.
			continue
		}
		newest, recent := newestFrames(cands, pending, visitGap)
		if now.Sub(newest) <= visitGap {
			// Something arrived for this field within the time a visit may
			// take, so its set can still be completed.
			open[area] = true
			if recent < ac.config.Count {
				waiting++
			}
			continue
		}
		if len(cands) == 0 {
			// Only files that are not finished FITS images; each of those has
			// been reported once already.
			continue
		}

		// No new image for longer than a visit may last: whatever is here will
		// never become a set. Say so at the moment the wait is given up, when
		// the operator can still see which field it was about.
		if ac.openFields[area] && !packed {
			for _, line := range describeNoSet(area, cands, ac.config.Count, visitGap) {
				fmt.Println(line)
			}
			explained += len(cands)
		}
		leftImages += len(cands)
		leftFields++
	}

	ac.openFields = open
	ac.waitingFields = waiting
	ac.notePacking(packErrArea, packErr, packedAny, hasNewFiles)

	// Incomplete sets that were already old when this run first saw them are
	// summed up once. Listing them field by field would bury the screen in
	// last month's leftovers every time the program starts. The sum waits for
	// a cycle that packs nothing: while a backlog of complete sets is still
	// being worked through, what will be left over is not yet known.
	if leftImages > 0 && !ac.leftoversTold && !hasNewFiles {
		ac.leftoversTold = true
		if leftImages > explained {
			fmt.Printf("Left in the camera folders: %s of %s that never made a\n",
				plural(leftImages, "older image", "older images"), plural(leftFields, "field", "fields"))
			fmt.Printf("  complete set (%d images within %s). They are not uploaded.\n",
				ac.config.Count, formatPauseDuration(visitGap))
		}
	}

	ac.noteStrays(strays)

	// Only the files seen this cycle are remembered, so this table cannot grow
	// without bound during a run that lasts for weeks.
	ac.stamps = stamps

	// In test mode, track if we've found files yet
	if ac.testMode && hasNewFiles {
		ac.testStartTime = time.Now() // Reset timeout when we find files
	}
}

// notePacking keeps a standing problem on the screen while complete sets of
// images cannot be packed. A set that fails is tried again on every cycle, so
// the error line alone scrolled away and came back without ever standing out:
// a full disk on this computer, or a processed folder that cannot be written
// to, could stop every upload while the status lines looked calm.
//
// err is the last packing error of this cycle, packed says whether anything
// was packed in it, and attempted whether a complete set was there to pack.
func (ac *AstroCam) notePacking(area string, err error, packed, attempted bool) {
	// Images left behind by a partial move are forgotten once a person (or a
	// later archive) has taken them out of the camera folder, or once their
	// name is in the processed folder, which keeps them from being sent again.
	for path := range ac.stranded {
		_, inCamera := os.Stat(path)
		_, inProcessed := os.Stat(filepath.Join(ac.config.ProcessedDirectory, filepath.Base(path)))
		if inCamera != nil || inProcessed == nil {
			delete(ac.stranded, path)
		}
	}
	if cond := ac.conditions[conditionPacking]; cond != nil && cond.reason == strandedReason &&
		len(ac.stranded) == 0 && err == nil {
		ac.clearCondition(conditionPacking, "those images are not in the camera folder any more")
	}

	switch {
	case ac.stopping():
		// Packing was cut short on purpose; nothing can be concluded from it.
	case err != nil:
		reason := "Images cannot be packed into archives"
		action := "trying again every cycle. Nothing is lost: the images stay in the camera " +
			"folder until an archive of them is complete. Check that the disk that holds " +
			"the temp folder (" + ac.tempDirectory + ") is not full and can be written to."
		if errors.Is(err, errImagesNotMoved) {
			reason = "Packed images cannot be moved into the processed folder"
			action = "trying again every cycle; nothing is uploaded from these images until " +
				"they can be moved. Nothing is lost: they stay in the camera folder. Check that " +
				"the processed folder (" + ac.config.ProcessedDirectory + ") exists, is not full " +
				"and can be written to, and that no other program (an image viewer, a virus " +
				"scanner) has the images open."
		}
		ac.setCondition(conditionPacking, "ERROR", reason,
			fmt.Sprintf("Field %s: %v", area, err), action, time.Time{})
	case len(ac.stranded) > 0:
		ac.setCondition(conditionPacking, "WARNING", strandedReason,
			strandedDetail(ac.stranded),
			"uploading as usual. These images went out in an archive already: move them "+
				"into the processed folder by hand, or they may be sent a second time or be "+
				"reported as an incomplete visit. This warning ends when they are gone from "+
				"the camera folder.",
			time.Time{})
	case packed:
		ac.clearCondition(conditionPacking, "images are being packed again")
	case !attempted:
		ac.clearCondition(conditionPacking, "no complete set is waiting to be packed any more")
	}
}

// strandedReason names the warning about images that were packed and uploaded
// but could not be moved out of the camera folder.
const strandedReason = "Uploaded images are still in the camera folder"

// strandedDetail names the images left in the camera folder, the first few in
// full and the rest counted.
func strandedDetail(stranded map[string]bool) string {
	paths := make([]string, 0, len(stranded))
	for path := range stranded {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	const shown = 5
	text := "Still in the camera folder: " + strings.Join(paths[:min(len(paths), shown)], ", ")
	if len(paths) > shown {
		text += fmt.Sprintf(" and %d more", len(paths)-shown)
	}
	return text
}

// reportScanSet says which folders are being watched, whenever that changes.
//
// Every night adds a dated folder and none is ever dropped, so after a few
// months the full list runs to several screens. The first few are shown and
// the rest are counted, which is enough to see that the right place is being
// watched without pushing everything else off the terminal.
func (ac *AstroCam) reportScanSet(existing []string) {
	set := strings.Join(existing, "\n")
	if set == ac.lastScanSet {
		return
	}
	ac.lastScanSet = set

	const shown = 3
	fmt.Printf("Watching %s for images:\n", plural(len(existing), "folder", "folders"))
	for i, dir := range existing {
		if i == shown && len(existing) > shown+1 {
			fmt.Printf("  ... and %d older ones beside them\n", len(existing)-shown)
			break
		}
		fmt.Printf("  %s\n", dir)
	}
}

// noteStrays records how many recent FITS files match no field, for the status
// lines, and explains them: when they first appear, and again now and then
// while more keep arriving.
//
// Such files are skipped without a word by the scan itself, which is exactly
// what happens all night long when the camera software names its files in a
// way this program does not expect - and a night of images that are never
// uploaded should not look, on screen, the same as a night with no images.
func (ac *AstroCam) noteStrays(strays []strayFile) {
	ac.strayCount = len(strays)
	if len(strays) == 0 {
		ac.strayTold = 0
		return
	}
	if len(strays) <= ac.strayTold {
		// Nothing new since the last explanation. Remember the smaller number,
		// so that the next file to arrive counts as new.
		ac.strayTold = len(strays)
		return
	}
	if ac.strayTold > 0 && ac.cycle-ac.strayToldCycle < conditionFullBannerEveryCycles {
		return
	}
	ac.strayTold, ac.strayToldCycle = len(strays), ac.cycle

	newest := strays[0]
	for _, s := range strays[1:] {
		if s.mtime.After(newest.mtime) {
			newest = s
		}
	}
	verb, they := "match", "are"
	if len(strays) == 1 {
		verb, they = "matches", "is"
	}
	period := formatPauseDuration(recentFileWindow)
	if recentFileWindow == time.Hour {
		period = "hour" // "in the last hour", not "in the last 1 hour"
	}
	fmt.Printf("NOTE: %s written in the last %s %s no field in areas.txt\n",
		plural(len(strays), "FITS file", "FITS files"), period, verb)
	fmt.Printf("  and %s ignored. The newest one:\n", they)
	fmt.Printf("    %s\n", newest.name)
	fmt.Printf("  A file is taken only when its name starts with a field name from areas.txt\n")
	fmt.Printf("  followed by \"_\" (in NINA the file pattern must start with $$TARGETNAME$$).\n")
}

// checkLeftoverArchives tests the archives already waiting in the temp
// directory, once, at startup.
//
// An archive that fails a LOCAL integrity test is damaged and can never be
// delivered however often it is sent, so it is moved aside rather than taking a
// delivery slot on every cycle from now on. Nothing is deleted, and this never
// gives up on an archive because of anything the SERVER said: an archive that
// is locally sound is retried for as long as it takes.
func (ac *AstroCam) checkLeftoverArchives() {
	archives, err := ac.getArchiveFiles()
	if err != nil || len(archives) == 0 {
		return
	}

	fmt.Printf("Checking %d archive(s) left by an earlier run...\n", len(archives))
	badDir := filepath.Join(ac.tempDirectory, "bad")

	for _, archiveFile := range archives {
		// Reading a long queue back takes a while; do not hold up an operator
		// who has changed their mind about starting the program.
		if ac.stopping() {
			return
		}
		testErr := ac.testArchive(archiveFile)
		if testErr == nil {
			continue
		}

		fmt.Printf("Archive %s is damaged and can never be delivered: %v\n",
			filepath.Base(archiveFile), testErr)
		if err := os.MkdirAll(badDir, 0755); err != nil {
			fmt.Printf("  could not create %s: %v\n", badDir, err)
			continue
		}
		if err := os.Rename(archiveFile, filepath.Join(badDir, filepath.Base(archiveFile))); err != nil {
			fmt.Printf("  could not move it aside: %v\n", err)
			continue
		}
		fmt.Printf("  moved to %s, nothing deleted. Frames still in the camera directory are packed again normally.\n", badDir)
	}
}

// checkTestTimeout checks if test mode should timeout
func (ac *AstroCam) checkTestTimeout() {
	if !ac.testMode {
		return
	}

	const testTimeout = 2 * time.Minute
	if time.Since(ac.testStartTime) > testTimeout {
		fmt.Printf("Test timeout: No new images found within %v. Exiting.\n", testTimeout)
		os.Exit(0) // Success exit - timeout is expected behavior in test mode
	}
}

// programLoop matches Python programLoop function
func (ac *AstroCam) programLoop() {
	ac.cycle++

	// Pack first. Getting a complete visit into the temp directory is what
	// makes it safe, and that must not wait behind a queue of retries.
	ac.makeJobForAreas()

	if !ac.stopping() {
		ac.uploadPhase()
	}
	if ac.stopping() {
		// The operator is leaving. What is still waiting is summed up once on
		// the way out, which is more use than a status report cut short.
		return
	}

	// Last, so that whatever else this cycle printed, the bottom of the terminal
	// says how things stand and names any problem that is still going on. An
	// operator who glances at the window sees that, not the chatter before it.
	// The queue is counted again first, because the uploads above have just
	// changed it (a failure to read it was reported by uploadPhase already).
	ac.refreshQueue()
	ac.reportStatus()

	// Check test timeout
	ac.checkTestTimeout()
}

func (ac *AstroCam) run() {
	fmt.Println("========================================")
	if ac.testMode {
		fmt.Println("ASTROCAM TEST MODE - AUTOMATED TESTING")
		fmt.Printf("Test timeout: 2 minutes\n")
	} else {
		fmt.Println("ASTROCAM NORMAL OPERATION - CONTINUOUS MONITORING")
	}
	fmt.Println("========================================")

	fmt.Printf("Configuration:\n")

	// Determine actual interval with minimum enforcement
	actualInterval := ac.config.Interval
	if actualInterval < MIN_INTERVAL {
		actualInterval = MIN_INTERVAL
	}

	// Display interval information
	if ac.config.RequestedInterval != actualInterval {
		fmt.Printf("  Scan interval: %d seconds (requested: %d, minimum: %d, using: %d)\n",
			actualInterval, ac.config.RequestedInterval, MIN_INTERVAL, actualInterval)
	} else {
		fmt.Printf("  Scan interval: %d seconds (minimum: %d)\n", actualInterval, MIN_INTERVAL)
	}

	fmt.Printf("  Upload server: %s\n", ac.config.Server)
	if servers := ac.uploadServerCandidates(); len(servers) > 1 {
		fmt.Printf("  Fallback server: %s\n", servers[1])
	}
	fmt.Printf("  Files per archive: %d\n", ac.config.Count)
	fmt.Printf("  Camera directory: %s\n", ac.config.CameraDirectory)
	fmt.Printf("  Processed directory: %s\n", ac.config.ProcessedDirectory)
	fmt.Printf("  Temp directory: %s\n", ac.tempDirectory)
	// Which archive format is used is nobody's concern unless SAI_ARCHIVE_MODE
	// was set to force one: the server takes both, and the line that reports
	// each archive names its format anyway.
	if ac.config.ArchiveMode != "auto" {
		var archiveFormatDesc string
		if ac.useRAR {
			archiveFormatDesc = fmt.Sprintf("RAR (using %s)", ac.rarPath)
		} else if ac.zipCompressed {
			archiveFormatDesc = "ZIP compressed"
		} else {
			archiveFormatDesc = "ZIP uncompressed"
		}
		fmt.Printf("  Archive format: %s (SAI_ARCHIVE_MODE=%s)\n", archiveFormatDesc, ac.config.ArchiveMode)
	}
	fmt.Printf("  FITS file extensions: .fts, .fits, .fit\n")

	if ac.hasCredentials() {
		fmt.Printf("  Authentication: Enabled (username: %s)\n", ac.config.Username)
	} else {
		fmt.Printf("  Authentication: Disabled (no credentials provided)\n")
	}
	fmt.Println("========================================")

	// A request to stop - Ctrl+C, closing the console window, a termination
	// signal - cancels this context, and everything that can take a while (an
	// upload under way, a status check, a wait) is tied to it. The request used
	// to be looked at only between two cycles, so a cycle that was waiting to
	// upload ignored Ctrl+C for minutes on end.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ac.ctx = ctx

	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		fmt.Printf("\nStop requested (%v). Stopping...\n", sig)
		cancel()
		// Whatever is still holding the program up, a second request is not
		// made to wait for it. Nothing is lost by leaving at once: an archive
		// is deleted only after the server has confirmed it.
		sig = <-sigChan
		fmt.Printf("Stop requested again (%v) - leaving at once.\n", sig)
		os.Exit(1)
	}()

	// Use the actual interval (with minimum enforcement)
	ticker := time.NewTicker(ac.cycleInterval())
	defer ticker.Stop()

	// Archives left by an earlier run are checked once, before the first
	// cycle, so a damaged one does not slow down every cycle that follows.
	ac.checkLeftoverArchives()

	// Run once immediately, then once per interval until told to stop.
	ac.programLoop()
	for !ac.stopping() {
		select {
		case <-ticker.C:
			ac.programLoop()
		case <-ctx.Done():
		}
	}

	// Say what is left behind, so that stopping never looks like losing data.
	_, err := ac.refreshQueue()
	switch {
	case err != nil:
		fmt.Printf("astrocam-go stopped.\n")
	case ac.queuedCount > 0:
		fmt.Printf("astrocam-go stopped. %s (%s) not yet uploaded: kept in\n",
			plural(ac.queuedCount, "archive", "archives"), humanBytes(ac.queuedBytes))
		fmt.Printf("  %s\n", ac.tempDirectory)
		fmt.Printf("  and sent the next time astrocam-go runs.\n")
	default:
		fmt.Printf("astrocam-go stopped. No archive was waiting to be uploaded.\n")
	}
}

// Version is set by build flags during release builds
var version string

// buildLabel names this binary: the release tag when it was built as a
// release, otherwise the source revision the Go toolchain recorded. It is
// printed at startup and in every status report, because a screenshot of the
// console is how problems get reported, and the first thing to know about a
// screenshot is which version of the program produced it.
func buildLabel() string {
	if version != "" {
		return version
	}
	revision, modified := "", false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if modified {
		// Built from files that differ from that revision.
		revision += "+"
	}
	return "dev-" + revision
}

func main() {
	// Disable Windows QuickEdit mode first thing to prevent console freezing
	// This function is implemented in platform-specific files (quickedit_*.go)
	disableQuickEditMode()

	// Define all flags consistently using flag package
	testMode := flag.Bool("test", false, "Run in test mode (exit on errors, timeout after 2 minutes)")
	showVersion := flag.Bool("version", false, "Show version information")

	// Parse all flags
	flag.Parse()

	// Handle version flag after parsing
	if *showVersion {
		if version != "" {
			fmt.Printf("AstroCam-GO %s\n", version)
		} else {
			fmt.Printf("AstroCam-GO %s (development build)\n", buildLabel())
		}
		return
	}

	// Acquire a file lock to prevent multiple instances from running simultaneously.
	// The lock file is placed next to the executable (or in the current directory as fallback).
	lockPath := "astrocam.lock"
	if execPath, err := os.Executable(); err == nil {
		lockPath = filepath.Join(filepath.Dir(execPath), lockPath)
	}
	lock, err := acquireFileLock(lockPath)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer lock.release()

	app, err := NewAstroCam(*testMode)
	if err != nil {
		log.Fatalf("Initialization failed: %v", err)
	}

	app.run()
}
