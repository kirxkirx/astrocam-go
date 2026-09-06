package main

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// fitsFile writes a file that passes the completeness test: a whole number of
// FITS records, starting with the SIMPLE keyword.
func fitsFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	buf := make([]byte, fitsBlockSize)
	for i := range buf {
		buf[i] = ' '
	}
	// A minimal but valid primary header: the completeness check reads it to
	// work out how long the file has to be.
	for i, card := range []string{
		"SIMPLE  =                    T",
		"BITPIX  =                    8",
		"NAXIS   =                    0",
		"END",
	} {
		copy(buf[i*80:], []byte(card))
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// testCam builds an AstroCam wired up the way NewAstroCam would, without the
// filesystem layout the real constructor expects.
func testCam(t *testing.T, cfg *Config, areas []string) *AstroCam {
	t.Helper()
	ac := &AstroCam{
		config:     cfg,
		areas:      areas,
		archiveExt: ".zip",
		stamps:     make(map[string]fileStamp),
		rejects:    make(map[string]int),
	}
	for _, area := range areas {
		ac.areaPatterns = append(ac.areaPatterns,
			regexp.MustCompile("^"+regexp.QuoteMeta(area)+"(_|-SF_).*"+fitsExtensionPattern+"$"))
	}
	return ac
}

func names(cs []candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.name)
	}
	return out
}

func TestParseFrameTime(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"Ser-06-Q2b1x1_2026-08-30_23-06-45_20.00sec_-0.00C_LIGHT_0152.fits", "2026-08-30 23:06:45", true},
		{"064_2025-6-28_21-23-59_001.fts", "2025-06-28 21:23:59", true},
		{"064-SF_2025-12-01_00-00-02_002.fits", "2025-12-01 00:00:02", true},
		{"no_timestamp_here.fits", "", false},
	}
	for _, c := range cases {
		got, ok := parseFrameTime(c.name)
		if ok != c.ok {
			t.Errorf("parseFrameTime(%q) ok=%v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && got.Format("2006-01-02 15:04:05") != c.want {
			t.Errorf("parseFrameTime(%q) = %s, want %s", c.name, got.Format("2006-01-02 15:04:05"), c.want)
		}
	}
}

// A pair whose two frames land on either side of a date boundary is still one
// visit and must still be packable. This is the case the grouping rule exists
// for.
func TestSelectVisitAcrossMidnight(t *testing.T) {
	mk := func(name string) candidate {
		when, ok := parseFrameTime(name)
		if !ok {
			t.Fatalf("cannot parse %s", name)
		}
		return candidate{path: "/x/" + name, name: name, when: when}
	}
	cands := []candidate{
		mk("Cep-01_2026-09-04_00-00-12_0002.fits"),
		mk("Cep-01_2026-09-03_23-59-50_0001.fits"),
	}
	got := selectVisit(cands, 2, visitGap)
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
	if got[0].name != "Cep-01_2026-09-03_23-59-50_0001.fits" {
		t.Errorf("frames out of order, first is %s", got[0].name)
	}
}

// Frames of two separate visits to the same field must never share an archive,
// even though together they satisfy the frame count.
func TestSelectVisitNeverMixesVisits(t *testing.T) {
	mk := func(name string) candidate {
		when, _ := parseFrameTime(name)
		return candidate{path: "/x/" + name, name: name, when: when}
	}
	cands := []candidate{
		mk("Aql-01_2026-03-27_04-02-00_0001.fits"),
		mk("Aql-01_2026-03-27_05-43-00_0002.fits"),
		mk("Aql-01_2026-03-27_05-43-40_0003.fits"),
	}
	got := selectVisit(cands, 2, visitGap)
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
	for _, c := range got {
		if c.name == "Aql-01_2026-03-27_04-02-00_0001.fits" {
			t.Fatalf("the 04:02 leftover was mixed into the 05:43 visit: %v", names(got))
		}
	}
	if got := selectVisit(cands[:1], 2, visitGap); got != nil {
		t.Errorf("a single frame was packed as a set: %v", names(got))
	}
}

