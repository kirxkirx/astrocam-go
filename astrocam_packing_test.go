package main

// Tests for what happens on this computer between a complete set of images and
// an upload: moving the images into a processed folder on another drive, and
// the standing problems that keep a packing failure, or an upload that never
// finishes, on the screen instead of in a line that scrolls away.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failRenames makes every move of an image into the processed folder fail with
// err, for the length of the test.
func failRenames(t *testing.T, err error) {
	t.Helper()
	orig := renameImage
	renameImage = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	t.Cleanup(func() { renameImage = orig })
}

func noMoveRetryDelay(t *testing.T) {
	t.Helper()
	orig := moveRetryDelay
	moveRetryDelay = 0
	t.Cleanup(func() { moveRetryDelay = orig })
}

func pairVisit(t *testing.T, paths ...string) []candidate {
	t.Helper()
	var visit []candidate
	for _, p := range paths {
		visit = append(visit, candidate{path: p, name: filepath.Base(p), when: mustTime(t, p)})
	}
	return visit
}

// A processed folder on another drive than the camera folder cannot be reached
// with a rename. That used to fail every move, throw every archive away and
// upload nothing at all. Now the images are copied there and the originals
// deleted, and the archive goes out.
func TestProcessedFolderOnAnotherDrive(t *testing.T) {
	failRenames(t, errCrossDevice)
	camera, processed := t.TempDir(), t.TempDir()
	p1 := fitsFile(t, camera, "Cep-01_2026-11-22_23-10-00_0001.fits")
	p2 := fitsFile(t, camera, "Cep-01_2026-11-22_23-10-40_0002.fits")
	taken := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p1, taken, taken); err != nil {
		t.Fatal(err)
	}

	ac := testCam(t, &Config{CameraDirectory: camera, ProcessedDirectory: processed, Count: 2}, []string{"Cep-01"})
	ac.tempDirectory = t.TempDir()
	ac.zipCompressed = true

	var archive string
	var err error
	out := captureStdout(t, func() { archive, err = ac.packVisit("Cep-01", pairVisit(t, p1, p2)) })
	if err != nil {
		t.Fatalf("packing into a processed folder on another drive failed: %v\n%s", err, out)
	}
	if !exists(archive) {
		t.Fatalf("the archive %s was not kept for upload", archive)
	}
	for _, p := range []string{p1, p2} {
		if exists(p) {
			t.Errorf("%s is still in the camera folder", filepath.Base(p))
		}
		data, err := os.ReadFile(filepath.Join(processed, filepath.Base(p)))
		if err != nil || len(data) != fitsBlockSize {
			t.Errorf("%s did not arrive complete in the processed folder: %d bytes, %v", filepath.Base(p), len(data), err)
		}
	}
	if info, err := os.Stat(filepath.Join(processed, filepath.Base(p1))); err != nil || !info.ModTime().Equal(taken) {
		t.Errorf("the copy lost the time the image was taken: %v (%v)", info.ModTime(), err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(filepath.Join(processed, filepath.Base(p2))); err != nil || info.Mode().Perm() != 0644 {
			t.Errorf("the copy did not keep the permissions of the image: %v (%v)", info.Mode().Perm(), err)
		}
	}
	entries, _ := os.ReadDir(processed)
	if len(entries) != 2 {
		t.Errorf("the processed folder holds %d files, want the two images and no leftover copy", len(entries))
	}
	if strings.Contains(out, "Error") || strings.Contains(out, "Warning") {
		t.Errorf("a move that worked was reported as a problem:\n%s", out)
	}
}

// When the copy got through but the original could not be deleted, the image
// is still safely in the processed folder, and its name there keeps it from
// being packed again. The archive must go out: throwing it away would leave a
// visit that is never sent.
func TestCopiedImageWhoseOriginalStaysIsDelivered(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder that this user cannot delete files from")
	}
	failRenames(t, errCrossDevice)
	noMoveRetryDelay(t)
	camera, processed := t.TempDir(), t.TempDir()
	p1 := fitsFile(t, camera, "Cep-01_2026-11-22_23-10-00_0001.fits")
	p2 := fitsFile(t, camera, "Cep-01_2026-11-22_23-10-40_0002.fits")
	if err := os.Chmod(camera, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(camera, 0755) })

	ac := testCam(t, &Config{CameraDirectory: camera, ProcessedDirectory: processed, Count: 2}, []string{"Cep-01"})
	ac.tempDirectory = t.TempDir()
	ac.zipCompressed = true

	var archive string
	var err error
	out := captureStdout(t, func() { archive, err = ac.packVisit("Cep-01", pairVisit(t, p1, p2)) })
	if err != nil || !exists(archive) {
		t.Fatalf("the archive was not kept although both images reached the processed folder: %v\n%s", err, out)
	}
	if !strings.Contains(out, "could not be deleted") {
		t.Errorf("the original left in the camera folder was not pointed out:\n%s", out)
	}
	for _, p := range []string{p1, p2} {
		if !exists(filepath.Join(processed, filepath.Base(p))) {
			t.Errorf("%s is missing from the processed folder", filepath.Base(p))
		}
	}
}

