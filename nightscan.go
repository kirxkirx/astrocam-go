package main

// Helpers for finding image files across the dated folders that imaging
// software creates, and for grouping the frames of one field into the visit
// that an archive is allowed to contain.
//
// The rule the rest of the program relies on: whether a frame is eligible must
// be a property of THAT FRAME - it does not match any area, it is not a
// finished FITS file, it was already moved to the processed directory. It must
// never be a property of what time it is, of how the server is feeling, or of
// how old the folder holding it happens to be. A set that was complete and
// unpacked at 11:59 must still be complete and packable at 12:01.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// dateFolderLayout is the name given to a night's folder, e.g. 2026-09-03.
const dateFolderLayout = "2006-01-02"

// candidate is one image file that may be packed into an archive.
//
// Three possible sources for the moment the frame was taken are kept, because
// which of them is available depends on the camera software. resolveFrameTimes
// settles on one source per field before any gap between frames is measured.
type candidate struct {
	path     string    // path as produced by the scan, used to open the file
	name     string    // base name, which is also the name stored in the archive
	when     time.Time // the chosen frame time, filled in by resolveFrameTimes
	obs      time.Time // DATE-OBS from the FITS header
	hasObs   bool
	named    time.Time // a timestamp embedded in the file name
	hasNamed bool
	size     int64
	mtime    time.Time
}

// resolveFrameTimes picks ONE time source for a field and applies it to every
// candidate of that field.
//
// DATE-OBS is preferred because it is a standard FITS keyword that every camera
// writes and it records when the exposure actually started. A timestamp in the
// file name is the next best thing, and the file modification time is the last
// resort. The sources are never mixed: DATE-OBS is UTC while a name or an mtime
// is usually local, so a gap measured between two different sources would be
// meaningless and could split or merge a visit.
func resolveFrameTimes(cands []candidate) {
	useHeader, useName := true, true
	for _, c := range cands {
		if !c.hasObs {
			useHeader = false
		}
		if !c.hasNamed {
			useName = false
		}
	}

	for i := range cands {
		switch {
		case useHeader:
			cands[i].when = cands[i].obs
		case useName:
			cands[i].when = cands[i].named
		default:
			cands[i].when = cands[i].mtime
		}
	}
}

// fileStamp is what a file looked like on the previous scan cycle. A file is
// only considered finished when its size and mtime are unchanged since then.
type fileStamp struct {
	size  int64
	mtime time.Time
	// reported is set once a permanently unusable file has been named in the
	// log, so a truncated or locked frame costs one line rather than one per
	// cycle for the rest of the run. It rides on the size and mtime, so a file
	// that is later completed or replaced is reported again.
	reported bool
}

// frameTimePattern matches the date and time that imaging software embeds in
// the file name. It accepts both the zero-padded form written by NINA
// (2026-09-03_23-06-45) and the unpadded form written by the older camera
// software (2025-6-28_21-23-59).
var frameTimePattern = regexp.MustCompile(`(\d{4})-(\d{1,2})-(\d{1,2})[_T](\d{1,2})-(\d{1,2})-(\d{1,2})`)

// parseFrameTime extracts the moment a frame was taken from its file name.
// The name is used rather than the file mtime because a copy, a restore or a
// virus scanner rewrites every mtime to one instant, which is exactly the
// situation in which frames from two different nights must stay apart.
func parseFrameTime(name string) (time.Time, bool) {
	m := frameTimePattern.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	var n [6]int
	for i := 0; i < 6; i++ {
		v, err := strconv.Atoi(m[i+1])
		if err != nil {
			return time.Time{}, false
		}
		n[i] = v
	}
	if n[1] < 1 || n[1] > 12 || n[2] < 1 || n[2] > 31 {
		return time.Time{}, false
	}
	if n[3] > 23 || n[4] > 59 || n[5] > 59 {
		return time.Time{}, false
	}
	return time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, time.Local), true
}

