package main

import (
	"archive/zip"
	"bufio"
	"bytes"
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
	MIN_INTERVAL     = 15     // Minimum allowed interval in seconds
	DEFAULT_INTERVAL = 15     // Default interval if not specified/invalid
	MAX_INTERVAL     = 86400  // Maximum allowed interval in seconds (24 hours)
)

type Config struct {
	Server             string
	Username           string
	Password           string
	CameraDirectory    string
	ProcessedDirectory string
	Interval           int
	RequestedInterval  int    // Store the original requested interval
	Count              int
	Prefix             string
	Postfix            string
	ArchiveMode        string // "auto", "rar", "zip", "zip-uncompressed"
}

type AstroCam struct {
	config         *Config
	areas          []string
	tempDirectory  string
	currentDir     string
	lastUploadTime time.Time
	useRAR         bool   // Whether to use RAR (true) or ZIP (false)
	archiveExt     string // ".rar" or ".zip"
	zipCompressed  bool   // Whether to compress ZIP files
	rarPath        string // Path to rar executable (if found)
	testMode       bool   // Whether running in test mode
	testStartTime  time.Time
	fitsExt        string // Determined FITS file extension (.fts, .fits, or .fit)
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
		Interval:          DEFAULT_INTERVAL,    // Use default instead of hardcoded 180
		RequestedInterval: DEFAULT_INTERVAL,    // Initialize both to default
		Count:             3,                   // default
		ArchiveMode:       "auto",             // default
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
				config.RequestedInterval = val  // Store what was requested
				config.Interval = DEFAULT_INTERVAL
			} else {
				// Valid value - store it (will be enforced to minimum later)
				config.RequestedInterval = val
				config.Interval = val
			}
		case "SAI_COUNT":
			if val, err := strconv.Atoi(value); err == nil {
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

// determineFitsExtension determines which FITS file extension to use
// by checking for existing files in the camera directory.
// Matches shell script logic: try fts, fits, fit in order, default to fts
func (ac *AstroCam) determineFitsExtension() string {
	possibleExtensions := []string{"fts", "fits", "fit"}
	
	fmt.Printf("Determining FITS extension in: %s\n", ac.config.CameraDirectory)
	
	for _, ext := range possibleExtensions {
		pattern := filepath.Join(ac.config.CameraDirectory, "*."+ext)
		matches, err := filepath.Glob(pattern)
		if err == nil && len(matches) > 0 {
			fmt.Printf("FITS file extension detected: .%s (found %d files)\n", ext, len(matches))
			return "." + ext
		}
		fmt.Printf("No .%s files found\n", ext)
	}
	
	// Default to .fts if no files found with any extension
	fmt.Printf("FITS file extension: .fts (default, no existing files found)\n")
	return ".fts"
}

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
		config:        config,
		areas:         areas,
		tempDirectory: tempDir,
		currentDir:    currentDir,
		lastUploadTime: time.Time{},
		useRAR:        useRAR,
		archiveExt:    archiveExt,
		zipCompressed: zipCompressed,
		rarPath:       rarPath,
		testMode:      testMode,
		testStartTime: time.Now(),
	}

	// Determine FITS file extension after creating the struct
	ac.fitsExt = ac.determineFitsExtension()

	return ac, nil
}

// fileBrowser matches Python _filebrowser method  
func (ac *AstroCam) fileBrowser(constellation, dir, ext string) ([]string, error) {
	// Fixed pattern to match Python: "(^" + constellation + "(_|-SF_).*\\" + ext + ")"
	pattern := fmt.Sprintf("^%s(_|-SF_).*%s", constellation, regexp.QuoteMeta(ext))
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("could not read directory %s: %w", dir, err)
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && regex.MatchString(entry.Name()) {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}

	return files, nil
}

// sortByNamePart matches Python _sortByNamePart method
func sortByNamePart(inputFileName string) string {
	filename := filepath.Base(inputFileName)
	pos := strings.Index(filename, "_")
	if pos == -1 {
		return filename
	}
	// Return everything after first underscore, removing extension
	// Find the last dot and remove from there
	lastDot := strings.LastIndex(filename, ".")
	if lastDot == -1 {
		return filename[pos+1:]
	}
	return filename[pos+1 : lastDot]
}