// Images that cannot be moved at all - another program holds them, or the
// processed folder cannot be written to - stop every upload of them. That has
// to stand on the screen, and the console must not promise a delivery that
// does not happen.
func TestImagesThatCannotBeMovedAreAStandingProblem(t *testing.T) {
	failRenames(t, os.ErrPermission)
	noMoveRetryDelay(t)
	camera := t.TempDir()
	fitsFile(t, camera, "A-01_2026-11-22_23-10-00_0001.fits")
	fitsFile(t, camera, "A-01_2026-11-22_23-10-40_0002.fits")

	ac := testCam(t, &Config{CameraDirectory: camera, ProcessedDirectory: t.TempDir(), Count: 2}, []string{"A-01"})
	ac.tempDirectory = t.TempDir()
	ac.zipCompressed = true

	captureStdout(t, ac.makeJobForAreas) // first sight of the images: nothing is packed yet
	out := captureStdout(t, ac.makeJobForAreas)

	cond := ac.conditions[conditionPacking]
	if cond == nil || cond.reason != "Packed images cannot be moved into the processed folder" {
		t.Fatalf("want a standing problem about images that cannot be moved, got %+v\n%s", cond, out)
	}
	if !strings.Contains(cond.detail, "A-01") {
		t.Errorf("the problem does not name the field: %s", cond.detail)
	}
	if strings.Contains(out, "will be delivered") || strings.Contains(out, "will be uploaded") {
		t.Errorf("the console promised a delivery that does not happen:\n%s", out)
	}
	if archives, _ := filepath.Glob(filepath.Join(ac.tempDirectory, "*.zip")); len(archives) != 0 {
		t.Errorf("an archive of images that stayed in the camera folder was kept: %v", archives)
	}

	// Once the images can be moved, the next cycle packs them and says so.
	renameImage = os.Rename
	out = captureStdout(t, ac.makeJobForAreas)
	if ac.hasCondition(conditionPacking) || !strings.Contains(out, "RESOLVED") {
		t.Errorf("the problem did not end once the images could be moved:\n%s", out)
	}
}