// isCompleteFITS reports whether a file is a FITS file that has been written
// all the way to the end.
//
// The FITS standard requires every file to start with the SIMPLE keyword and to
// be a whole number of 2880 byte records, and the header states how many pixels
// follow it, so the file's own header says how long it has to be. That is the
// authoritative test for "finished", and it is what replaced the fixed wait
// that used to be applied after spotting a new file: a frame the camera
// software is still writing fails it, however long or short the wait.
func isCompleteFITS(path string, size int64) (obs time.Time, hasObs bool, complete bool) {
	if size < fitsBlockSize || size%fitsBlockSize != 0 {
		return time.Time{}, false, false
	}

	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false, false
	}
	defer f.Close()

	want, obs, hasObs, ok := readFITSHeader(f)
	if !ok {
		return time.Time{}, false, false
	}
	// A file carrying extensions is longer than its primary header demands, so
	// this is a floor rather than an exact length.
	return obs, hasObs, size >= want
}

// readFITSHeader reads the primary header and returns the smallest size the
// file can have (the header records plus the image data they describe, each
// padded to a whole number of 2880 byte records) together with the DATE-OBS
// observation time when the header carries one.
//
// Only keywords defined by the FITS standard are used, so this works for any
// camera that writes FITS rather than for one particular program.
func readFITSHeader(f *os.File) (minSize int64, obs time.Time, hasObs bool, ok bool) {
	const cardLength = 80
	const maxHeaderBlocks = 200 // a real header is a handful of records

	block := make([]byte, fitsBlockSize)
	axisLength := make(map[string]int64, 8)
	bitpix, naxis := int64(0), int64(-1)
	haveBitpix := false

	for blocks := int64(1); blocks <= maxHeaderBlocks; blocks++ {
		if _, err := io.ReadFull(f, block); err != nil {
			return 0, time.Time{}, false, false
		}
		if blocks == 1 && string(block[:6]) != "SIMPLE" {
			return 0, time.Time{}, false, false
		}

		for i := 0; i+cardLength <= fitsBlockSize; i += cardLength {
			card := block[i : i+cardLength]
			key := strings.TrimSpace(string(card[:8]))

			if key == "END" {
				if !haveBitpix || naxis < 0 {
					return 0, time.Time{}, false, false
				}
				pixels := int64(1)
				for axis := int64(1); axis <= naxis; axis++ {
					n, present := axisLength[fmt.Sprintf("NAXIS%d", axis)]
					if !present || n < 0 {
						return 0, time.Time{}, false, false
					}
					pixels *= n
				}
				data := int64(0)
				if naxis > 0 {
					data = (absInt64(bitpix) / 8) * pixels
				}
				dataBlocks := (data + fitsBlockSize - 1) / fitsBlockSize
				return (blocks + dataBlocks) * fitsBlockSize, obs, hasObs, true
			}

			if key == "DATE-OBS" {
				if t, good := parseDateObs(headerString(card)); good {
					obs, hasObs = t, true
				}
				continue
			}

			value, good := headerInt(card)
			if !good {
				continue
			}
			switch {
			case key == "BITPIX":
				bitpix, haveBitpix = value, true
			case key == "NAXIS":
				naxis = value
			case strings.HasPrefix(key, "NAXIS"):
				axisLength[key] = value
			}
		}
	}
	return 0, time.Time{}, false, false
}

// headerString reads the quoted string value of one FITS header card.
func headerString(card []byte) string {
	text := string(card)
	first := strings.IndexByte(text, '\'')
	if first < 0 {
		return ""
	}
	rest := text[first+1:]
	last := strings.IndexByte(rest, '\'')
	if last < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:last])
}

