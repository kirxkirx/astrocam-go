package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
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
)

// errServerUnreachable marks a preflight/upload failure caused by the server
// being unreachable at the network level (DNS failure, timeout, connection
// refused) - as opposed to a server-side rejection (out of disk space, high
// load). Only an unreachable primary server triggers fail-over to the optional
// SAI_FALLBACK_SERVER; a reachable server that rejects the upload is handled by
// the existing pause-and-retry logic instead.
var errServerUnreachable = errors.New("server unreachable")

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
	fitsExtPattern   string               // Regex pattern matching all FITS file extensions (.fts, .fits, .fit)
	uploadPauseUntil time.Time            // Skip uploads until this time after a server-side rejection (high load or out of disk space)
	areaPatterns     []*regexp.Regexp     // one compiled matcher per areas entry, same order
	stamps           map[string]fileStamp // size and mtime of each candidate on the previous cycle
	rejects          map[string]int       // per-archive count of server content rejections
	noVisit          map[string]int       // per-area frame count already reported as "no single visit"
	lastScanSet      string               // last scan set printed, so it is logged only when it changes
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
	case "auto":
		fallthrough
	default:
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

	// Display mode and archive type information
	modeStr := "NORMAL OPERATION"
	if testMode {
		modeStr = "TEST"
	}

	var archiveTypeDesc string
	if useRAR {
		archiveTypeDesc = fmt.Sprintf("RAR (using %s)", rarPath)
	} else if zipCompressed {
		archiveTypeDesc = "ZIP compressed (built-in)"
	} else {
		archiveTypeDesc = "ZIP uncompressed (built-in)"
	}

	fmt.Printf("=== ASTROCAM STARTING IN %s MODE ===\n", modeStr)
	fmt.Printf("Archive mode: %s\n", config.ArchiveMode)
	fmt.Printf("Archive format: %s\n", archiveTypeDesc)

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
	ac.noVisit = make(map[string]int)

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

// collectCandidates reads one directory once and returns the files that may be
// packed, grouped by area. A file is a candidate only when it matches an area,
// looks like a finished FITS file, has not changed since the previous cycle,
// and is not already sitting in the processed directory. Every one of those is
// a statement about the file itself, so a file that qualifies stays qualified
// until it is packed.
//
// The stamps map is filled in with what every matching file looks like now; it
// becomes the comparison point for the next cycle.
func (ac *AstroCam) collectCandidates(dir string, stamps map[string]fileStamp) map[string][]candidate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("Warning: cannot read image directory %s: %v\n", dir, err)
		}
		return nil
	}

	byArea := make(map[string][]candidate)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		area := ac.areaForFile(name)
		if area == "" {
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
			continue
		}
		// Already delivered under this name; never pack it a second time.
		if _, err := os.Stat(filepath.Join(ac.config.ProcessedDirectory, name)); err == nil {
			continue
		}

		named, hasNamed := parseFrameTime(name)
		byArea[area] = append(byArea[area], candidate{
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
	return byArea
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
		return ac.sortByArchiveName(files[i]) < ac.sortByArchiveName(files[j])
	})

	return files, nil
}