func TestSplitDatedPath(t *testing.T) {
	cases := []struct {
		path       string
		wantBase   string
		wantSuffix []string
		wantOK     bool
	}{
		{filepath.Join("/data", "2026-08-16", "LIGHT"), filepath.Clean("/data"), []string{"LIGHT"}, true},
		{filepath.Join("/data", "sub", "2026-08-16", "LIGHT"), filepath.Join("/data", "sub"), []string{"LIGHT"}, true},
		{filepath.Join("/data", "2026-08-16", "M31", "LIGHT"), filepath.Clean("/data"), []string{"M31", "LIGHT"}, true},
		{filepath.Join("/data", "2026-08-16"), "", nil, false},
		{filepath.Join("/home", "user", "camera", "input"), "", nil, false},
		{filepath.Join("/data", "2026-13-40", "LIGHT"), "", nil, false},
	}
	for _, c := range cases {
		base, suffix, ok := splitDatedPath(c.path)
		if ok != c.wantOK {
			t.Errorf("splitDatedPath(%q) ok=%v, want %v", c.path, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if base != c.wantBase || len(suffix) != len(c.wantSuffix) {
			t.Errorf("splitDatedPath(%q) = (%q, %v), want (%q, %v)", c.path, base, suffix, c.wantBase, c.wantSuffix)
			continue
		}
		for i := range suffix {
			if suffix[i] != c.wantSuffix[i] {
				t.Errorf("splitDatedPath(%q) suffix = %v, want %v", c.path, suffix, c.wantSuffix)
				break
			}
		}
	}
}

// The regression test for the loss this change exists to stop: a complete set
// in a previous night's folder must still be found after the name of "the
// current night" has moved on, with nothing on disk having changed.
func TestScanDirsKeepsPreviousNights(t *testing.T) {
	base := t.TempDir()
	configured := filepath.Join(base, "2026-08-15", "LIGHT")
	if err := os.MkdirAll(configured, 0755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(base, "2026-08-16", "LIGHT")
	fitsFile(t, orphan, "Cep-01_2026-08-16_23-30-00_0001.fits")
	fitsFile(t, orphan, "Cep-01_2026-08-16_23-30-40_0002.fits")
	if err := os.MkdirAll(filepath.Join(base, "2026-08-17", "LIGHT"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "notadate", "LIGHT"), 0755); err != nil {
		t.Fatal(err)
	}

	ac := testCam(t, &Config{CameraDirectory: configured, Count: 2}, []string{"Cep-01"})
	dirs := ac.scanDirs()

	found := false
	for _, d := range dirs {
		if d == filepath.Clean(orphan) {
			found = true
		}
		if d == filepath.Clean(filepath.Join(base, "notadate", "LIGHT")) {
			t.Errorf("a folder whose name is not a date was scanned: %s", d)
		}
	}
	if !found {
		t.Fatalf("the previous night's folder %s is not in the scan set %v", orphan, dirs)
	}
}

// Two spellings of the same configured directory must produce one entry.
func TestScanDirsDeduplicates(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "2026-08-16", "LIGHT")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}

	ac := testCam(t, &Config{CameraDirectory: real + string(filepath.Separator), Count: 2}, []string{"Cep-01"})
	dirs := ac.scanDirs()

	seen := 0
	for _, d := range dirs {
		if d == filepath.Clean(real) {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the configured directory appears %d times in %v, want once", seen, dirs)
	}
}

// A file is only a candidate once it has stopped changing, so a frame that is
// still being written is never packed.
func TestCollectCandidatesWaitsForAStableFile(t *testing.T) {
	dir := t.TempDir()
	processed := t.TempDir()
	fitsFile(t, dir, "Cep-01_2026-08-16_23-30-00_0001.fits")

	ac := testCam(t, &Config{CameraDirectory: dir, ProcessedDirectory: processed, Count: 2}, []string{"Cep-01"})

	stamps := make(map[string]fileStamp)
	if got := ac.collectCandidates(dir, stamps); len(got["Cep-01"]) != 0 {
		t.Fatalf("a file was accepted on the cycle it first appeared")
	}
	ac.stamps = stamps

	if got := ac.collectCandidates(dir, make(map[string]fileStamp)); len(got["Cep-01"]) != 1 {
		t.Fatalf("an unchanged file was not accepted on the next cycle")
	}
}

// A frame whose name is already in the processed directory has been delivered
// once and must never be packed again.
func TestCollectCandidatesSkipsAlreadyProcessed(t *testing.T) {
	dir := t.TempDir()
	processed := t.TempDir()
	name := "Cep-01_2026-08-16_23-30-00_0001.fits"
	fitsFile(t, dir, name)
	fitsFile(t, processed, name)

	ac := testCam(t, &Config{CameraDirectory: dir, ProcessedDirectory: processed, Count: 2}, []string{"Cep-01"})
	stamps := make(map[string]fileStamp)
	ac.collectCandidates(dir, stamps)
	ac.stamps = stamps

	if got := ac.collectCandidates(dir, make(map[string]fileStamp)); len(got["Cep-01"]) != 0 {
		t.Errorf("a frame already in the processed directory was offered for packing")
	}
}

func TestIsCompleteFITS(t *testing.T) {
	dir := t.TempDir()
	good := fitsFile(t, dir, "good.fits")
	if info, _ := os.Stat(good); !complete3(good, info.Size()) {
		t.Errorf("a whole FITS file was rejected")
	}

	short := filepath.Join(dir, "short.fits")
	if err := os.WriteFile(short, []byte("SIMPLE  = T"), 0644); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(short); complete3(short, info.Size()) {
		t.Errorf("a half-written file was accepted")
	}

	notFits := filepath.Join(dir, "other.fits")
	if err := os.WriteFile(notFits, make([]byte, fitsBlockSize), 0644); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(notFits); complete3(notFits, info.Size()) {
		t.Errorf("a file that does not start with SIMPLE was accepted")
	}
}

func TestDistinctNames(t *testing.T) {
	a := candidate{name: "x.fits"}
	b := candidate{name: "y.fits"}
	if !distinctNames([]candidate{a, b}) {
		t.Errorf("two different names were reported as clashing")
	}
	if distinctNames([]candidate{a, a}) {
		t.Errorf("a duplicate file name was not detected")
	}
}

// A backlog packed as .rar must still be delivered by a run now using zip.
func TestGetArchiveFilesListsBothFormats(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"2026-08-16_A_230000.rar", "2026-08-16_A_231000.zip", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "incoming"), 0755); err != nil {
		t.Fatal(err)
	}

	ac := testCam(t, &Config{Count: 2}, nil)
	ac.tempDirectory = dir

	got, err := ac.getArchiveFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want the .rar and the .zip and nothing else", got)
	}
	if filepath.Base(got[0]) != "2026-08-16_A_230000.rar" {
		t.Errorf("archives are not oldest first: %v", got)
	}

	ac.rejects[got[0]] = 3
	got, _ = ac.getArchiveFiles()
	if filepath.Base(got[0]) != "2026-08-16_A_231000.zip" {
		t.Errorf("a repeatedly refused archive still blocks the queue: %v", got)
	}
}