// An archive that cannot be built - a full disk, a temp folder that cannot be
// written to - is a problem of this computer, and it has to stand on the
// screen too. It ends when there is nothing left to pack.
func TestArchivesThatCannotBeBuiltAreAStandingProblem(t *testing.T) {
	camera := t.TempDir()
	p1 := fitsFile(t, camera, "A-01_2026-11-22_23-10-00_0001.fits")
	p2 := fitsFile(t, camera, "A-01_2026-11-22_23-10-40_0002.fits")

	ac := testCam(t, &Config{CameraDirectory: camera, ProcessedDirectory: t.TempDir(), Count: 2}, []string{"A-01"})
	// A file where the temp folder should be: nothing can be written there.
	blocked := filepath.Join(t.TempDir(), "temp")
	if err := os.WriteFile(blocked, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	ac.tempDirectory = blocked
	ac.zipCompressed = true

	captureStdout(t, ac.makeJobForAreas)
	out := captureStdout(t, ac.makeJobForAreas)
	cond := ac.conditions[conditionPacking]
	if cond == nil || cond.reason != "Images cannot be packed into archives" {
		t.Fatalf("want a standing problem about archives that cannot be built, got %+v\n%s", cond, out)
	}
	if !exists(p1) || !exists(p2) {
		t.Errorf("images of an archive that could not be built left the camera folder")
	}

	os.Remove(p1)
	os.Remove(p2)
	out = captureStdout(t, ac.makeJobForAreas)
	if ac.hasCondition(conditionPacking) || !strings.Contains(out, "RESOLVED") {
		t.Errorf("the problem did not end when nothing was left to pack:\n%s", out)
	}
}

// An upload that runs out of time leaves the server answering its status check
// normally, so "Server OK" stays true. Without a problem of its own the only
// sign would be a queue that keeps growing.
func TestUploadsThatRunOutOfTimeAreAStandingProblem(t *testing.T) {
	orig := uploadTimeout
	uploadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { uploadTimeout = orig })

	var slow atomic.Bool
	slow.Store(true)
	_, ac := startServer(t, say(200, statusOK), func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if slow.Load() {
			time.Sleep(time.Second)
		}
		io.WriteString(w, uploadOK)
	})
	archive := queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	captureStdout(t, ac.uploadPhase)
	cond := ac.conditions[conditionTransfer]
	if cond == nil || !strings.HasPrefix(cond.reason, "Uploads do not finish within") {
		t.Fatalf("want a standing problem about uploads that run out of time, got %+v", cond)
	}
	if ac.hasCondition(conditionDelivery) || ac.hasCondition(conditionArchive) {
		t.Errorf("a slow transfer was blamed on the server or on the archive")
	}
	if !exists(archive) {
		t.Errorf("the archive was not kept after the upload ran out of time")
	}

	slow.Store(false)
	ac.lastUploadTime = time.Time{}
	out := captureStdout(t, ac.uploadPhase)
	if ac.hasCondition(conditionTransfer) || !strings.Contains(out, "RESOLVED") {
		t.Errorf("the problem did not end when an upload went through:\n%s", out)
	}
}

// Uploads that are answered again, even with a refusal, finish in time: the
// problem with the transfer is over, and the refusal is a matter of its own.
func TestTransferProblemEndsWhenUploadsAreAnsweredAgain(t *testing.T) {
	orig := uploadTimeout
	uploadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { uploadTimeout = orig })

	var slow atomic.Bool
	slow.Store(true)
	_, ac := startServer(t, say(200, statusOK), func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if slow.Load() {
			time.Sleep(time.Second)
		}
		io.WriteString(w, "<html><body>UNMW_STATUS:ERROR Error during processing</body></html>")
	})
	queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	captureStdout(t, ac.uploadPhase)
	if !ac.hasCondition(conditionTransfer) {
		t.Fatalf("an upload that ran out of time raised no problem")
	}
	slow.Store(false)
	ac.lastUploadTime = time.Time{}
	out := captureStdout(t, ac.uploadPhase)
	if ac.hasCondition(conditionTransfer) {
		t.Errorf("the transfer problem stayed although the server answers uploads again:\n%s", out)
	}
}

// The reminder line of every standing problem has to fit an 80 column console.
func TestPackingAndTransferRemindersFitTheConsole(t *testing.T) {
	for _, reason := range []string{
		"Images cannot be packed into archives",
		"Packed images cannot be moved into the processed folder",
		"Uploads do not finish within " + formatPauseDuration(900*time.Second),
	} {
		ac := &AstroCam{}
		ac.setCondition(conditionPacking, "ERROR", reason, "", "", time.Time{})
		ac.conditions[conditionPacking].since = time.Now().Add(-(99*time.Hour + 59*time.Minute))
		line := strings.TrimRight(captureStdout(t, func() { ac.printConditionReminder(ac.conditions[conditionPacking]) }), "\n")
		checkWidth(t, []string{line})
	}
}