// sortByArchiveName matches Python _sortByArchiveName method  
func (ac *AstroCam) sortByArchiveName(archiveFileName string) string {
	filename := filepath.Base(archiveFileName)
	
	// Remove archive extension (.rar or .zip)
	pos := strings.LastIndex(filename, ac.archiveExt)
	if pos != -1 {
		filename = filename[:pos]
	}
	
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

// getArchiveFiles matches Python getArchiveFiles method
func (ac *AstroCam) getArchiveFiles() ([]string, error) {
	pattern := filepath.Join(ac.tempDirectory, "*"+ac.archiveExt)
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("error scanning for archive files: %w", err)
	}

	// Sort files using the same logic as Python
	sort.Slice(files, func(i, j int) bool {
		return ac.sortByArchiveName(files[i]) < ac.sortByArchiveName(files[j])
	})

	return files, nil
}

// getImageFiles matches Python _getImageFiles method
func (ac *AstroCam) getImageFiles(area string) (*FileGroup, error) {
	// Use the determined FITS extension instead of hardcoded ".fts"
	files, err := ac.fileBrowser(area, ac.config.CameraDirectory, ac.fitsExt)
	if err != nil {
		return nil, err
	}

	// Sort files by name part (matching Python logic)
	sort.Slice(files, func(i, j int) bool {
		return sortByNamePart(files[i]) < sortByNamePart(files[j])
	})

	// Take up to 'count' files
	maxFiles := ac.config.Count
	if len(files) < maxFiles {
		maxFiles = len(files)
	}

	if maxFiles == 0 {
		return &FileGroup{}, nil
	}

	filesToArchive := make([]string, maxFiles)
	filesToDelete := make([]string, maxFiles)

	for i := 0; i < maxFiles; i++ {
		fmt.Printf("Processing file: %s\n", files[i])
		filesToArchive[i] = filepath.Base(files[i])  // ONLY basename for archive!
		
		// Convert to absolute path for reliable deletion/moving
		absPath, err := filepath.Abs(files[i])
		if err != nil {
			absPath = files[i] // fallback to original if abs fails
		}
		filesToDelete[i] = absPath                    // Absolute path for deletion
	}

	return &FileGroup{
		FilesToArchive: filesToArchive,
		FilesToDelete:  filesToDelete,
	}, nil
}