func TestUploadServerCandidates(t *testing.T) {
	ac := &AstroCam{config: &Config{Server: "https://primary/upload.py"}}
	if got := ac.uploadServerCandidates(); len(got) != 1 || got[0] != "https://primary/upload.py" {
		t.Errorf("no fallback: got %v", got)
	}
	ac = &AstroCam{config: &Config{Server: "https://primary/upload.py", FallbackServer: "https://backup/upload.py"}}
	got := ac.uploadServerCandidates()
	if len(got) != 2 || got[0] != "https://primary/upload.py" || got[1] != "https://backup/upload.py" {
		t.Errorf("with fallback: got %v", got)
	}
	ac = &AstroCam{config: &Config{Server: "https://primary/upload.py", FallbackServer: "https://primary/upload.py"}}
	if got := ac.uploadServerCandidates(); len(got) != 1 {
		t.Errorf("duplicate fallback: got %v", got)
	}
}

// The archive name has a resolution of one second, so two archives packed in
// the same second must still get separate names.
func TestArchivePathsAreUnique(t *testing.T) {
	dir := t.TempDir()
	ac := testCam(t, &Config{Count: 2}, nil)
	ac.tempDirectory = dir

	_, first, err := ac.archivePaths("Cep-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, second, err := ac.archivePaths("Cep-01")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Errorf("a second archive would have overwritten %s", filepath.Base(first))
	}
}