// A crash in the middle of a move to another drive can leave a copy under its
// temporary name. When the image is in place already, or its original is still
// in the camera folder, the copy is not needed and goes. Otherwise it may be
// the only copy left: a complete one finishes the move, and one that is not
// complete is left for a person - it is never deleted. Nothing else in the
// processed folder is touched.
func TestInterruptedMovesAreFinishedAtStart(t *testing.T) {
	camera, dir := t.TempDir(), t.TempDir()
	partial := func(image string) string {
		return filepath.Join(dir, "."+image+".123456"+partialCopySuffix)
	}
	data, _ := os.ReadFile(fitsFile(t, t.TempDir(), "x.fits"))

	// Complete, the image is not in place and its original is gone: the only
	// copy, so it is put in place.
	os.WriteFile(partial("A-01_2026-11-22_23-10-00_0001.fits"), data, 0644)
	// Cut short, but its original is still in the camera folder.
	fitsFile(t, camera, "A-01_2026-11-22_23-10-40_0002.fits")
	os.WriteFile(partial("A-01_2026-11-22_23-10-40_0002.fits"), data[:1000], 0644)
	// Complete, but the image is in place already.
	os.WriteFile(partial("A-01_2026-11-22_23-11-20_0003.fits"), data, 0644)
	os.WriteFile(filepath.Join(dir, "A-01_2026-11-22_23-11-20_0003.fits"), data, 0644)
	// Cut short, and no original anywhere: maybe the only copy, kept.
	os.WriteFile(partial("A-01_2026-11-22_23-12-00_0004.fits"), data[:1000], 0644)
	keep := []string{".hidden", "notes.partial", ".notes.txt.123.partial", ".A-01.fits.partial"}
	for _, name := range keep {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644)
	}

	ac := testCam(t, &Config{CameraDirectory: camera, ProcessedDirectory: dir, Count: 2}, []string{"A-01"})
	out := captureStdout(t, ac.finishPartialCopies)

	if !exists(filepath.Join(dir, "A-01_2026-11-22_23-10-00_0001.fits")) || exists(partial("A-01_2026-11-22_23-10-00_0001.fits")) {
		t.Errorf("the only, complete copy was not put in place:\n%s", out)
	}
	if exists(partial("A-01_2026-11-22_23-10-40_0002.fits")) || exists(filepath.Join(dir, "A-01_2026-11-22_23-10-40_0002.fits")) {
		t.Errorf("a copy cut short whose original is in the camera folder was not removed, or was put in place:\n%s", out)
	}
	if exists(partial("A-01_2026-11-22_23-11-20_0003.fits")) {
		t.Errorf("a copy of an image that is in place already was left behind")
	}
	if !exists(partial("A-01_2026-11-22_23-12-00_0004.fits")) || !strings.Contains(out, "may be the only copy") {
		t.Errorf("a copy that may be the only one was deleted, or not pointed out:\n%s", out)
	}
	for _, name := range keep {
		if !exists(filepath.Join(dir, name)) {
			t.Errorf("%s was touched", name)
		}
	}
}

// Images that disappeared from the camera folder after they were packed - a
// person or another program took them - cannot be moved either. The archive is
// then their only copy and must be kept, not thrown away as if the images were
// still there to be packed again.
func TestArchiveOfVanishedImagesIsKept(t *testing.T) {
	noMoveRetryDelay(t)
	orig := renameImage
	renameImage = func(oldpath, newpath string) error {
		os.Remove(oldpath)
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: os.ErrNotExist}
	}
	t.Cleanup(func() { renameImage = orig })

	camera := t.TempDir()
	p1 := fitsFile(t, camera, "Cep-01_2026-11-22_23-10-00_0001.fits")
	p2 := fitsFile(t, camera, "Cep-01_2026-11-22_23-10-40_0002.fits")
	ac := testCam(t, &Config{CameraDirectory: camera, ProcessedDirectory: t.TempDir(), Count: 2}, []string{"Cep-01"})
	ac.tempDirectory = t.TempDir()
	ac.zipCompressed = true

	var archive string
	var err error
	out := captureStdout(t, func() { archive, err = ac.packVisit("Cep-01", pairVisit(t, p1, p2)) })
	if err != nil || !exists(archive) {
		t.Fatalf("the only copy of a visit was thrown away: %v\n%s", err, out)
	}
	if !strings.Contains(out, "disappeared from the camera folder") {
		t.Errorf("the vanished images were not pointed out:\n%s", out)
	}
}

