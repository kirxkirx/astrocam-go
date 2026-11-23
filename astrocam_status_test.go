package main

// Tests for what the operator is told: the status lines that end every cycle,
// who gets the blame when an upload is not accepted, the notes about images
// that are not being uploaded, and stopping the program on request.

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer stands in for the upload script: one behaviour for the status
// check (GET) and one for an upload (POST), with a count of each.
type fakeServer struct {
	gets, posts int32
	get, post   http.HandlerFunc
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		io.Copy(io.Discard, r.Body)
		atomic.AddInt32(&f.posts, 1)
		f.post(w, r)
		return
	}
	atomic.AddInt32(&f.gets, 1)
	f.get(w, r)
}

// say answers with a fixed status and body.
func say(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

const (
	statusOK      = "UNMW_STATUS:OK"
	uploadOK      = "<html><body><!-- UNMW_STATUS:OK --><p>Upload successful. Redirecting to results...</p></body></html>"
	unauthorized  = "<html><head><title>401 Unauthorized</title></head><body><h1>Unauthorized</h1><p>This server could not verify that you are authorized to access the document requested.</p></body></html>"
	badGateway    = "<html><head><title>502 Bad Gateway</title></head><body><h1>Bad Gateway</h1><p>The proxy server received an invalid response from an upstream server.</p></body></html>"
	processingErr = "<html><body>UNMW_STATUS:ERROR Error during processing</body></html>"
)

// startServer runs a fake upload server for the length of one test and
// returns an AstroCam set up to deliver to it.
func startServer(t *testing.T, get, post http.HandlerFunc) (*fakeServer, *AstroCam) {
	t.Helper()
	srv := &fakeServer{get: get, post: post}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	ac := testCam(t, &Config{Server: ts.URL + "/upload.py", Count: 2}, []string{"Field"})
	ac.tempDirectory = t.TempDir()
	return srv, ac
}

// queueArchive puts a small but valid archive into the temp directory, the way
// packVisit leaves one there for the upload phase.
func queueArchive(t *testing.T, ac *AstroCam, name string) string {
	t.Helper()
	path := filepath.Join(ac.tempDirectory, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, member := range []string{"one.fits", "two.fits"} {
		w, err := zw.Create(member)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("x"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// checkWidth fails the test for every line too long for an 80 column console.
func checkWidth(t *testing.T, lines []string) {
	t.Helper()
	for _, line := range lines {
		if len(line) > consoleWidth {
			t.Errorf("%d characters, which does not fit the console: %q", len(line), line)
		}
	}
}

func TestStatusLinesWhenAllIsWell(t *testing.T) {
	now := time.Date(2026, 11, 23, 5, 23, 19, 0, time.Local)
	ac := testCam(t, &Config{Count: 2}, []string{"Field"})
	ac.label = "v0.6.0"
	ac.serverOKAt = now.Add(-9 * time.Minute)
	ac.uploadedCount = 37
	ac.lastUploadOK = now.Add(-9 * time.Minute)
	ac.lastImageAt = now.Add(-38 * time.Second)
	ac.waitingFields = 1

	want := []string{
		"05:23:19 Server OK at 05:14:19. 37 archives uploaded, last at 05:14:19.",
		"  v0.6.0 Queue empty. Last image at 05:22:41. 1 field waits for its 2nd image.",
	}
	got := ac.statusLines(now)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("status lines are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	checkWidth(t, got)
}

// A program that has just started knows nothing yet and must say exactly that,
// rather than claim a server status it never asked for.
func TestStatusLinesBeforeAnythingHasHappened(t *testing.T) {
	now := time.Date(2026, 11, 23, 5, 23, 19, 0, time.Local)
	ac := testCam(t, &Config{Count: 2}, []string{"Field"})

	want := []string{
		"05:23:19 Server not contacted yet: no archive has been ready to upload.",
		"         Queue empty. No images seen yet.",
	}
	got := ac.statusLines(now)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("status lines are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	checkWidth(t, got)
}

// The server's state is the first thing on the first line, in capitals when it
// is bad, together with how long it has lasted and when the next try is due.
func TestStatusLinesNameAServerProblem(t *testing.T) {
	now := time.Date(2026, 11, 23, 5, 23, 19, 0, time.Local)
	ac := testCam(t, &Config{Count: 2}, []string{"Field"})
	ac.label = "v0.6.0"
	ac.conditions = map[string]*problemCondition{conditionDelivery: {
		severity: "ERROR",
		reason:   "Server is out of disk space",
		since:    now.Add(-(4*time.Hour + 32*time.Minute)),
		until:    time.Date(2026, 11, 23, 6, 21, 4, 0, time.Local),
	}}
	ac.uploadedCount = 12
	ac.lastUploadOK = time.Date(2026, 11, 23, 0, 49, 3, 0, time.Local)
	ac.serverOKAt = ac.lastUploadOK
	ac.queuedCount = 50
	ac.lastImageAt = now.Add(-38 * time.Second)
	// An upload is not "due" while the server is refusing them: the line above
	// already says when the next try is.
	ac.lastUploadTime = now

	want := []string{
		"05:23:19 !!! SERVER IS OUT OF DISK SPACE for 4h 32m. Next try at 06:21:04.",
		"         12 archives uploaded, last at 00:49:03.",
		"  v0.6.0 50 archives queued. Last image at 05:22:41.",
	}
	got := ac.statusLines(now)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("status lines are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	checkWidth(t, got)
}

// Whatever the state, no status line may be longer than the console is wide.
func TestStatusLinesFitTheConsole(t *testing.T) {
	now := time.Date(2026, 11, 23, 5, 23, 19, 0, time.Local)
	longAgo := now.Add(-40 * 24 * time.Hour)

	reasons := []string{
		"Server is out of disk space",
		"Server is busy: system load too high",
		"Server reported a temporary error",
		"Upload server is unreachable",
	}
	for _, code := range []int{301, 401, 403, 404, 407, 410, 500, 502, 503, 504} {
		reason, _ := classifyHTTPStatus(code)
		reasons = append(reasons, reason)
	}

	for _, label := range []string{"", "v0.6.0", "v10.20.30", "dev-0236ae8+"} {
		for _, count := range []int{2, 3} {
			ac := testCam(t, &Config{Count: count}, []string{"Field"})
			ac.label = label
			ac.uploadedCount = 123456
			ac.lastUploadOK = longAgo
			ac.serverOKAt = longAgo
			ac.serverOKName = "Fallback server"
			ac.serverWarning = "it is LOW ON DISK SPACE"
			ac.queuedCount = 98765
			ac.queuedBytes = 1023 * 1024 * 1024
			ac.lastUploadTime = now
			ac.lastImageAt = longAgo
			ac.waitingFields = 4321
			ac.strayCount = 54321
			checkWidth(t, ac.statusLines(now))

			for _, reason := range reasons {
				ac.conditions = map[string]*problemCondition{conditionDelivery: {
					severity: "ERROR", reason: reason, since: longAgo, until: now.Add(time.Hour),
				}}
				lines := ac.statusLines(now)
				checkWidth(t, lines)
				if !strings.Contains(lines[0], "!!! "+strings.ToUpper(reason)) {
					t.Errorf("the first status line does not name %q: %q", reason, lines[0])
				}
			}
		}
	}
}

// "Last image at 05:14" must not look healthy the day after - but midnight is
// the middle of an observing night, and must not change how a time is shown.
func TestStatusLinesDateAnOldMoment(t *testing.T) {
	now := time.Date(2026, 11, 23, 5, 23, 19, 0, time.Local)
	if got := clockTime(now.Add(-10*time.Minute), now); got != "05:13:19" {
		t.Errorf("a moment ten minutes ago is shown as %q", got)
	}
	if got := clockTime(time.Date(2026, 11, 22, 23, 10, 5, 0, time.Local), now); got != "23:10:05" {
		t.Errorf("a moment earlier in the same night, before midnight, is shown as %q", got)
	}
	if got := clockTime(time.Date(2026, 11, 22, 5, 14, 40, 0, time.Local), now); got != "Nov 22 05:14" {
		t.Errorf("a moment a day ago is shown as %q, which hides that a day has passed", got)
	}
}

// The queue sentence says when the next upload starts, so that an operator
// looking at "3 archives queued" under "Server OK" does not have to wonder why
// nothing is moving.
func TestStatusLinesSayWhenTheNextUploadIsDue(t *testing.T) {
	// Half a minute past midnight, to make sure that the day changing under a
	// running night does not change what the lines look like.
	now := time.Date(2026, 11, 23, 0, 0, 30, 0, time.Local)
	ac := testCam(t, &Config{Count: 2}, []string{"Field"})
	ac.label = "v0.6.0"
	ac.serverOKAt = now.Add(-45 * time.Second)
	ac.uploadedCount = 1
	ac.lastUploadOK = now.Add(-45 * time.Second)
	ac.queuedCount = 3
	ac.lastUploadTime = now.Add(-45 * time.Second)
	ac.lastImageAt = now.Add(-50 * time.Second)

	// This is what the screen shows for most of a night, so it has to stay at
	// two lines per cycle.
	want := []string{
		"00:00:30 Server OK at 23:59:45. 1 archive uploaded, last at 23:59:45.",
		"  v0.6.0 3 archives queued, next upload at 00:01:45. Last image at 23:59:40.",
	}
	got := ac.statusLines(now)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("status lines are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	checkWidth(t, got)
}

func TestFlowLinesKeepsSentencesWhole(t *testing.T) {
	sentences := []string{
		"This sentence is forty characters long..",
		"And so is this one, to the very last dot",
		"Short.",
	}
	got := flowLines("12:00:00 ", statusIndent, sentences)
	want := []string{
		"12:00:00 This sentence is forty characters long..",
		"         And so is this one, to the very last dot Short.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A sentence too long for any line is broken between words, never inside
	// one - also when it is the opening text that leaves it too little room.
	long := strings.TrimSpace(strings.Repeat("word ", 40))
	for _, first := range []string{"12:00:00 ", "Field with-quite-a-long-name-for-a-field-Q2b1x1: "} {
		lines := flowLines(first, "  ", []string{long, "Then a short one."})
		checkWidth(t, lines)
		joined := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
		if want := first + long + " Then a short one."; joined != want {
			t.Errorf("words were lost or split while wrapping:\n%q\nwant\n%q", joined, want)
		}
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		code  int
		scope int
		words string
	}{
		{301, scopeConfig, "redirects"},
		{302, scopeConfig, "redirects"},
		{401, scopeConfig, "refuses the login"},
		{403, scopeConfig, "refuses access"},
		{404, scopeConfig, "address not found"},
		{500, scopeServer, "Server-side error"},
		{502, scopeServer, "Server-side error"},
		{504, scopeServer, "Server-side error"},
		// Too large, or malformed: about this one request, not about the server.
		{413, scopeUnclear, ""},
		{400, scopeUnclear, ""},
		// 507 has always had its own handling: the disk is full.
		{507, scopeUnclear, ""},
		{200, scopeUnclear, ""},
	}
	for _, c := range cases {
		reason, scope := classifyHTTPStatus(c.code)
		if scope != c.scope {
			t.Errorf("HTTP %d: scope %d, want %d", c.code, scope, c.scope)
		}
		if !strings.Contains(reason, c.words) {
			t.Errorf("HTTP %d: reason %q does not say %q", c.code, reason, c.words)
		}
	}
}

// The advice has to match the problem. "Upload" contains the word "load", and
// the advice for a busy server ("it should recover on its own") used to be one
// careless substring match away from being given for a wrong address.
func TestOperatorAdviceMatchesTheProblem(t *testing.T) {
	cases := []struct{ reason, want string }{
		{"Server is out of disk space", "free disk space"},
		{"Server is busy: system load too high", "recover on its own"},
		{"unreachable", "network connection"},
		{"Upload server is unreachable", "network connection"},
		{"Server refuses the login (HTTP 401)", "SAI_PASSWORD"},
		{"Server refuses access (HTTP 403)", "SAI_PASSWORD"},
		{"Upload address not found on the server (HTTP 404)", "SAI_SERVER"},
		{"Server redirects uploads elsewhere (HTTP 301)", "SAI_SERVER"},
		{"Server-side error (HTTP 502)", "fixed there"},
	}
	for _, c := range cases {
		advice := operatorAdviceFor(c.reason)
		if !strings.Contains(advice, c.want) {
			t.Errorf("advice for %q does not mention %q: %s", c.reason, c.want, advice)
		}
		if !strings.Contains(advice, "Nothing is lost") {
			t.Errorf("advice for %q does not say that nothing is lost: %s", c.reason, advice)
		}
	}
}

func TestPlainServerText(t *testing.T) {
	got := plainServerText("<html><body>UNMW_STATUS:ERROR Error during processing.<br>\n  Exit status 1 &amp; more</body></html>")
	if want := "UNMW_STATUS:ERROR Error during processing. Exit status 1 & more"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if long := plainServerText(strings.Repeat("word ", 1000)); len(long) > 700 {
		t.Errorf("a %d character answer was not cut down for the banner", len(long))
	}
}

// A proxy in front of the upload script answers with a page of its own when it
// cannot reach the script. Privoxy's comes with kilobytes of CSS ahead of its
// first visible word, and the 4 kB read limit cuts it off inside the style
// sheet; the console has to show the title, not the CSS.
func TestPlainServerTextSkipsStyleSheets(t *testing.T) {
	privoxy := `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.01//EN">
<html>
<head>
  <title>503 - Forwarding failure (Privoxy@localhost)</title>
  <meta http-equiv="Content-Style-Type" content="text/css">
  <style type="text/css">
/*
 * CSS for Privoxy CGI and script output
 */
body,td,th { font-family: arial, helvetica, helv, sans-serif; }
` + strings.Repeat("td.box { border: solid black 1px; }\n", 100)
	if got, want := plainServerText(privoxy), "503 - Forwarding failure (Privoxy@localhost)"; got != want {
		t.Errorf("a page cut off inside its style sheet: got %q, want %q", got, want)
	}

	page := "<html><head><title>502 Proxy Error</title><STYLE>p { color: red }</STYLE>" +
		"<script>var x = 1;</script></head><body><p>The proxy server received an invalid response.</p></body></html>"
	if got, want := plainServerText(page), "502 Proxy Error The proxy server received an invalid response."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A test run stops at the first answer that will not let it upload, and the
// log of a CI run is all there is to go on afterwards. The server's own words
// have to be in it: "Server-side error (HTTP 503)" alone does not say whether
// the upload script refused or a proxy in front of it could not reach it.
func TestTestModeFatalTextCarriesTheServerResponse(t *testing.T) {
	got := testModeFatalText("Server-side error (HTTP 503)",
		httpAnswerText("503 Service Unavailable", "", "<html><head><title>503 - Forwarding failure (Privoxy@localhost)</title></head></html>"))
	want := "FATAL ERROR (Test Mode): Server-side error (HTTP 503)\n" +
		"  Server response: HTTP 503 Service Unavailable. 503 - Forwarding failure (Privoxy@localhost)\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// The upload script's own refusal carries its UNMW_STATUS marker, which is
	// how the CI integration test knows that the real server was reached.
	got = testModeFatalText("Server is busy: system load too high", "UNMW_STATUS:ERROR system load too high: 63.2\n")
	if !strings.Contains(got, "  Server response: UNMW_STATUS:ERROR system load too high: 63.2\n") {
		t.Errorf("the server's answer is missing: %q", got)
	}

	if got := testModeFatalText("Server-side error (HTTP 502)", ""); got != "FATAL ERROR (Test Mode): Server-side error (HTTP 502)\n" {
		t.Errorf("an empty answer must not add a line: %q", got)
	}
}

// The bug this guards against lost data without a trace: an upload answered
// with a redirect was repeated by the HTTP client as a GET without the
// archive, the server's status check answered that GET with UNMW_STATUS:OK,
// and the archive was deleted here as delivered.
func TestUploadIsNotFooledByARedirect(t *testing.T) {
	srv, ac := startServer(t, say(200, statusOK), func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere/upload.py", http.StatusMovedPermanently)
	})
	archive := queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	out := captureStdout(t, ac.uploadPhase)

	if !exists(archive) {
		t.Fatalf("the archive was deleted although the server never received it:\n%s", out)
	}
	if ac.uploadedCount != 0 || strings.Contains(out, "Successfully uploaded") {
		t.Errorf("a redirect was reported as a successful upload:\n%s", out)
	}
	if n := atomic.LoadInt32(&srv.gets); n != 1 {
		t.Errorf("%d GET requests, want only the status check: the redirect was followed", n)
	}
	cond := ac.conditions[conditionDelivery]
	if cond == nil {
		t.Fatalf("no standing problem was raised:\n%s", out)
	}
	if !strings.Contains(cond.reason, "redirects") || !strings.Contains(cond.detail, "/elsewhere/upload.py") {
		t.Errorf("the problem does not say that, and where, the server redirects: %q / %q", cond.reason, cond.detail)
	}
	if ac.hasCondition(conditionArchive) || len(ac.rejects) != 0 {
		t.Errorf("the redirect was counted against the archive")
	}
}

// When the status check itself is redirected there is no point in sending the
// archive after it.
func TestRedirectedStatusCheckStopsBeforeUploading(t *testing.T) {
	redirect := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/upload.py", http.StatusFound)
	}
	srv, ac := startServer(t, redirect, redirect)
	archive := queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	captureStdout(t, ac.uploadPhase)

	if n := atomic.LoadInt32(&srv.posts); n != 0 {
		t.Errorf("the archive was sent %d time(s) to a server that redirects", n)
	}
	// The redirect is reported as such. Following it would end in whatever
	// the other address has to say, here "unreachable", which sends the
	// operator looking for a network fault that does not exist.
	cond := ac.conditions[conditionDelivery]
	if cond == nil || cond.reason != "Server redirects uploads elsewhere (HTTP 302)" ||
		!strings.Contains(cond.detail, "https://example.invalid/upload.py") {
		t.Errorf("want the redirect and its target reported, got %+v", cond)
	}
	if !exists(archive) {
		t.Errorf("the archive was deleted")
	}
}

// A wrong password is a problem with config.env, not with the archive that was
// about to be sent, and the archive must not even leave the machine.
func TestRefusedLoginIsNotBlamedOnTheArchive(t *testing.T) {
	srv, ac := startServer(t, say(401, unauthorized), say(401, unauthorized))
	archive := queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	out := captureStdout(t, func() {
		ac.uploadPhase()
		ac.reportStatus()
	})

	if n := atomic.LoadInt32(&srv.posts); n != 0 {
		t.Errorf("the archive was sent %d time(s) although the status check was refused", n)
	}
	cond := ac.conditions[conditionDelivery]
	if cond == nil || cond.reason != "Server refuses the login (HTTP 401)" {
		t.Fatalf("want a standing login problem, got %+v\n%s", cond, out)
	}
	if !strings.Contains(cond.action, "SAI_USERNAME") || !strings.Contains(cond.action, "SAI_PASSWORD") {
		t.Errorf("the operator is not told what to check: %s", cond.action)
	}
	if strings.Contains(cond.detail, "<") {
		t.Errorf("the banner would show raw HTML: %s", cond.detail)
	}
	if ac.hasCondition(conditionArchive) || len(ac.rejects) != 0 {
		t.Errorf("the refused login was counted against the archive")
	}
	if strings.Contains(out, "damaged") || strings.Contains(out, "by hand") {
		t.Errorf("the operator is invited to remove a perfectly good archive:\n%s", out)
	}
	if !strings.Contains(out, "!!! SERVER REFUSES THE LOGIN (HTTP 401) for ") {
		t.Errorf("the status lines do not name the problem:\n%s", out)
	}
	if !exists(archive) || !ac.isUploadPaused() {
		t.Errorf("archive kept: %v, uploads paused: %v", exists(archive), ac.isUploadPaused())
	}
}

// Some servers ask for a password only when something is uploaded. The status
// check then passes, and the refusal arrives as the answer to the upload.
func TestLoginRefusedOnlyForUploads(t *testing.T) {
	srv, ac := startServer(t, say(200, statusOK), say(401, unauthorized))
	first := queueArchive(t, ac, "2026-11-23_Field_010203.zip")
	second := queueArchive(t, ac, "2026-11-23_Field_010405.zip")

	captureStdout(t, ac.uploadPhase)

	if n := atomic.LoadInt32(&srv.posts); n != 1 {
		t.Errorf("%d uploads were tried; one refused login is enough to know", n)
	}
	cond := ac.conditions[conditionDelivery]
	if cond == nil || !strings.Contains(cond.reason, "refuses the login") {
		t.Fatalf("want a standing login problem, got %+v", cond)
	}
	if ac.hasCondition(conditionArchive) || len(ac.rejects) != 0 {
		t.Errorf("the refused login was counted against the archive")
	}
	if !exists(first) || !exists(second) {
		t.Errorf("an archive was deleted")
	}

	// The status check passes again when the pause is over. That must not be
	// taken for the end of a problem the status check never saw.
	ac.uploadPauseUntil = time.Now().Add(-time.Second)
	ac.lastUploadTime = time.Time{}
	out := captureStdout(t, ac.uploadPhase)
	if strings.Contains(out, "RESOLVED") {
		t.Errorf("a passing status check was taken for the end of the login problem:\n%s", out)
	}
	if cond := ac.conditions[conditionDelivery]; cond == nil || !strings.Contains(cond.reason, "refuses the login") {
		t.Errorf("the login problem is no longer standing: %+v", cond)
	}
}

// A proxy with nothing behind it answers for the upload script. That is the
// server's trouble and says nothing about the archive.
func TestGatewayErrorIsAServerProblem(t *testing.T) {
	srv, ac := startServer(t, say(502, badGateway), say(502, badGateway))
	queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	captureStdout(t, ac.uploadPhase)

	if n := atomic.LoadInt32(&srv.posts); n != 0 {
		t.Errorf("the archive was sent %d time(s) to a server that answers 502", n)
	}
	cond := ac.conditions[conditionDelivery]
	if cond == nil || cond.reason != "Server-side error (HTTP 502)" {
		t.Fatalf("want a standing server problem, got %+v", cond)
	}
	if !strings.Contains(cond.detail, "HTTP 502 Bad Gateway") || strings.Contains(cond.detail, "<") {
		t.Errorf("the server's answer is not shown in plain words: %s", cond.detail)
	}
	if ac.hasCondition(conditionArchive) {
		t.Errorf("the gateway error was blamed on the archive")
	}
}

// An error page can show markup as text, such as a filter that quotes the
// "<script>" it objected to. Once the page is plain text, that is just words:
// cleaning it a second time would take it for the start of a script and drop
// everything after it from the banner.
func TestServerTextIsCleanedOnlyOnce(t *testing.T) {
	page := "<html><body><p>Bad header: &lt;script&gt; seen in the upstream reply; check the backend</p></body></html>"
	_, ac := startServer(t, say(502, page), say(502, page))
	queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	captureStdout(t, ac.uploadPhase)

	cond := ac.conditions[conditionDelivery]
	if cond == nil {
		t.Fatalf("want a standing server problem")
	}
	if !strings.Contains(cond.detail, "Bad header: <script> seen in the upstream reply; check the backend") {
		t.Errorf("the server's words were cut short: %q", cond.detail)
	}
}

// An old server knows nothing of status checks and answers them with some page
// of its own. Uploads to it must go on working.
func TestServerWithoutStatusCheckStillGetsUploads(t *testing.T) {
	for _, code := range []int{200, 405} {
		_, ac := startServer(t, say(code, "<html><body>Upload form</body></html>"), say(200, uploadOK))
		archive := queueArchive(t, ac, "2026-11-23_Field_010203.zip")

		out := captureStdout(t, ac.uploadPhase)

		if exists(archive) || ac.uploadedCount != 1 {
			t.Errorf("status check answered HTTP %d without a status: the archive was not delivered:\n%s", code, out)
		}
	}
}

// A problem that the status check found is over when the status check says so.
func TestStatusCheckWithdrawsTheProblemItFound(t *testing.T) {
	var full atomic.Bool
	full.Store(true)
	status := func(w http.ResponseWriter, r *http.Request) {
		if full.Load() {
			say(507, "UNMW_STATUS:ERROR ERROR: server x is out of disk space, only 1000 MB free at uploads")(w, r)
			return
		}
		say(200, statusOK)(w, r)
	}
	_, ac := startServer(t, status, say(200, uploadOK))
	archive := queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	captureStdout(t, ac.uploadPhase)
	cond := ac.conditions[conditionDelivery]
	if cond == nil || cond.reason != "Server is out of disk space" || !ac.isUploadPaused() {
		t.Fatalf("a full server did not pause the uploads: %+v", cond)
	}

	full.Store(false)
	ac.uploadPauseUntil = time.Now().Add(-time.Second) // the hour is up
	out := captureStdout(t, func() {
		ac.uploadPhase()
		ac.refreshQueue()
		ac.reportStatus()
	})

	// The announcement is folded to fit the console; undo that to read it.
	announced := strings.ReplaceAll(out, "\n=== ", " ")
	if !strings.Contains(announced, "RESOLVED: Server is out of disk space - the upload server answers its status check normally again") ||
		ac.hasCondition(conditionDelivery) {
		t.Errorf("the end of the problem was not announced when the status check passed:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "=== ") && len(line) > consoleWidth {
			t.Errorf("the announcement does not fit the console: %q", line)
		}
	}
	if exists(archive) || ac.uploadedCount != 1 {
		t.Errorf("the waiting archive was not delivered once the server had room:\n%s", out)
	}
	if !strings.Contains(out, " Server OK at ") || !strings.Contains(out, " 1 archive uploaded, last at ") {
		t.Errorf("the status lines do not show the recovery:\n%s", out)
	}
	if !strings.Contains(out, "Queue empty.") {
		t.Errorf("the status lines still show a queue:\n%s", out)
	}

	// The same recovery, but the archive that is waiting is then refused for a
	// reason of its own. The disk is no longer full, and the status lines must
	// not go on saying that it is just because no upload has gone through.
	full.Store(true)
	_, ac = startServer(t, status, say(200, processingErr))
	queueArchive(t, ac, "2026-11-23_Field_010203.zip")
	captureStdout(t, ac.uploadPhase)
	full.Store(false)
	ac.uploadPauseUntil = time.Now().Add(-time.Second)
	out = captureStdout(t, func() {
		ac.uploadPhase()
		ac.refreshQueue()
		ac.reportStatus()
	})
	if ac.hasCondition(conditionDelivery) || strings.Contains(out, "!!! SERVER IS OUT OF DISK SPACE") {
		t.Errorf("the server has room again, but the status lines still say it is full:\n%s", out)
	}
	if !strings.Contains(out, " Server OK at ") || !strings.Contains(out, "1 archive queued") {
		t.Errorf("the status lines do not show a healthy server with one archive waiting:\n%s", out)
	}
}

// A server that cannot be reached gives the same network error on every cycle.
// It is printed when it is new; after that the status lines carry the problem,
// and the error text is not poured over the screen again every 15 seconds.
func TestUnreachableServerIsNotSpelledOutEveryCycle(t *testing.T) {
	fallback, ac := startServer(t, say(200, statusOK), say(200, uploadOK))
	liveURL := ac.config.Server

	// An address with nothing listening: a server that has just been closed.
	// It is made after the live one, so that the live one cannot be handed the
	// port that was just given up.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL + "/upload.py"
	dead.Close()

	ac.config.Server = deadURL
	archive := queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	first := captureStdout(t, func() {
		ac.uploadPhase()
		ac.reportStatus()
	})
	cond := ac.conditions[conditionDelivery]
	if cond == nil || cond.reason != "Upload server is unreachable" {
		t.Fatalf("want a standing \"unreachable\" problem, got %+v\n%s", cond, first)
	}
	if !strings.Contains(first, "Server "+deadURL+" is unreachable: ") ||
		!strings.Contains(first, "All upload servers are unreachable") {
		t.Errorf("the failure was not reported when it first happened:\n%s", first)
	}
	if !strings.Contains(cond.detail, "Tried: "+deadURL+" (") {
		t.Errorf("the banner does not carry the address and the network error: %s", cond.detail)
	}

	second := captureStdout(t, func() {
		ac.uploadPhase()
		ac.reportStatus()
	})
	if strings.Contains(second, "is unreachable: ") || strings.Contains(second, "All upload servers") {
		t.Errorf("the same network error was spelled out again one cycle later:\n%s", second)
	}
	if !strings.Contains(second, "!!! UPLOAD SERVER IS UNREACHABLE for ") {
		t.Errorf("the status lines do not name the standing problem:\n%s", second)
	}

	// A fallback server appears (in real life: it is added to config.env and
	// the program restarted). The archive goes there and the problem is over.
	ac.config.FallbackServer = liveURL
	third := captureStdout(t, func() {
		ac.uploadPhase()
		ac.reportStatus()
	})
	if exists(archive) || atomic.LoadInt32(&fallback.posts) != 1 {
		t.Errorf("the archive was not delivered to the fallback server:\n%s", third)
	}
	if ac.hasCondition(conditionDelivery) || !strings.Contains(third, "RESOLVED") {
		t.Errorf("the end of the problem was not announced:\n%s", third)
	}
	if !strings.Contains(third, " Fallback server OK at ") {
		t.Errorf("the status lines do not say which server took the upload:\n%s", third)
	}
}

// A server that is low on disk space still takes uploads, and the status line
// has to say both.
func TestServerWarningIsKeptOnScreen(t *testing.T) {
	_, ac := startServer(t,
		say(200, "UNMW_STATUS:WARNING WARNING: server x is low on disk space, only 9000 MB free at uploads"),
		say(200, uploadOK))
	queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	out := captureStdout(t, func() {
		ac.uploadPhase()
		ac.reportStatus()
	})
	if ac.uploadedCount != 1 {
		t.Errorf("a warning stopped the upload:\n%s", out)
	}
	if !strings.Contains(out, ", but it is LOW ON DISK SPACE.") {
		t.Errorf("the status lines do not pass the warning on:\n%s", out)
	}
}

// Uploads are spaced two minutes apart. The program must not sit still for
// those two minutes: the cycle ends, new images get packed, the status lines
// are printed, and the next upload starts when it is due.
func TestUploadSpacingDoesNotBlockTheCycle(t *testing.T) {
	srv, ac := startServer(t, say(200, statusOK), say(200, uploadOK))
	first := queueArchive(t, ac, "2026-11-23_Field_010203.zip")
	second := queueArchive(t, ac, "2026-11-23_Field_010405.zip")

	started := time.Now()
	captureStdout(t, ac.uploadPhase)
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("the upload phase took %v: it waited out the gap between uploads", elapsed)
	}
	if exists(first) || !exists(second) || ac.uploadedCount != 1 {
		t.Fatalf("want the older archive delivered and the newer one waiting; delivered %d", ac.uploadedCount)
	}

	// The next cycle comes too early for another upload. It must leave the
	// server alone altogether, not ask for its status and then do nothing.
	captureStdout(t, ac.uploadPhase)
	if gets, posts := atomic.LoadInt32(&srv.gets), atomic.LoadInt32(&srv.posts); gets != 1 || posts != 1 {
		t.Errorf("%d status checks and %d uploads so far, want 1 and 1", gets, posts)
	}

	// Once the gap has passed the second archive goes out.
	ac.lastUploadTime = time.Now().Add(-uploadThrottleDelay)
	captureStdout(t, ac.uploadPhase)
	if exists(second) || ac.uploadedCount != 2 {
		t.Errorf("the second archive was not delivered when its turn came")
	}
}

// Who is to blame for a refused archive is judged from the evidence.
func TestRefusalBlame(t *testing.T) {
	refusal := errors.New("upload not confirmed by server (HTTP 200): " + processingErr)

	// Every archive refused, none accepted: that points at the server, and the
	// operator must not be told to throw archives away.
	ac := testCam(t, &Config{Count: 2}, []string{"Field"})
	for i := 0; i < rejectsBeforeWarning; i++ {
		ac.noteRefusal("/temp/a.zip", refusal)
		ac.noteRefusal("/temp/b.zip", refusal)
	}
	cond := ac.conditions[conditionArchive]
	if cond == nil || cond.reason != "The server refuses every archive it is sent" {
		t.Fatalf("want the server named as the likely cause, got %+v", cond)
	}
	if !strings.Contains(cond.action, "nothing is deleted") || strings.Contains(cond.action, "by hand") {
		t.Errorf("the advice invites the operator to remove archives: %s", cond.action)
	}
	if !strings.Contains(cond.detail, "UNMW_STATUS:ERROR Error during processing") || strings.Contains(cond.detail, "<") {
		t.Errorf("the server's answer is not shown in plain words: %s", cond.detail)
	}

	// One archive refused while another goes through: now it is the archive.
	ac = testCam(t, &Config{Count: 2}, []string{"Field"})
	ac.noteRefusal("/temp/a.zip", refusal)
	ac.uploadedCount++ // another archive was accepted afterwards
	ac.noteRefusal("/temp/a.zip", refusal)
	ac.noteRefusal("/temp/a.zip", refusal)
	cond = ac.conditions[conditionArchive]
	if cond == nil || cond.reason != "The server keeps refusing one archive" {
		t.Fatalf("want the one archive named, got %+v", cond)
	}
	if !strings.Contains(cond.detail, "a.zip has been refused 3 times, while other archives were accepted") {
		t.Errorf("the detail does not give the evidence: %s", cond.detail)
	}

	// A single archive and nothing to compare it with: say that it is unclear.
	ac = testCam(t, &Config{Count: 2}, []string{"Field"})
	for i := 0; i < rejectsBeforeWarning; i++ {
		ac.noteRefusal("/temp/a.zip", refusal)
	}
	cond = ac.conditions[conditionArchive]
	if cond == nil || !strings.Contains(cond.action, "not yet clear") {
		t.Fatalf("want an honest \"not yet clear\", got %+v", cond)
	}
	for _, c := range ac.conditions {
		if strings.Contains(c.action, "damaged") {
			t.Errorf("an archive is called damaged without evidence: %s", c.action)
		}
	}
}

// End to end: the server accepts one archive and keeps refusing another. The
// warning must name the right one, and must go away when it is moved aside.
func TestOneBadArchiveAmongGoodOnes(t *testing.T) {
	_, ac := startServer(t, say(200, statusOK), func(w http.ResponseWriter, r *http.Request) {
		// The multipart body was read by the fake server; the file name is in
		// the form data, which it discarded, so tell the archives apart by size.
		if r.ContentLength > 1000 {
			say(200, processingErr)(w, r)
			return
		}
		say(200, uploadOK)(w, r)
	})
	good := queueArchive(t, ac, "2026-11-23_Field_010203.zip")
	bad := filepath.Join(ac.tempDirectory, "2026-11-23_Field_010101.zip")
	if err := os.WriteFile(bad, make([]byte, 4000), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		for i := 0; i < 2*rejectsBeforeWarning+1; i++ {
			ac.lastUploadTime = time.Time{} // let every cycle upload at once
			ac.uploadPhase()
		}
		ac.refreshQueue()
		ac.reportStatus()
	})

	if exists(good) || !exists(bad) {
		t.Fatalf("good archive delivered: %v, refused archive kept: %v\n%s", !exists(good), exists(bad), out)
	}
	cond := ac.conditions[conditionArchive]
	if cond == nil || cond.reason != "The server keeps refusing one archive" {
		t.Fatalf("want a warning about the one archive, got %+v\n%s", cond, out)
	}
	if !strings.Contains(out, "!!! WARNING ") || !strings.Contains(out, ": The server keeps refusing one archive") {
		t.Errorf("the warning is not repeated under the status lines:\n%s", out)
	}
	if !strings.Contains(out, " Server OK at ") {
		t.Errorf("a refused archive must not make the server look broken:\n%s", out)
	}

	// The operator moves it away, as the banner suggests. The warning ends.
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		ac.refreshQueue()
		ac.reportStatus()
	})
	if ac.hasCondition(conditionArchive) || !strings.Contains(out, "RESOLVED") || strings.Contains(out, "!!! WARNING") {
		t.Errorf("the warning outlived the archive it was about:\n%s", out)
	}
}

// Ctrl+C has to end a wait and an upload in progress, not be noticed minutes
// later when the cycle happens to finish.
func TestStopRequestEndsWaitsAndUploads(t *testing.T) {
	release := make(chan struct{})
	_, ac := startServer(t, say(200, statusOK), func(w http.ResponseWriter, r *http.Request) {
		<-release // a server that never answers
	})
	// Registered after the server's own cleanup, so it runs before it: the
	// handler has to be let go before the server can shut down.
	t.Cleanup(func() { close(release) })
	archive := queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ac.ctx = ctx

	slept := make(chan bool, 1)
	go func() { slept <- ac.sleep(time.Hour) }()
	time.AfterFunc(300*time.Millisecond, cancel)

	started := time.Now()
	out := captureStdout(t, ac.uploadPhase)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the upload went on for %v after the stop request", elapsed)
	}
	select {
	case served := <-slept:
		if served {
			t.Errorf("a wait reported that it ran its full hour")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("a wait did not end when the program was asked to stop")
	}

	if !ac.stopping() {
		t.Errorf("the program does not know it was asked to stop")
	}
	if !exists(archive) {
		t.Errorf("the archive was deleted by an interrupted upload")
	}
	if len(ac.rejects) != 0 || len(ac.conditions) != 0 || strings.Contains(out, "Upload error") {
		t.Errorf("an upload interrupted by the operator was treated as a failure:\n%s", out)
	}
	// Later waits end at once too, and nothing new is started.
	if ac.sleep(time.Hour) || ac.waitForUploadSlot(true) {
		t.Errorf("a wait started after the stop request ran to its end")
	}
}

// sets the modification time of a file to some time ago.
func age(t *testing.T, path string, ago time.Duration) {
	t.Helper()
	when := time.Now().Add(-ago)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestScanDirNotesRecentFilesOfNoField(t *testing.T) {
	dir := t.TempDir()
	fitsFile(t, dir, "Cep-01_2026-11-22_23-10-00_0001.fits")
	fitsFile(t, dir, "2026-11-22_23-11-00_LIGHT_0002.fits")
	fitsFile(t, dir, "M31_2026-11-22_23-12-00_0003.FIT")
	// Last month's, and one from earlier tonight: by now both are history, and
	// a note about them would only be noise.
	age(t, fitsFile(t, dir, "M31_2026-10-01_22-00-00_0001.fits"), 30*24*time.Hour)
	age(t, fitsFile(t, dir, "M42_2026-11-22_21-00-00_0001.fits"), 2*time.Hour)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	ac := testCam(t, &Config{CameraDirectory: dir, ProcessedDirectory: t.TempDir(), Count: 2}, []string{"Cep-01"})
	found := ac.scanDir(dir, make(map[string]fileStamp))

	var names []string
	for _, s := range found.strays {
		names = append(names, s.name)
	}
	if len(names) != 2 {
		t.Errorf("strays are %v; want the two recent FITS files of no field, and neither the old ones nor notes.txt", names)
	}
}

// The whole scan, with real files: a complete pair is packed, a field that
// has just started is counted as waiting, an old single image is summed up
// once, and a file that belongs to no field is pointed out.
func TestScanTellsTheOperatorWhatIsNotBeingUploaded(t *testing.T) {
	camera := t.TempDir()
	fitsFile(t, camera, "A-01_2026-11-22_23-10-00_0001.fits")
	fitsFile(t, camera, "A-01_2026-11-22_23-10-40_0002.fits")
	fitsFile(t, camera, "B-02_2026-11-22_23-12-00_0003.fits")
	old := fitsFile(t, camera, "C-03_2026-11-01_02-00-00_0009.fits")
	age(t, old, 3*24*time.Hour)
	fitsFile(t, camera, "2026-11-22_23-13-00_LIGHT_0004.fits")

	ac := testCam(t, &Config{CameraDirectory: camera, ProcessedDirectory: t.TempDir(), Count: 2},
		[]string{"A-01", "B-02", "C-03"})
	ac.tempDirectory = t.TempDir()
	ac.zipCompressed = true

	// First scan: every file is new, so nothing may be packed yet. But the
	// images have been seen, and the stray is explained straight away.
	ac.cycle = 1
	first := captureStdout(t, ac.makeJobForAreas)
	if strings.Contains(first, "Archive created") {
		t.Fatalf("a file was packed on the cycle it first appeared:\n%s", first)
	}
	if ac.lastImageAt.IsZero() || time.Since(ac.lastImageAt) > time.Minute {
		t.Errorf("the newest image is not known after the first scan: %v", ac.lastImageAt)
	}
	if ac.waitingFields != 1 {
		t.Errorf("%d fields waiting, want 1: B-02 has one image, A-01 already has both", ac.waitingFields)
	}
	if ac.strayCount != 1 || !strings.Contains(first, "NOTE: 1 FITS file written in the last hour matches no field in areas.txt") ||
		!strings.Contains(first, "2026-11-22_23-13-00_LIGHT_0004.fits") {
		t.Errorf("the file of no field was not pointed out (count %d):\n%s", ac.strayCount, first)
	}
	if strings.Contains(first, "Left in the camera folders") {
		t.Errorf("leftovers were announced before it was known what can be packed:\n%s", first)
	}

	// Second scan: the pair is packed. What will be left over is not summed up
	// while complete sets are still being packed.
	ac.cycle = 2
	second := captureStdout(t, ac.makeJobForAreas)
	if !strings.Contains(second, "Archive created") {
		t.Fatalf("the complete pair was not packed:\n%s", second)
	}
	if strings.Contains(second, "NOTE:") || strings.Contains(second, "Left in the camera folders") {
		t.Errorf("more was said than that a pair was packed:\n%s", second)
	}
	if ac.waitingFields != 1 || ac.strayCount != 1 {
		t.Errorf("%d fields waiting and %d strays, want 1 and 1", ac.waitingFields, ac.strayCount)
	}

	// Third scan: nothing left to pack, so the old single image is summed up.
	ac.cycle = 3
	third := captureStdout(t, ac.makeJobForAreas)
	if !strings.Contains(third, "Left in the camera folders: 1 older image of 1 field that never made a") {
		t.Errorf("the old incomplete set was not mentioned:\n%s", third)
	}
	if strings.Contains(third, "Field B-02") {
		t.Errorf("a field that is still being observed was written off:\n%s", third)
	}

	// Fourth scan: nothing has changed, so nothing is said again.
	ac.cycle = 4
	if fourth := captureStdout(t, ac.makeJobForAreas); fourth != "" {
		t.Errorf("a scan that found nothing new printed:\n%s", fourth)
	}

	// The wait for B-02 runs out: more than a visit's length has passed since
	// its only image. That is said once, when it happens.
	age(t, filepath.Join(camera, "B-02_2026-11-22_23-12-00_0003.fits"), visitGap+time.Minute)
	ac.cycle = 5
	captureStdout(t, ac.makeJobForAreas) // the file looks changed; it settles again
	ac.openFields = map[string]bool{"B-02": true}
	ac.cycle = 6
	sixth := captureStdout(t, ac.makeJobForAreas)
	if !strings.Contains(sixth, "Field B-02: only 1 of 2 images arrived, and 10 minutes have passed.") ||
		!strings.Contains(sixth, "NOT complete") {
		t.Errorf("giving up on B-02 was not explained:\n%s", sixth)
	}
	if ac.waitingFields != 0 {
		t.Errorf("%d fields waiting, want none", ac.waitingFields)
	}
	ac.cycle = 7
	if seventh := captureStdout(t, ac.makeJobForAreas); seventh != "" {
		t.Errorf("the explanation was repeated:\n%s", seventh)
	}
}

// An archive that cannot be read from the local disk has not been refused by
// anybody, and a line that is no address at all is not an unlucky archive.
func TestLocalFaultsAreNotBlamedOnTheServerOrTheArchive(t *testing.T) {
	srv, ac := startServer(t, say(200, statusOK), say(200, uploadOK))
	missing := filepath.Join(ac.tempDirectory, "2026-11-23_Field_010203.zip")

	out := captureStdout(t, func() {
		for i := 0; i < rejectsBeforeWarning; i++ {
			if err := ac.uploadFile(missing, ac.config.Server); err != nil {
				ac.noteRefusal(missing, err)
			}
		}
	})
	cond := ac.conditions[conditionArchive]
	if cond == nil || cond.reason != "An archive cannot be read from this computer's disk" {
		t.Fatalf("want a local fault reported, got %+v\n%s", cond, out)
	}
	if strings.Contains(cond.detail+cond.action, "refus") || strings.Contains(cond.detail, "answer") {
		t.Errorf("the server is said to have refused an archive it never saw: %s / %s", cond.detail, cond.action)
	}
	if n := atomic.LoadInt32(&srv.posts); n != 0 {
		t.Errorf("%d uploads reached the server although the archive could not be read", n)
	}

	// SAI_SERVER holds something that is not an address.
	_, ac = startServer(t, say(200, statusOK), say(200, uploadOK))
	ac.config.Server = "http://bad host/upload.py"
	archive := queueArchive(t, ac, "2026-11-23_Field_010203.zip")
	out = captureStdout(t, ac.uploadPhase)
	cond = ac.conditions[conditionDelivery]
	if cond == nil || cond.reason != "Upload server is unreachable" || !strings.Contains(cond.detail, "not a usable address") {
		t.Fatalf("want the address reported as unusable, got %+v\n%s", cond, out)
	}
	if !strings.Contains(cond.action, "SAI_SERVER") {
		t.Errorf("the operator is not pointed at SAI_SERVER: %s", cond.action)
	}
	if !exists(archive) || len(ac.rejects) != 0 || ac.hasCondition(conditionArchive) {
		t.Errorf("a bad address was counted against the archive")
	}
}

// What the server answered is printed as text, not as the HTML it came in.
func TestUploadErrorIsPrintedAsPlainText(t *testing.T) {
	_, ac := startServer(t, say(200, statusOK), say(200, processingErr))
	queueArchive(t, ac, "2026-11-23_Field_010203.zip")

	out := captureStdout(t, ac.uploadPhase)
	want := "upload not confirmed by server (HTTP 200): UNMW_STATUS:ERROR Error during processing\n"
	if !strings.Contains(out, "Upload error (") || !strings.Contains(out, want) {
		t.Errorf("the server's answer is not printed in plain words:\n%s", out)
	}
	if strings.Contains(out, "<html>") {
		t.Errorf("raw HTML was printed:\n%s", out)
	}
}

func TestDescribeNoSet(t *testing.T) {
	at := func(name string, minutes int) candidate {
		return candidate{name: name, when: time.Date(2026, 11, 22, 23, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute)}
	}

	// The text is set into lines to fit the console, wherever the breaks fall.
	explain := func(area string, frames []candidate, count int) string {
		lines := describeNoSet(area, frames, count, visitGap)
		checkWidth(t, lines)
		return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	}

	// The case behind "has 2 images, needs 2, uploads nothing".
	text := explain("Gem-02-Q2b1x1", []candidate{at("a", 0), at("b", 47)}, 2)
	if text != "Field Gem-02-Q2b1x1: its 2 images were taken 47 minutes apart. "+
		"More than 10 minutes means two separate visits: they are NOT combined." {
		t.Errorf("two images of different visits are not explained: %s", text)
	}

	text = explain("Gem-02-Q2b1x1", []candidate{at("a", 0)}, 2)
	if text != "Field Gem-02-Q2b1x1: only 1 of 2 images arrived, and 10 minutes have passed. "+
		"The set is NOT complete: nothing is uploaded, the image stays where it is." {
		t.Errorf("a lone image is not explained: %s", text)
	}

	text = explain("Gem-02-Q2b1x1", []candidate{at("a", 0), at("b", 1), at("c", 60), at("d", 61)}, 3)
	if !strings.Contains(text, "4 images, but no 3 of them within 10 minutes of each other.") ||
		!strings.Contains(text, "NOT combined") {
		t.Errorf("two short visits are not explained: %s", text)
	}

	// A field may have any name; the explanation still has to fit.
	explain("A-rather-long-target-name-typed-into-the-sequencer-Q2b1x1", []candidate{at("a", 0), at("b", 47)}, 2)

	// A gap barely over the limit must not be printed as the limit itself.
	if got := roughDuration(visitGap + 20*time.Second); got != "11 minutes" {
		t.Errorf("a gap of 10 min 20 s reads %q", got)
	}
	if got := roughDuration(26 * time.Hour); got != "26 hours" {
		t.Errorf("a gap of 26 hours reads %q", got)
	}
}

// The list of watched folders grows by one every night. It must stay short.
func TestWatchedFoldersAreListedBriefly(t *testing.T) {
	ac := testCam(t, &Config{Count: 2}, []string{"Field"})
	var dirs []string
	for day := 1; day <= 28; day++ {
		dirs = append(dirs, filepath.Join("/images", time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC).Format(dateFolderLayout), "LIGHT"))
	}

	out := captureStdout(t, func() { ac.reportScanSet(dirs) })
	if n := strings.Count(out, "\n"); n > 6 {
		t.Errorf("%d lines to list the watched folders:\n%s", n, out)
	}
	if !strings.Contains(out, "Watching 28 folders for images:") || !strings.Contains(out, dirs[0]) ||
		!strings.Contains(out, "and 25 older ones") {
		t.Errorf("the summary does not say what is watched:\n%s", out)
	}
	if again := captureStdout(t, func() { ac.reportScanSet(dirs) }); again != "" {
		t.Errorf("an unchanged set of folders was listed again:\n%s", again)
	}
}

// The explanation of files that match no field is given when they appear and
// repeated only now and then, however many cycles pass.
func TestStrayFilesAreExplainedOnceInAWhile(t *testing.T) {
	ac := testCam(t, &Config{Count: 2}, []string{"Field"})
	strays := []strayFile{{name: "x_1.fits", mtime: time.Now()}}

	explained := 0
	for cycle := 1; cycle <= 3*conditionFullBannerEveryCycles; cycle++ {
		ac.cycle = cycle
		// One more stray file arrives on every cycle.
		strays = append(strays, strayFile{name: "x.fits", mtime: time.Now()})
		if out := captureStdout(t, func() { ac.noteStrays(strays) }); out != "" {
			explained++
		}
	}
	if explained != 3 {
		t.Errorf("explained %d times in %d cycles, want 3", explained, 3*conditionFullBannerEveryCycles)
	}
	if ac.strayCount != len(strays) {
		t.Errorf("the status lines would show %d stray files, there are %d", ac.strayCount, len(strays))
	}

	// They are gone (renamed, or a night has passed); new ones count as new.
	captureStdout(t, func() { ac.noteStrays(nil) })
	if out := captureStdout(t, func() { ac.noteStrays(strays[:1]) }); out == "" {
		t.Errorf("a stray file that appeared after the others had gone was not explained")
	}
}

func TestVersionIsShownUnderTheTime(t *testing.T) {
	ac := testCam(t, &Config{Count: 2}, []string{"Field"})
	for label, want := range map[string]string{
		"":             "         ",
		"v0.6.0":       "  v0.6.0 ",
		"v10.20.3":     "v10.20.3 ",
		"dev-0236ae8+": "dev-0236ae8+ ",
	} {
		ac.label = label
		if got := ac.versionGutter(); got != want {
			t.Errorf("label %q starts the second status line with %q, want %q", label, got, want)
		}
	}
	if buildLabel() == "" {
		t.Errorf("this binary has no name to show")
	}
}