func TestSortByArchiveNameStripsEitherExtension(t *testing.T) {
	ac := testCam(t, &Config{Postfix: "_TTU"}, nil)
	if z, r := ac.sortByArchiveName("2026-08-16_Cep-01_230000_TTU.zip"), ac.sortByArchiveName("2026-08-16_Cep-01_230000_TTU.rar"); z != r {
		t.Errorf("the same archive sorts differently by format: %q vs %q", z, r)
	}
	list := []string{
		"2026-08-17_Cep-01_010000_TTU.zip",
		"2026-08-16_Cep-01_230000_TTU.rar",
	}
	sort.Slice(list, func(i, j int) bool { return ac.sortByArchiveName(list[i]) < ac.sortByArchiveName(list[j]) })
	if list[0] != "2026-08-16_Cep-01_230000_TTU.rar" {
		t.Errorf("archives across a date change sort wrongly: %v", list)
	}
}

func TestVisitGapMatchesServerSide(t *testing.T) {
	// The server groups frames into one visit with VISIT_GROUP_MAX_GAP_DAYS =
	// 0.007 days. This side must be no wider than that.
	if visitGap > time.Duration(0.007*24*float64(time.Hour)) {
		t.Errorf("visitGap %v is wider than the server's visit window", visitGap)
	}
}

// A visit whose frames landed in two different night folders must still be
// packed, and the archive must still store bare base names.
func TestPackVisitAcrossTwoFolders(t *testing.T) {
	base := t.TempDir()
	temp := t.TempDir()
	processed := t.TempDir()

	before := filepath.Join(base, "2026-09-03", "LIGHT")
	after := filepath.Join(base, "2026-09-04", "LIGHT")
	p1 := fitsFile(t, before, "Cep-01_2026-09-03_23-59-50_0001.fits")
	p2 := fitsFile(t, after, "Cep-01_2026-09-04_00-00-12_0002.fits")

	ac := testCam(t, &Config{CameraDirectory: before, ProcessedDirectory: processed, Count: 2}, []string{"Cep-01"})
	ac.tempDirectory = temp
	ac.zipCompressed = true

	visit := []candidate{
		{path: p1, name: filepath.Base(p1), when: mustTime(t, p1)},
		{path: p2, name: filepath.Base(p2), when: mustTime(t, p2)},
	}
	archive, err := ac.packVisit("Cep-01", visit)
	if err != nil {
		t.Fatalf("packVisit across two folders failed: %v", err)
	}

	got, err := ac.archiveMemberNames(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("archive holds %v, want two frames", got)
	}
	for _, n := range got {
		if strings.ContainsAny(n, "/\\") {
			t.Errorf("member %q carries a directory part", n)
		}
	}
	for _, p := range []string{p1, p2} {
		if _, err := os.Stat(filepath.Join(processed, filepath.Base(p))); err != nil {
			t.Errorf("%s was not moved to the processed directory", filepath.Base(p))
		}
	}
}

func mustTime(t *testing.T, path string) time.Time {
	t.Helper()
	when, ok := parseFrameTime(filepath.Base(path))
	if !ok {
		t.Fatalf("cannot parse a frame time from %s", path)
	}
	return when
}

// An archive whose contents do not match the frames that were selected must
// never be delivered, because the server deletes nothing on our behalf.
func TestVerifyArchiveMembers(t *testing.T) {
	dir := t.TempDir()
	ac := testCam(t, &Config{Count: 2}, nil)

	path := filepath.Join(dir, "a.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, n := range []string{"one.fits", "sub/two.fits"} {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("x"))
	}
	zw.Close()
	f.Close()

	if err := ac.verifyArchiveMembers(path, []string{"one.fits", "two.fits"}); err == nil {
		t.Errorf("a member stored with a directory part was accepted")
	}
	if err := ac.verifyArchiveMembers(path, []string{"one.fits"}); err == nil {
		t.Errorf("an archive with the wrong number of members was accepted")
	}
}

// A file whose length is a whole number of FITS records can still be cut
// short. The header says how long it must be, so use that.
func TestIsCompleteFITSRejectsAlignedTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cut.fits")

	// A header describing one 2880 byte data record, but with no data written.
	buf := make([]byte, fitsBlockSize)
	for i := range buf {
		buf[i] = ' '
	}
	for i, card := range []string{
		"SIMPLE  =                    T",
		"BITPIX  =                    8",
		"NAXIS   =                    1",
		"NAXIS1  =                 2880",
		"END",
	} {
		copy(buf[i*80:], []byte(card))
	}
	if err := os.WriteFile(path, buf, 0644); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(path)
	if complete3(path, info.Size()) {
		t.Errorf("a file missing its data records was accepted as complete")
	}

	// Append the promised data record; now it is complete.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(make([]byte, fitsBlockSize))
	f.Close()

	info, _ = os.Stat(path)
	if !complete3(path, info.Size()) {
		t.Errorf("a complete file was rejected")
	}
}