// A move into the processed folder that got there but reported an error - a
// network drive that timed out on the reply - leaves an image under its name in
// both folders. The next scan skips such an image for good, so its archive is
// the only way the visit is ever delivered, and it must be kept.
func TestArchiveIsKeptWhenItsImagesAreAlreadyInTheProcessedFolder(t *testing.T) {
	noMoveRetryDelay(t)
	camera, processed := t.TempDir(), t.TempDir()
	p1 := fitsFile(t, camera, "Cep-01_2026-11-22_23-10-00_0001.fits")
	p2 := fitsFile(t, camera, "Cep-01_2026-11-22_23-10-40_0002.fits")
	orig := renameImage
	renameImage = func(oldpath, newpath string) error {
		data, err := os.ReadFile(oldpath)
		if err == nil {
			err = os.WriteFile(newpath, data, 0644)
		}
		if err == nil {
			err = &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: os.ErrDeadlineExceeded}
		}
		return err
	}
	t.Cleanup(func() { renameImage = orig })

	ac := testCam(t, &Config{CameraDirectory: camera, ProcessedDirectory: processed, Count: 2}, []string{"Cep-01"})
	ac.tempDirectory = t.TempDir()
	ac.zipCompressed = true

	var archive string
	var err error
	out := captureStdout(t, func() { archive, err = ac.packVisit("Cep-01", pairVisit(t, p1, p2)) })
	if err != nil || !exists(archive) {
		t.Fatalf("the archive was thrown away although the next scan skips its images for good: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Delete them from the camera folder") {
		t.Errorf("the copies left in the camera folder were not pointed out:\n%s", out)
	}
	if len(ac.stranded) != 0 {
		t.Errorf("images that are in the processed folder were counted as left behind: %v", ac.stranded)
	}
}

// When only some images of an archive can be moved, the archive goes out and
// the rest stay in the camera folder, where they can be sent twice. That needs
// a person, so it stands on the screen until they are gone.
func TestImagesLeftBehindByAnUploadedArchiveAreAWarning(t *testing.T) {
	noMoveRetryDelay(t)
	camera := t.TempDir()
	fitsFile(t, camera, "A-01_2026-11-22_23-10-00_0001.fits")
	p2 := fitsFile(t, camera, "A-01_2026-11-22_23-10-40_0002.fits")
	orig := renameImage
	renameImage = func(oldpath, newpath string) error {
		if oldpath == p2 {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: os.ErrPermission}
		}
		return orig(oldpath, newpath)
	}
	t.Cleanup(func() { renameImage = orig })

	ac := testCam(t, &Config{CameraDirectory: camera, ProcessedDirectory: t.TempDir(), Count: 2}, []string{"A-01"})
	ac.tempDirectory = t.TempDir()
	ac.zipCompressed = true

	captureStdout(t, ac.makeJobForAreas)
	out := captureStdout(t, ac.makeJobForAreas)
	if archives, _ := filepath.Glob(filepath.Join(ac.tempDirectory, "*.zip")); len(archives) != 1 {
		t.Fatalf("want the archive kept for upload, got %v\n%s", archives, out)
	}
	cond := ac.conditions[conditionPacking]
	if cond == nil || cond.severity != "WARNING" || cond.reason != strandedReason || !strings.Contains(cond.detail, filepath.Base(p2)) {
		t.Fatalf("want a standing warning naming the image left behind, got %+v\n%s", cond, out)
	}

	// A person moves it by hand: the warning ends.
	if err := os.Rename(p2, filepath.Join(ac.config.ProcessedDirectory, filepath.Base(p2))); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, ac.makeJobForAreas)
	if ac.hasCondition(conditionPacking) || !strings.Contains(out, "RESOLVED") {
		t.Errorf("the warning did not end once the image was moved:\n%s", out)
	}
}

// A mistyped SAI_ARCHIVE_MODE used to be echoed at startup as if it had been
// taken. It is said out loud now, and the default is used.
func TestUnknownArchiveModeIsReported(t *testing.T) {
	cfg := &Config{ArchiveMode: "zipp"}
	out := captureStdout(t, func() { determineArchiveSettings(cfg) })
	if !strings.Contains(out, "unknown SAI_ARCHIVE_MODE 'zipp'") || cfg.ArchiveMode != "auto" {
		t.Errorf("an unknown archive mode was not reported and replaced by auto: %q, mode %q", out, cfg.ArchiveMode)
	}
	for _, mode := range []string{"auto", "rar", "zip", "zip-uncompressed"} {
		cfg := &Config{ArchiveMode: mode}
		if out := captureStdout(t, func() { determineArchiveSettings(cfg) }); strings.Contains(out, "unknown") {
			t.Errorf("the valid mode %q was reported as unknown", mode)
		}
	}
}

// The archive is flushed to the disk before any of its images leaves the
// camera folder. Windows flushes only through a handle that may write, so a
// read-only open would fail there on every archive; the CI runs this test on
// Windows too.
func TestArchiveCanBeFlushedToTheDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026-11-23_CI_A-01_052206_TEST.zip")
	if err := os.WriteFile(path, []byte("archive"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := syncFile(path); err != nil {
		t.Errorf("an archive could not be flushed to the disk: %v", err)
	}
}