// moveImages moves the frames of a packed archive into the processed
// directory. It reports an error when any of them could not be moved, because
// a frame left in the camera directory is packed again on a later cycle and
// the same images are then delivered twice.
//
// A frame is never deleted just because a file of the same name already exists
// in the processed directory: that would destroy an image which was never
// delivered under that name. Such a file is left alone for a human to sort out.
func (ac *AstroCam) moveImages(files []string) error {
	const maxRetries = 2
	const retryDelay = 3 * time.Second

	var failed []string
	for attempt := 1; attempt <= maxRetries; attempt++ {
		failed = nil

		for _, file := range files {
			basename := filepath.Base(file)
			targetPath := filepath.Join(ac.config.ProcessedDirectory, basename)

			if _, err := os.Stat(targetPath); err == nil {
				fmt.Printf("Error: %s already exists in the processed directory; leaving the camera copy in place\n",
					basename)
				failed = append(failed, file)
				continue
			}

			if err := os.Rename(file, targetPath); err != nil {
				fmt.Printf("Error: Cannot move file %s (attempt %d/%d): %v\n",
					basename, attempt, maxRetries, err)
				failed = append(failed, file)
			}
		}

		if len(failed) == 0 {
			return nil
		}
		if attempt < maxRetries {
			fmt.Printf("Waiting %v before retry...\n", retryDelay)
			time.Sleep(retryDelay)
			files = failed
		}
	}

	fmt.Printf("WARNING: %d image file(s) stayed in the camera directory:\n", len(failed))
	for _, file := range failed {
		fmt.Printf("  - %s\n", filepath.Base(file))
	}
	fmt.Printf("The archive itself is complete and will be delivered. These frames will be packed again on a later cycle, which sends them twice.\n")

	if ac.testMode {
		fmt.Printf("FATAL ERROR (Test Mode): Failed to move %d files after %d attempts\n", len(failed), maxRetries)
		os.Exit(1)
	}
	return fmt.Errorf("%d image file(s) could not be moved", len(failed))
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

// waitForUploadThrottle ensures 120 seconds between upload attempts
func (ac *AstroCam) waitForUploadThrottle() {
	const uploadThrottleDelay = 120 * time.Second

	if ac.lastUploadTime.IsZero() {
		// First upload, no need to wait
		return
	}

	timeSinceLastUpload := time.Since(ac.lastUploadTime)
	if timeSinceLastUpload < uploadThrottleDelay {
		waitTime := uploadThrottleDelay - timeSinceLastUpload
		fmt.Printf("Upload throttling: Waiting %v before next upload attempt...\n", waitTime.Round(time.Second))
		time.Sleep(waitTime)
	}
}

// checkServerDiskSpace sends a GET preflight request to the given server to
// check its disk space and load. Returns a status and a message string. Status
// is one of: "ok", "warning", "error" (server alive but rejecting),
// "unreachable" (network-level failure - candidate for fail-over) or "unknown"
// (server answered without a recognized UNMW_STATUS marker).
func (ac *AstroCam) checkServerDiskSpace(server string) (string, string) {
	req, err := http.NewRequest("GET", server, nil)
	if err != nil {
		return "unknown", fmt.Sprintf("failed to create request: %v", err)
	}

	if ac.hasCredentials() {
		req.SetBasicAuth(ac.config.Username, ac.config.Password)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Transport-level failure -> the server is unreachable (as opposed to a
		// server that answers with an error). This is what triggers fail-over.
		return "unreachable", fmt.Sprintf("preflight request to %s failed: %v", server, err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	body := string(bodyBytes)

	if strings.Contains(body, "UNMW_STATUS:ERROR") {
		return "error", body
	}
	if strings.Contains(body, "UNMW_STATUS:WARNING") {
		return "warning", body
	}
	if strings.Contains(body, "UNMW_STATUS:OK") {
		return "ok", ""
	}

	// No UNMW_STATUS marker: old server or unexpected response
	if resp.StatusCode == 507 {
		return "error", fmt.Sprintf("server is out of disk space (HTTP 507): %s", body)
	}

	return "unknown", ""
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

	if err := ac.moveImages(paths); err != nil {
		stayed := 0
		for _, path := range paths {
			if _, statErr := os.Stat(path); statErr == nil {
				stayed++
			}
		}
		if stayed == len(paths) {
			// Every frame is still in the camera directory, so this archive is
			// not the only copy of anything. Throw it away rather than deliver
			// the same visit again on every cycle from now on.
			os.Remove(finalPath)
			return ERROR, fmt.Errorf("archive %s discarded because none of its frames could be moved: %w",
				filepath.Base(finalPath), err)
		}
		fmt.Printf("Warning: archive %s is ready and will be delivered, but %v\n",
			filepath.Base(finalPath), err)
	}

	return finalPath, nil
}

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

// uploadFile matches FileUploader functionality with proper resource management
func (ac *AstroCam) uploadFile(filePath, server string) error {
	// Wait for upload throttling (120 seconds between uploads)
	ac.waitForUploadThrottle()

	fmt.Printf("Uploading %s to server: %s\n", filepath.Base(filePath), server)

	// Update last upload time before attempting upload
	ac.lastUploadTime = time.Now()

	// Open file with proper resource management
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
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
		return fmt.Errorf("failed to copy file data: %w", err)
	}

	writer.Close()

	// Create HTTP request
	req, err := http.NewRequest("POST", server, &body)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Only set authentication if credentials are provided
	if ac.hasCredentials() {
		req.SetBasicAuth(ac.config.Username, ac.config.Password)
		fmt.Printf("Using authentication for upload\n")
	} else {
		fmt.Printf("Uploading without authentication (no credentials provided)\n")
	}

	// Send request with timeout for large files/slow server
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Transport-level failure -> the server is unreachable. Wrap the
		// sentinel so the caller can fail over to SAI_FALLBACK_SERVER instead
		// of keeping the archive. Test-mode fatality is decided by the caller
		// once every candidate server has been tried.
		return fmt.Errorf("%w: upload to %s failed: %v", errServerUnreachable, server, err)
	}
	defer resp.Body.Close()

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
	// 503 "system load too high" -> short pause) from the server's message.
	uploadErr := fmt.Errorf("server returned status %d: %s; %s", resp.StatusCode, resp.Status, strings.TrimSpace(bodyStr))
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

// pauseUploads records that uploads must be skipped until a future time and
// prints an informative message naming the cause and how long the pause lasts.
// In test mode, a server-side rejection is fatal (exit immediately).
func (ac *AstroCam) pauseUploads(reason string, duration time.Duration, detail string) {
	if ac.testMode {
		fmt.Printf("FATAL ERROR (Test Mode): %s\n", reason)
		os.Exit(1)
	}
	ac.uploadPauseUntil = time.Now().Add(duration)
	fmt.Printf("%s. Pausing uploads for %s, will retry after %s.\nServer response: %s\n",
		reason, formatPauseDuration(duration),
		ac.uploadPauseUntil.Format("15:04:05"), strings.TrimSpace(detail))
}

// isUploadPaused returns true if we are still within a pause window set after a
// server-side rejection (high load or out of disk space).
func (ac *AstroCam) isUploadPaused() bool {
	if ac.uploadPauseUntil.IsZero() {
		return false
	}
	if time.Now().After(ac.uploadPauseUntil) {
		ac.uploadPauseUntil = time.Time{} // Reset
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

// pickServer runs the preflight check once per cycle and returns the server to
// upload to. A server that cannot be reached is skipped so the fallback is
// tried; a server that answers with a rejection pauses uploads instead, since
// failing over would only move the same problem to the other machine.
func (ac *AstroCam) pickServer(servers []string, from int) (string, int, bool) {
	for idx := from; idx < len(servers); idx++ {
		server := servers[idx]
		if idx > 0 {
			fmt.Printf("Primary server unreachable - trying fallback server: %s\n", server)
		}

		status, msg := ac.checkServerDiskSpace(server)
		switch status {
		case "unreachable":
			fmt.Printf("Server %s is unreachable: %s\n", server, msg)
			continue
		case "error":
			reason, pause := classifyServerError(msg)
			ac.pauseUploads(reason, pause, msg)
			return "", 0, false
		case "warning":
			fmt.Printf("Server disk space warning: %s\n", msg)
		}
		return server, idx, true
	}

	fmt.Printf("All upload servers are unreachable - keeping every archive for a later retry\n")
	if ac.testMode {
		fmt.Printf("FATAL ERROR (Test Mode): all upload servers are unreachable\n")
		os.Exit(1)
	}
	return "", 0, false
}

// uploadPhase delivers the archives waiting in the temp directory, oldest
// first. Nothing is ever given up on: an archive is deleted only once the
// server has confirmed it, and everything else stays on disk to be tried
// again on the next cycle, for as many nights as that takes.
//
// The phase is bounded in time so a long queue cannot stop new images from
// being packed. Whatever is left over is simply picked up next cycle.
func (ac *AstroCam) uploadPhase() {
	archives, err := ac.getArchiveFiles()
	if err != nil {
		fmt.Printf("Error scanning archive files: %v\n", err)
		return
	}
	if len(archives) == 0 {
		return
	}

	var waiting int64
	for _, a := range archives {
		if info, err := os.Stat(a); err == nil {
			waiting += info.Size()
		}
	}
	fmt.Printf("%d archive(s) waiting to be delivered (%d MB), oldest is %s\n",
		len(archives), waiting/(1024*1024), filepath.Base(archives[0]))

	if ac.isUploadPaused() {
		fmt.Printf("Uploads are paused until %s; the archives are kept and retried after that.\n",
			ac.uploadPauseUntil.Format("15:04:05"))
		return
	}

	servers := ac.uploadServerCandidates()
	server, si, ok := ac.pickServer(servers, 0)
	if !ok {
		return
	}

	deadline := time.Now().Add(uploadPhaseBudget)
	for i := 0; i < len(archives); i++ {
		archiveFile := archives[i]
		if time.Now().After(deadline) {
			fmt.Printf("Upload budget for this cycle is used up; %d archive(s) still waiting.\n",
				len(archives)-i)
			return
		}

		err := ac.uploadFile(archiveFile, server)
		if err == nil {
			delete(ac.rejects, archiveFile)
			if delErr := ac.deleteFile(archiveFile); delErr != nil {
				fmt.Printf("Warning: Error deleting file after upload: %v\n", delErr)
			}
			continue
		}

		fmt.Printf("Upload error (%s): %v\n", server, err)

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
			i--
			continue
		}
		if uploadErrorIsServerRejection(err) {
			reason, pause := classifyServerError(err.Error())
			ac.pauseUploads(reason, pause, err.Error())
			return
		}

		// The server refused this particular archive. Keep it and let it fall
		// behind the others, so one archive cannot hold up the whole queue.
		ac.rejects[archiveFile]++
	}
}

// makeJobForAreas packs every field that has a complete visit waiting, in
// every directory that may hold images.
//
// Packing never depends on the state of the server. An archive in the temp
// directory is safe, because it is retried until it is delivered, so the
// shortest possible path from the camera directory into the temp directory is
// what actually protects a night's data.
func (ac *AstroCam) makeJobForAreas() {
	dirs := ac.scanDirs()

	var existing []string
	for _, d := range dirs {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			existing = append(existing, d)
		}
	}
	if len(existing) == 0 {
		fmt.Printf("WARNING: none of the image directories exist yet: %v\n", dirs)
		return
	}
	if set := fmt.Sprintf("%v", existing); set != ac.lastScanSet {
		fmt.Printf("Scanning %d image directories: %v\n", len(existing), existing)
		ac.lastScanSet = set
	}

	// Candidates from every scanned folder are pooled before a visit is chosen,
	// so a visit whose frames straddle a folder boundary is still seen as one
	// visit. Grouping by frame time rather than by folder is what keeps the
	// frames of two different visits out of the same archive.
	stamps := make(map[string]fileStamp)
	byArea := make(map[string][]candidate)
	seenName := make(map[string]bool)

	for _, dir := range existing {
		for area, cands := range ac.collectCandidates(dir, stamps) {
			for _, c := range cands {
				// The same base name in two folders is one frame as far as an
				// archive is concerned; keep the first copy found.
				if seenName[c.name] {
					continue
				}
				seenName[c.name] = true
				byArea[area] = append(byArea[area], c)
			}
		}
	}

	hasNewFiles := false
	for _, area := range ac.areas {
		cands := byArea[area]
		if len(cands) == 0 {
			continue
		}

		// Settle on one time source for this field before any gap between its
		// frames is measured.
		resolveFrameTimes(cands)

		visit := selectVisit(cands, ac.config.Count, visitGap)
		if visit == nil {
			// Enough frames are present but they belong to different visits,
			// so none of them forms a set that may be packed. Say so once per
			// change rather than on every cycle for the rest of the night.
			if len(cands) >= ac.config.Count && ac.noVisit[area] != len(cands) {
				fmt.Printf("INFO: Area '%s' has %d ready frame(s) but no single visit of %d\n",
					area, len(cands), ac.config.Count)
			}
			ac.noVisit[area] = len(cands)
			continue
		}
		delete(ac.noVisit, area)

		hasNewFiles = true
		archiveFile, err := ac.packVisit(area, visit)
		if err != nil {
			fmt.Printf("Error processing area %s: %v\n", area, err)
			continue
		}
		fmt.Printf("Archive created: %s\n", filepath.Base(archiveFile))
	}

	// Only the files seen this cycle are remembered, so this table cannot grow
	// without bound during a run that lasts for weeks.
	ac.stamps = stamps

	// In test mode, track if we've found files yet
	if ac.testMode && hasNewFiles {
		ac.testStartTime = time.Now() // Reset timeout when we find files
	}
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
	// Pack first. Getting a complete visit into the temp directory is what
	// makes it safe, and that must not wait behind a queue of retries.
	fmt.Printf("Scanning camera directories... %s\n", time.Now().Format("2006-01-02 15:04:05"))
	ac.makeJobForAreas()

	fmt.Printf("Delivering archives... %s\n", time.Now().Format("2006-01-02 15:04:05"))
	ac.uploadPhase()

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

	fmt.Printf("  Files per archive: %d\n", ac.config.Count)
	fmt.Printf("  Camera directory: %s\n", ac.config.CameraDirectory)
	fmt.Printf("  Processed directory: %s\n", ac.config.ProcessedDirectory)
	fmt.Printf("  Temp directory: %s\n", ac.tempDirectory)
	fmt.Printf("  Archive mode: %s\n", ac.config.ArchiveMode)

	var archiveFormatDesc string
	if ac.useRAR {
		archiveFormatDesc = fmt.Sprintf("RAR (using %s)", ac.rarPath)
	} else if ac.zipCompressed {
		archiveFormatDesc = "ZIP compressed"
	} else {
		archiveFormatDesc = "ZIP uncompressed"
	}
	fmt.Printf("  Archive format: %s\n", archiveFormatDesc)
	fmt.Printf("  FITS file extensions: .fts, .fits, .fit\n")

	if ac.hasCredentials() {
		fmt.Printf("  Authentication: Enabled (username: %s)\n", ac.config.Username)
	} else {
		fmt.Printf("  Authentication: Disabled (no credentials provided)\n")
	}
	fmt.Println("========================================")

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Use the actual interval (with minimum enforcement)
	ticker := time.NewTicker(time.Duration(actualInterval) * time.Second)
	defer ticker.Stop()

	// Archives left by an earlier run are checked once, before the first
	// cycle, so a damaged one does not slow down every cycle that follows.
	ac.checkLeftoverArchives()

	// Run once immediately
	ac.programLoop()

	// Main loop
	for {
		select {
		case <-ticker.C:
			ac.programLoop()
		case sig := <-sigChan:
			fmt.Printf("\nShutdown signal received (%v). Performing cleanup...\n", sig)
			return
		}
	}
}

// Version is set by build flags during release builds
var version string

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
			fmt.Println("AstroCam-GO (development build)")
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