// parseDateObs reads a FITS DATE-OBS value. The standard form carries a time of
// day; a value that is only a date is refused, because every frame of a night
// would then share one timestamp and the frames of separate visits could not be
// told apart.
func parseDateObs(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// headerInt reads the integer value of one 80 character FITS header card.
func headerInt(card []byte) (int64, bool) {
	if len(card) < 11 || card[8] != '=' {
		return 0, false
	}
	value := string(card[10:])
	if comment := strings.IndexByte(value, '/'); comment >= 0 {
		value = value[:comment]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// selectVisit picks the frames of ONE visit to one field.
//
// Candidates are ordered by frame time and cut wherever two consecutive frames
// are further apart than visitGap; the oldest group that holds at least count
// frames wins, and exactly count frames are taken from inside that group. An
// archive therefore never mixes two visits, which is what the server side
// assumes when it checks the frames of a visit against each other.
//
// Returns nil when no group is complete yet. A group that stays below count
// forever is a short visit and is deliberately left alone.
func selectVisit(cands []candidate, count int, visitGap time.Duration) []candidate {
	if count <= 0 || len(cands) < count {
		return nil
	}

	sorted := make([]candidate, len(cands))
	copy(sorted, cands)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].when.Equal(sorted[j].when) {
			return sorted[i].name < sorted[j].name
		}
		return sorted[i].when.Before(sorted[j].when)
	})

	start := 0
	for i := 1; i <= len(sorted); i++ {
		if i == len(sorted) || sorted[i].when.Sub(sorted[i-1].when) > visitGap {
			if i-start >= count {
				return sorted[start : start+count]
			}
			start = i
		}
	}
	return nil
}

// framesNotIn returns the frames that are not part of the given visit.
func framesNotIn(frames, visit []candidate) []candidate {
	taken := make(map[string]bool, len(visit))
	for _, c := range visit {
		taken[c.path] = true
	}
	var rest []candidate
	for _, c := range frames {
		if !taken[c.path] {
			rest = append(rest, c)
		}
	}
	return rest
}

// newestFrames looks at everything a field still has in the camera folders,
// ready or not, and returns when the most recent file was written and how many
// files were written within gap of it.
//
// File times are used here, not the frame times that decide what may share an
// archive, because the question is a different one: not "do these frames
// belong together" but "is the camera still at work on this field". Nothing is
// ever packed, or held back, on the strength of this answer; it only decides
// what the operator is told.
func newestFrames(ready []candidate, pending []pendingFrame, gap time.Duration) (newest time.Time, recent int) {
	times := make([]time.Time, 0, len(ready)+len(pending))
	for _, c := range ready {
		times = append(times, c.mtime)
	}
	for _, p := range pending {
		times = append(times, p.mtime)
	}
	for _, t := range times {
		if t.After(newest) {
			newest = t
		}
	}
	for _, t := range times {
		if newest.Sub(t) <= gap {
			recent++
		}
	}
	return newest, recent
}

// describeNoSet explains why the images a field has are not being combined
// into an archive. It is only called for frames among which selectVisit found
// no complete visit, and it returns lines that fit an 80 column console.
//
// "Has 2 images, needs 2" with nothing uploaded is the most baffling thing
// this program can show, and the two cases behind it call for different words:
// either the visit ended before all its images had arrived, or the images are
// there but were taken too far apart to be one visit. Mixing two visits in one
// archive is the one thing the server cannot tolerate, so the second case is
// deliberate - but it has to be said, not left for the operator to work out.
func describeNoSet(area string, frames []candidate, count int, gap time.Duration) []string {
	n := len(frames)
	if n == 0 {
		return nil
	}
	sorted := make([]candidate, n)
	copy(sorted, frames)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].when.Before(sorted[j].when) })

	// The size of the most recent visit: the frames at the end of the list
	// with no more than gap between one and the next.
	last := 1
	for i := n - 1; i > 0 && sorted[i].when.Sub(sorted[i-1].when) <= gap; i-- {
		last++
	}
	limit := formatPauseDuration(gap)

	var sentences []string
	switch {
	case last == n:
		stay := "the image stays where it is"
		if n > 1 {
			stay = "the images stay where they are"
		}
		sentences = []string{
			fmt.Sprintf("only %d of %d images arrived, and %s have passed.", n, count, limit),
			fmt.Sprintf("The set is NOT complete: nothing is uploaded, %s.", stay),
		}
	case n == 2:
		sentences = []string{
			fmt.Sprintf("its 2 images were taken %s apart.", roughDuration(sorted[1].when.Sub(sorted[0].when))),
			fmt.Sprintf("More than %s means two separate visits: they are NOT combined.", limit),
		}
	default:
		sentences = []string{
			fmt.Sprintf("%d images, but no %d of them within %s of each other.", n, count, limit),
			"They are from separate visits and are NOT combined.",
		}
	}
	// The name of a field can be any length, so the text is set into lines
	// here rather than written out with fixed line breaks.
	return flowLines("Field "+area+": ", "  ", sentences)
}