// The digits 507 inside a server process id must not be read as a full disk.
func TestClassifyServerErrorIgnoresBare507(t *testing.T) {
	body := "UNMW_STATUS:ERROR Error during processing.<br>./wrapper.sh " +
		"uploads/web_upload_2507841QkRtvSlm/2026-09-04_001_115422.zip<br>Exit status 1"
	reason, pause := classifyServerError(body)
	if pause == DISK_SPACE_PAUSE {
		t.Errorf("a process id containing 507 was read as a full disk: %q", reason)
	}
	if uploadErrorIsServerRejection(errors.New(body)) {
		t.Errorf("a generic processing error was treated as a server rejection")
	}

	for _, real := range []string{
		"UNMW_STATUS:ERROR server is out of disk space, only 1000 MB free",
		"server is out of disk space (HTTP 507): Insufficient disk space",
	} {
		if _, pause := classifyServerError(real); pause != DISK_SPACE_PAUSE {
			t.Errorf("a real out-of-disk message was not recognised: %q", real)
		}
	}
}

// complete3 keeps the tests readable now that isCompleteFITS also reports the
// observation time it found.
func complete3(path string, size int64) bool {
	_, _, complete := isCompleteFITS(path, size)
	return complete
}

// The frame time must come from the FITS header when the camera writes one, so
// the grouping does not depend on any particular file naming convention.
func TestResolveFrameTimesPrefersHeader(t *testing.T) {
	base := time.Date(2026, 9, 3, 23, 0, 0, 0, time.UTC)
	cands := []candidate{
		{name: "a.fits", obs: base, hasObs: true, named: base.Add(time.Hour), hasNamed: true, mtime: base.Add(48 * time.Hour)},
		{name: "b.fits", obs: base.Add(time.Minute), hasObs: true, named: base.Add(2 * time.Hour), hasNamed: true, mtime: base.Add(49 * time.Hour)},
	}
	resolveFrameTimes(cands)
	if !cands[0].when.Equal(base) || !cands[1].when.Equal(base.Add(time.Minute)) {
		t.Errorf("DATE-OBS was not used: %v", []time.Time{cands[0].when, cands[1].when})
	}

	// One frame without DATE-OBS drops the whole field to the name, so two
	// different clocks are never compared with each other.
	cands[1].hasObs = false
	resolveFrameTimes(cands)
	if !cands[0].when.Equal(base.Add(time.Hour)) || !cands[1].when.Equal(base.Add(2*time.Hour)) {
		t.Errorf("the field did not fall back to the name time: %v", []time.Time{cands[0].when, cands[1].when})
	}

	// Without either, the modification time is the last resort.
	cands[0].hasNamed, cands[1].hasNamed = false, false
	resolveFrameTimes(cands)
	if !cands[0].when.Equal(base.Add(48 * time.Hour)) {
		t.Errorf("the field did not fall back to mtime: %v", cands[0].when)
	}
}

func TestParseDateObs(t *testing.T) {
	if _, ok := parseDateObs("2026-09-03T23:06:45.123"); !ok {
		t.Errorf("a standard DATE-OBS value was refused")
	}
	if _, ok := parseDateObs("2026-09-03T23:06:45"); !ok {
		t.Errorf("a DATE-OBS value without fractional seconds was refused")
	}
	if _, ok := parseDateObs("2026-09-03"); ok {
		t.Errorf("a date with no time of day was accepted; every frame of a night would share it")
	}
}

// A camera writing .FITS or .FIT in upper case must be handled too.
func TestAreaMatchIgnoresExtensionCase(t *testing.T) {
	ac := testCam(t, &Config{Count: 2}, []string{"Cep-01"})
	for _, name := range []string{
		"Cep-01_2026-09-03_23-10-00.fits",
		"Cep-01_2026-09-03_23-10-00.FITS",
		"Cep-01_2026-09-03_23-10-00.Fit",
	} {
		if ac.areaForFile(name) != "Cep-01" {
			t.Errorf("%s was not recognised as an image of area Cep-01", name)
		}
	}
	if ac.areaForFile("cep-01_2026-09-03_23-10-00.fits") != "" {
		t.Errorf("the area name should stay case-sensitive")
	}
}