// moveImages matches Python _moveImages method with retry logic
func (ac *AstroCam) moveImages(files []string) error {
	const maxRetries = 2
	const retryDelay = 3 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		allSuccess := true
		var failedFiles []string

		for _, file := range files {
			basename := filepath.Base(file)
			targetPath := filepath.Join(ac.config.ProcessedDirectory, basename)

			// Check if target file already exists
			if _, err := os.Stat(targetPath); err == nil {
				// Target exists, delete source file
				if err := os.Remove(file); err != nil {
					fmt.Printf("Error: Cannot delete file %s (attempt %d/%d): %v\n", 
						filepath.Base(file), attempt, maxRetries, err)
					failedFiles = append(failedFiles, file)
					allSuccess = false
				}
			} else {
				// Target doesn't exist, move file
				if err := os.Rename(file, targetPath); err != nil {
					fmt.Printf("Error: Cannot move file %s (attempt %d/%d): %v\n", 
						filepath.Base(file), attempt, maxRetries, err)
					failedFiles = append(failedFiles, file)
					allSuccess = false
				}
			}
		}

		if allSuccess {
			return nil // All files moved successfully
		}

		// If this was the last attempt, handle failure
		if attempt == maxRetries {
			if ac.testMode {
				// In test mode, exit with error
				fmt.Printf("FATAL ERROR (Test Mode): Failed to move %d files after %d attempts:\n", 
					len(failedFiles), maxRetries)
				for _, file := range failedFiles {
					fmt.Printf("  - %s\n", filepath.Base(file))
				}
				os.Exit(1)
			} else {
				// In normal mode, log error but continue
				fmt.Printf("WARNING: Failed to move %d files after %d attempts. Files remain in camera directory:\n", 
					len(failedFiles), maxRetries)
				for _, file := range failedFiles {
					fmt.Printf("  - %s\n", filepath.Base(file))
				}
				fmt.Printf("Archive was uploaded successfully. New files with different names will be processed normally.\n")
				return nil // Return success to avoid re-uploading archive
			}
		}

		// Wait before retry
		fmt.Printf("Waiting %v before retry...\n", retryDelay)
		time.Sleep(retryDelay)
		files = failedFiles // Only retry the files that failed
	}

	return nil // This should never be reached due to the logic above
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
		
		buffer := make([]byte, 1024)
		_, err = rc.Read(buffer)
		rc.Close()
		
		if err != nil && err != io.EOF {
			return fmt.Errorf("failed to read file %s in archive: %w", file.Name, err)
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
	if ac.useRAR {
		return ac.testRARArchive(archiveFileName)
	} else {
		return ac.testZipArchive(archiveFileName)
	}
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

// packImagesForArea matches Python packImagesForArea method
func (ac *AstroCam) packImagesForArea(area string) (string, error) {
	originalDir, _ := os.Getwd()
	defer os.Chdir(originalDir)

	fileGroup, err := ac.getImageFiles(area)
	if err != nil {
		return ERROR, err
	}

	if len(fileGroup.FilesToArchive) == 0 {
		return EMPTY, nil
	}
	
	// Wait for files to complete writing (just in case)
	fmt.Printf("Found %d files for area %s, waiting 5 seconds for writes to complete...\n", 
		len(fileGroup.FilesToArchive), area)
	time.Sleep(5 * time.Second)

	// Create archive filename: YYYY-MM-DD_[PREFIX]AREA_HHMMSS[POSTFIX].ext
	now := time.Now()
	dateStr := now.Format("2006-01-02")
	timeStr := now.Format("150405")
	
	archiveFileName := filepath.Join(ac.tempDirectory, 
		fmt.Sprintf("%s_%s%s_%s%s%s", 
			dateStr, ac.config.Prefix, area, timeStr, ac.config.Postfix, ac.archiveExt))

	// Change to camera directory
	if err := os.Chdir(ac.config.CameraDirectory); err != nil {
		if ac.testMode {
			fmt.Printf("FATAL ERROR (Test Mode): Cannot change to camera directory: %v\n", err)
			os.Exit(1)
		}
		return ERROR, fmt.Errorf("could not change to camera directory: %w", err)
	}

	// Create archive
	var archiveTypeStr string
	if ac.useRAR {
		archiveTypeStr = "RAR"
	} else if ac.zipCompressed {
		archiveTypeStr = "ZIP"
	} else {
		archiveTypeStr = "ZIP (uncompressed)"
	}
	
	fmt.Printf("Creating %s archive: %s\n", archiveTypeStr, filepath.Base(archiveFileName))
	
	if err := ac.createArchive(archiveFileName, fileGroup.FilesToArchive); err != nil {
		if ac.testMode {
			fmt.Printf("FATAL ERROR (Test Mode): Archive creation failed: %v\n", err)
			os.Exit(1)
		}
		return ERROR, fmt.Errorf("failed to create archive: %w", err)
	}

	// Test archive integrity
	if err := ac.testArchive(archiveFileName); err != nil {
		fmt.Printf("Warning: Archive integrity test failed: %v\n", err)
		if ac.testMode {
			fmt.Printf("FATAL ERROR (Test Mode): Archive integrity test failed\n")
			os.Exit(1)
		}
		return ERROR, err
	}

	// Change back to original directory before moving files
	if err := os.Chdir(originalDir); err != nil {
		if ac.testMode {
			fmt.Printf("FATAL ERROR (Test Mode): Cannot change back to original directory: %v\n", err)
			os.Exit(1)
		}
		return ERROR, fmt.Errorf("could not change back to original directory: %w", err)
	}

	// Move processed images
	if err := ac.moveImages(fileGroup.FilesToDelete); err != nil {
		return ERROR, fmt.Errorf("failed to move images: %w", err)
	}

	return archiveFileName, nil
}

// hasCredentials checks if username and password are provided
func (ac *AstroCam) hasCredentials() bool {
	return ac.config.Username != "" && ac.config.Password != ""
}

// uploadFile matches FileUploader functionality with proper resource management
func (ac *AstroCam) uploadFile(filePath string) error {
	// Wait for upload throttling (120 seconds between uploads)
	ac.waitForUploadThrottle()
	
	fmt.Printf("Uploading to server: %s\n", filepath.Base(filePath))

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
	req, err := http.NewRequest("POST", ac.config.Server, &body)
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
		if ac.testMode {
			fmt.Printf("FATAL ERROR (Test Mode): Upload failed: %v\n", err)
			os.Exit(1)
		}
		return fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()

	// Check response
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fmt.Printf("Successfully uploaded: %s\n", filepath.Base(filePath))
		return nil
	}

	uploadErr := fmt.Errorf("server returned status %d: %s", resp.StatusCode, resp.Status)
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

// makeJobForArchive matches Python makeJobForArchive function
func (ac *AstroCam) makeJobForArchive(archiveFile string) {
	if err := ac.uploadFile(archiveFile); err != nil {
		fmt.Printf("Upload error: %v\n", err)
		return
	}

	if err := ac.deleteFile(archiveFile); err != nil {
		fmt.Printf("Warning: Error deleting file after upload: %v\n", err)
	}
}

// makeJobForArchives matches Python makeJobForArchives function
func (ac *AstroCam) makeJobForArchives() {
	archiveFiles, err := ac.getArchiveFiles()
	if err != nil {
		fmt.Printf("Error scanning archive files: %v\n", err)
		return
	}

	for _, archiveFile := range archiveFiles {
		fmt.Printf("Found existing archive: %s\n", filepath.Base(archiveFile))
		ac.makeJobForArchive(archiveFile)
	}
}

// makeJobForArea matches Python makeJobForArea function  
func (ac *AstroCam) makeJobForArea(area string) {
	archiveFile, err := ac.packImagesForArea(area)
	if err != nil {
		fmt.Printf("Error processing area %s: %v\n", area, err)
		return
	}

	if archiveFile == ERROR {
		fmt.Printf("Error: Archive creation failed for area %s\n", area)
		return
	}

	if archiveFile == EMPTY {
		return
	}

	fmt.Printf("Archive created: %s\n", filepath.Base(archiveFile))
	ac.makeJobForArchive(archiveFile)
}

// makeJobForAreas matches Python makeJobForAreas function
func (ac *AstroCam) makeJobForAreas() {
	hasNewFiles := false
	
	for _, area := range ac.areas {
		// Check if area has files without processing them - use determined extension
		files, err := ac.fileBrowser(area, ac.config.CameraDirectory, ac.fitsExt)
		if err != nil {
			continue
		}
		
		// Debug output to help troubleshooting
		if len(files) > 0 {
			fmt.Printf("INFO: Area '%s' has %d files (need %d)\n", area, len(files), ac.config.Count)
		}
		
		if len(files) >= ac.config.Count {
			hasNewFiles = true
			ac.makeJobForArea(area)
		}
	}
	
	// In test mode, track if we've found files yet
	if ac.testMode && hasNewFiles {
		ac.testStartTime = time.Now() // Reset timeout when we find files
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
	fmt.Printf("Scanning temp directory... %s\n", time.Now().Format("2006-01-02 15:04:05"))
	ac.makeJobForArchives()
	
	fmt.Printf("Scanning camera directory... %s\n", time.Now().Format("2006-01-02 15:04:05"))
	ac.makeJobForAreas()
	
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
	fmt.Printf("  FITS file extension: %s\n", ac.fitsExt)
	
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

	app, err := NewAstroCam(*testMode)
	if err != nil {
		log.Fatalf("Initialization failed: %v", err)
	}

	app.run()
}