// roughDuration renders the time between two images the way it would be said
// aloud: in minutes below two hours, in hours below two days, in days beyond.
// It rounds up, so a gap that is too long never reads as exactly the limit.
func roughDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	up := func(unit time.Duration) int { return int((d + unit - 1) / unit) }
	switch {
	case d < 2*time.Hour:
		return plural(up(time.Minute), "minute", "minutes")
	case d < 48*time.Hour:
		return plural(up(time.Hour), "hour", "hours")
	}
	return plural(up(24*time.Hour), "day", "days")
}

// distinctNames reports whether every candidate has a different base name.
// Both zip and rar store two members under the same name without complaining,
// and the server counts members rather than distinct names, so such an archive
// is reported as delivered and then quietly arrives one frame short.
func distinctNames(cands []candidate) bool {
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		if seen[c.name] {
			return false
		}
		seen[c.name] = true
	}
	return true
}

// splitDatedPath finds the deepest component of a path that is a date and
// returns what lies above it and what lies below it. For the usual NINA layout
// .../Images/2026-09-03/LIGHT this gives base=.../Images and suffix=[LIGHT],
// which is what lets us enumerate the other nights next to it.
//
// A path whose last component is the date itself has no suffix; such a folder
// normally holds calibration frames alongside the lights, so it is reported as
// not a dated layout and only the configured folder is scanned.
func splitDatedPath(path string) (base string, suffix []string, ok bool) {
	clean := filepath.Clean(path)
	var tail []string

	for {
		dir, last := filepath.Split(clean)
		if last == "" {
			return "", nil, false
		}
		if _, err := time.Parse(dateFolderLayout, last); err == nil {
			if len(tail) == 0 {
				return "", nil, false
			}
			return filepath.Clean(dir), tail, true
		}
		tail = append([]string{last}, tail...)

		parent := filepath.Clean(dir)
		if parent == clean {
			return "", nil, false
		}
		clean = parent
	}
}

// resolveChild returns the real path of a subdirectory, matching the name
// case-insensitively when an exact match does not exist. The name found on
// disk is used rather than the configured spelling, so a folder is never
// invented on a case-sensitive filesystem and never scanned twice under two
// spellings on Windows.
func resolveChild(dir, name string) (string, bool) {
	exact := filepath.Join(dir, name)
	if st, err := os.Stat(exact); err == nil && st.IsDir() {
		return exact, true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() && strings.EqualFold(e.Name(), name) {
			return filepath.Join(dir, e.Name()), true
		}
	}
	return "", false
}

// appendUniqueDir adds a directory unless the same one is already present.
// Comparison is by cleaned path, and additionally by os.SameFile so that a
// mapped drive and a UNC path to the same folder are not scanned twice.
func appendUniqueDir(dirs []string, add string) []string {
	clean := filepath.Clean(add)
	addInfo, addErr := os.Stat(clean)
	for _, d := range dirs {
		if filepath.Clean(d) == clean {
			return dirs
		}
		if addErr == nil {
			if info, err := os.Stat(d); err == nil && os.SameFile(info, addInfo) {
				return dirs
			}
		}
	}
	return append(dirs, clean)
}
