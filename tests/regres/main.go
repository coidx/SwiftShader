// Copyright 2019 The SwiftShader Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Regres is a tool that detects test regressions with SwiftShader changes.
//
// Regres monitors changes that have been put up for review with Gerrit.
// Once a new patchset has been found, regres will checkout, build and test the
// change against the parent changelist. Any differences in results are reported
// as a review comment on the change.
//
// Once a day regres will also test another, larger set of tests, and post the
// full test results as a Gerrit changelist. The CI test lists can be based from
// this daily test list, so testing can be limited to tests that were known to
// pass.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"./cause"
	"./git"
	"./shell"
	"./testlist"

	gerrit "github.com/andygrunwald/go-gerrit"
)

const (
	gitURL                  = "https://swiftshader.googlesource.com/SwiftShader"
	gerritURL               = "https://swiftshader-review.googlesource.com/"
	reportHeader            = "Regres report:"
	dataVersion             = 1
	changeUpdateFrequency   = time.Minute * 5
	changeQueryFrequency    = time.Minute * 5
	testTimeout             = time.Minute * 5  // timeout for a single test
	buildTimeout            = time.Minute * 10 // timeout for a build
	dailyUpdateTestListHour = 5                // 5am
	fullTestListRelPath     = "tests/regres/full-tests.json"
	ciTestListRelPath       = "tests/regres/ci-tests.json"
)

var (
	numParallelTests = runtime.NumCPU()

	deqpPath      = flag.String("deqp", "", "path to the deqp build directory")
	cacheDir      = flag.String("cache", "cache", "path to the output cache directory")
	gerritEmail   = flag.String("email", "$SS_REGRES_EMAIL", "gerrit email address for posting regres results")
	gerritUser    = flag.String("user", "$SS_REGRES_USER", "gerrit username for posting regres results")
	gerritPass    = flag.String("pass", "$SS_REGRES_PASS", "gerrit password for posting regres results")
	keepCheckouts = flag.Bool("keep", false, "don't delete checkout directories after use")
	dryRun        = flag.Bool("dry", false, "don't post regres reports to gerrit")
	maxProcMemory = flag.Uint64("max-proc-mem", shell.MaxProcMemory, "maximum virtual memory per child process")
)

func main() {
	if runtime.GOOS != "linux" {
		log.Fatal("regres only currently runs on linux")
	}

	flag.ErrHelp = errors.New("regres is a tool to detect regressions between versions of SwiftShader")
	flag.Parse()

	shell.MaxProcMemory = *maxProcMemory

	r := regres{
		deqpBuild:     *deqpPath,
		cacheRoot:     *cacheDir,
		gerritEmail:   os.ExpandEnv(*gerritEmail),
		gerritUser:    os.ExpandEnv(*gerritUser),
		gerritPass:    os.ExpandEnv(*gerritPass),
		keepCheckouts: *keepCheckouts,
		dryRun:        *dryRun,
	}

	if err := r.run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(-1)
	}
}

type regres struct {
	deqpBuild     string // path to the build directory of deqp
	cmake         string // path to cmake
	make          string // path to make
	cacheRoot     string // path to the regres cache directory
	gerritEmail   string // gerrit email address used for posting results
	gerritUser    string // gerrit username used for posting results
	gerritPass    string // gerrit password used for posting results
	keepCheckouts bool   // don't delete source & build checkouts after testing
	dryRun        bool   // don't post any reviews
	maxProcMemory uint64 // max virtual memory for child processes
}

// resolveDirs ensures that the necessary directories used can be found, and
// expands them to absolute paths.
func (r *regres) resolveDirs() error {
	allDirs := []*string{
		&r.deqpBuild,
		&r.cacheRoot,
	}

	for _, path := range allDirs {
		abs, err := filepath.Abs(*path)
		if err != nil {
			return cause.Wrap(err, "Couldn't find path '%v'", *path)
		}
		*path = abs
	}

	if err := os.MkdirAll(r.cacheRoot, 0777); err != nil {
		return cause.Wrap(err, "Couldn't create cache root directory")
	}

	for _, path := range allDirs {
		if _, err := os.Stat(*path); err != nil {
			return cause.Wrap(err, "Couldn't find path '%v'", *path)
		}
	}

	return nil
}

// resolveExes resolves all external executables used by regres.
func (r *regres) resolveExes() error {
	type exe struct {
		name string
		path *string
	}
	for _, e := range []exe{
		{"cmake", &r.cmake},
		{"make", &r.make},
	} {
		path, err := exec.LookPath(e.name)
		if err != nil {
			return cause.Wrap(err, "Couldn't find path to %s", e.name)
		}
		*e.path = path
	}
	return nil
}

// run performs the main processing loop for the regress tool. It:
// * Scans for open and recently updated changes in gerrit using queryChanges()
//   and changeInfo.update().
// * Builds the most recent patchset and the commit's parent CL using
//   r.newTest(<hash>).lazyRun().
// * Compares the results of the tests using compare().
// * Posts the results of the compare to gerrit as a review.
// * Repeats the above steps until the process is interrupted.
func (r *regres) run() error {
	if err := r.resolveExes(); err != nil {
		return cause.Wrap(err, "Couldn't resolve all exes")
	}

	if err := r.resolveDirs(); err != nil {
		return cause.Wrap(err, "Couldn't resolve all directories")
	}

	client, err := gerrit.NewClient(gerritURL, nil)
	if err != nil {
		return cause.Wrap(err, "Couldn't create gerrit client")
	}
	if r.gerritUser != "" {
		client.Authentication.SetBasicAuth(r.gerritUser, r.gerritPass)
	}

	changes := map[string]*changeInfo{} // Change ID -> changeInfo
	lastUpdatedTestLists := toDate(time.Now())
	lastQueriedChanges := time.Time{}

	for {
		if now := time.Now(); toDate(now) != lastUpdatedTestLists && now.Hour() >= dailyUpdateTestListHour {
			lastUpdatedTestLists = toDate(now)
			if err := r.updateTestLists(client); err != nil {
				log.Println(err.Error())
			}
		}

		// Update list of tracked changes.
		if time.Since(lastQueriedChanges) > changeQueryFrequency {
			lastQueriedChanges = time.Now()
			if err := queryChanges(client, changes); err != nil {
				log.Println(err.Error())
			}
		}

		// Update change info.
		for _, change := range changes {
			if time.Since(change.lastUpdated) > changeUpdateFrequency {
				change.lastUpdated = time.Now()
				err := change.update(client)
				if err != nil {
					log.Println(cause.Wrap(err, "Couldn't update info for change '%s'", change.id))
				}
			}
		}

		// Find the change with the highest priority.
		var change *changeInfo
		numPending := 0
		for _, c := range changes {
			if c.pending {
				numPending++
				if change == nil || c.priority > change.priority {
					change = c
				}
			}
		}

		if change == nil {
			// Everything up to date. Take a break.
			log.Println("Nothing to do. Sleeping")
			time.Sleep(time.Minute)
			continue
		}

		log.Printf("%d changes queued for testing\n", numPending)

		log.Printf("Testing change '%s'\n", change.id)

		// Test the latest patchset in the change, diff against parent change.
		msg, err := r.test(change)
		if err != nil {
			log.Println(cause.Wrap(err, "Failed to test changelist '%s'", change.latest))
			time.Sleep(time.Minute)
			continue
		}

		// Always include the reportHeader in the message.
		// changeInfo.update() uses this header to detect whether a patchset has
		// already got a test result.
		msg = reportHeader + "\n\n" + msg

		if r.dryRun {
			log.Printf("DRY RUN: add review to change '%v':\n%v\n", change.id, msg)
		} else {
			log.Printf("Posting review to '%s'\n", change.id)
			_, _, err = client.Changes.SetReview(change.id, change.latest.String(), &gerrit.ReviewInput{
				Message: msg,
				Tag:     "autogenerated:regress",
			})
			if err != nil {
				return cause.Wrap(err, "Failed to post comments on change '%s'", change.id)
			}
		}
		change.pending = false
	}
}

func (r *regres) test(change *changeInfo) (string, error) {
	log.Printf("Testing latest patchset for change '%s'\n", change.id)
	latest, testlists, err := r.testLatest(change)
	if err != nil {
		return "", cause.Wrap(err, "Failed to test latest change of '%v'", change.id)
	}

	log.Printf("Testing parent of change '%s'\n", change.id)
	parent, err := r.testParent(change, testlists)
	if err != nil {
		return "", cause.Wrap(err, "Failed to test parent change of '%v'", change.id)
	}

	log.Println("Comparing latest patchset's results with parent")
	msg := compare(parent, latest)

	return msg, nil
}

var additionalTestsRE = regexp.MustCompile(`\n\s*Test[s]?:\s*([^\s]+)[^\n]*`)

func (r *regres) testLatest(change *changeInfo) (*CommitTestResults, testlist.Lists, error) {
	// Get the test results for the latest patchset in the change.
	test := r.newTest(change.latest)
	defer test.cleanup()

	if err := test.checkout(); err != nil {
		return nil, nil, cause.Wrap(err, "Failed to checkout '%s'", change.latest)
	}

	testlists, err := test.loadTestLists(ciTestListRelPath)
	if err != nil {
		return nil, nil, cause.Wrap(err, "Failed to load '%s'", change.latest)
	}

	if matches := additionalTestsRE.FindAllStringSubmatch(change.commitMessage, -1); len(matches) > 0 {
		log.Println("Change description contains additional test patterns")

		// Change specifies additional tests to try. Load the full test list.
		fullTestLists, err := test.loadTestLists(fullTestListRelPath)
		if err != nil {
			return nil, nil, cause.Wrap(err, "Failed to load '%s'", change.latest)
		}

		// Add any tests in the full list that match the pattern to the list to test.
		for _, match := range matches {
			if len(match) > 1 {
				pattern := match[1]
				log.Printf("Adding custom tests with pattern '%s'\n", pattern)
				filtered := fullTestLists.Filter(func(name string) bool {
					ok, _ := filepath.Match(pattern, name)
					return ok
				})
				testlists = append(testlists, filtered...)
			}
		}
	}

	cachePath := test.resultsCachePath(testlists)

	if results, err := loadCommitTestResults(cachePath); err == nil {
		return results, testlists, nil // Use cached results
	}

	if err := test.build(); err != nil {
		return nil, nil, cause.Wrap(err, "Failed to build '%s'", change.latest)
	}

	results, err := test.run(testlists)
	if err != nil {
		return nil, nil, cause.Wrap(err, "Failed to test '%s'", change.latest)
	}

	// Cache the results for future tests
	if err := results.save(cachePath); err != nil {
		log.Printf("Warning: Couldn't save results of test to '%v'\n", cachePath)
	}

	return results, testlists, nil
}

func (r *regres) testParent(change *changeInfo, testlists testlist.Lists) (*CommitTestResults, error) {
	// Get the test results for the changes's parent changelist.
	test := r.newTest(change.parent)
	defer test.cleanup()

	cachePath := test.resultsCachePath(testlists)

	if results, err := loadCommitTestResults(cachePath); err == nil {
		return results, nil // Use cached results
	}

	// Couldn't load cached results. Have to build them.
	if err := test.checkout(); err != nil {
		return nil, cause.Wrap(err, "Failed to checkout '%s'", change.parent)
	}

	// Build the parent change.
	if err := test.build(); err != nil {
		return nil, cause.Wrap(err, "Failed to build '%s'", change.parent)
	}

	// Run the tests on the parent change.
	results, err := test.run(testlists)
	if err != nil {
		return nil, cause.Wrap(err, "Failed to test '%s'", change.parent)
	}

	// Store the results of the parent change to the cache.
	if err := results.save(cachePath); err != nil {
		log.Printf("Warning: Couldn't save results of test to '%v'\n", cachePath)
	}

	return results, nil
}

func (r *regres) updateTestLists(client *gerrit.Client) error {
	log.Println("Updating test lists")

	headHash, err := git.FetchRefHash("HEAD", gitURL)
	if err != nil {
		return cause.Wrap(err, "Could not get hash of master HEAD")
	}

	// Get the full test results for latest master.
	test := r.newTest(headHash)
	defer test.cleanup()

	testLists, err := test.loadTestLists(fullTestListRelPath)
	if err != nil {
		return cause.Wrap(err, "Failed to load full test lists for '%s'", headHash)
	}

	// Couldn't load cached results. Have to build them.
	if err := test.checkout(); err != nil {
		return cause.Wrap(err, "Failed to checkout '%s'", headHash)
	}

	// Build the change.
	if err := test.build(); err != nil {
		return cause.Wrap(err, "Failed to build '%s'", headHash)
	}

	// Run the tests on the change.
	results, err := test.run(testLists)
	if err != nil {
		return cause.Wrap(err, "Failed to test '%s'", headHash)
	}

	// Write out the test list status files.
	filePaths, err := test.writeTestListsByStatus(testLists, results)
	if err != nil {
		return cause.Wrap(err, "Failed to write test lists by status")
	}

	// Stage all the updated test files.
	for _, path := range filePaths {
		log.Println("Staging", path)
		git.Add(test.srcDir, path)
	}

	log.Println("Checking for existing test list")
	changes, _, err := client.Changes.QueryChanges(&gerrit.QueryChangeOptions{
		QueryOptions: gerrit.QueryOptions{
			Query: []string{fmt.Sprintf(`status:open+owner:"%v"`, r.gerritEmail)},
			Limit: 1,
		},
	})
	if err != nil {
		return cause.Wrap(err, "Failed to checking for existing test list")
	}

	commitMsg := strings.Builder{}
	commitMsg.WriteString("Regres: Update test lists @ " + headHash.String()[:8])
	if results != nil && len(*changes) > 0 {
		// Reuse gerrit change ID if there's already a change up for review.
		id := (*changes)[0].ChangeID
		commitMsg.WriteString("\n\n")
		commitMsg.WriteString("Change-Id: " + id)
	}

	if err := git.Commit(test.srcDir, commitMsg.String(), git.CommitFlags{
		Name:  "SwiftShader Regression Bot",
		Email: r.gerritEmail,
	}); err != nil {
		return cause.Wrap(err, "Failed to commit test results")
	}

	if r.dryRun {
		log.Printf("DRY RUN: post results for review")
	} else {
		log.Println("Pushing test results for review")
		if err := git.Push(test.srcDir, gitURL, "HEAD", "refs/for/master", git.PushFlags{
			Username: r.gerritUser,
			Password: r.gerritPass,
		}); err != nil {
			return cause.Wrap(err, "Failed to push test results for review")
		}
		log.Println("Test results posted for review")
	}

	return nil
}

// changeInfo holds the important information about a single, open change in
// gerrit.
type changeInfo struct {
	id            string    // Gerrit change ID.
	pending       bool      // Is this change waiting a test for the latest patchset?
	priority      int       // Calculated priority based on Gerrit labels.
	latest        git.Hash  // Git hash of the latest patchset in the change.
	parent        git.Hash  // Git hash of the changelist this change is based on.
	lastUpdated   time.Time // Time the change was last fetched.
	commitMessage string
}

// queryChanges updates the changes map by querying gerrit for the latest open
// changes.
func queryChanges(client *gerrit.Client, changes map[string]*changeInfo) error {
	log.Println("Checking for latest changes")
	results, _, err := client.Changes.QueryChanges(&gerrit.QueryChangeOptions{
		QueryOptions: gerrit.QueryOptions{
			Query: []string{"status:open+-age:3d"},
			Limit: 100,
		},
	})
	if err != nil {
		return cause.Wrap(err, "Failed to get list of changes")
	}

	ids := map[string]bool{}
	for _, r := range *results {
		ids[r.ChangeID] = true
	}

	// Add new changes
	for id := range ids {
		if _, found := changes[id]; !found {
			log.Printf("Tracking new change '%v'\n", id)
			changes[id] = &changeInfo{id: id}
		}
	}

	// Remove old changes
	for id := range changes {
		if found := ids[id]; !found {
			log.Printf("Untracking change '%v'\n", id)
			delete(changes, id)
		}
	}

	return nil
}

// update queries gerrit for information about the given change.
func (c *changeInfo) update(client *gerrit.Client) error {
	change, _, err := client.Changes.GetChange(c.id, &gerrit.ChangeOptions{
		AdditionalFields: []string{"CURRENT_REVISION", "CURRENT_COMMIT", "MESSAGES", "LABELS"},
	})
	if err != nil {
		return cause.Wrap(err, "Getting info for change '%s'", c.id)
	}

	current, ok := change.Revisions[change.CurrentRevision]
	if !ok {
		return fmt.Errorf("Couldn't find current revision for change '%s'", c.id)
	}

	if len(current.Commit.Parents) == 0 {
		return fmt.Errorf("Couldn't find current commit for change '%s' has no parents(?)", c.id)
	}

	kokoroPresubmit := change.Labels["Kokoro-Presubmit"].Approved.AccountID != 0
	codeReviewScore := change.Labels["Code-Review"].Value
	presubmitReady := change.Labels["Presubmit-Ready"].Approved.AccountID != 0

	c.priority = 0
	if presubmitReady {
		c.priority += 10
	}
	c.priority += codeReviewScore
	if kokoroPresubmit {
		c.priority++
	}

	// Is the change from a Googler?
	canTest := strings.HasSuffix(current.Commit.Committer.Email, "@google.com")

	// Has the latest patchset already been tested?
	if canTest {
		for _, msg := range change.Messages {
			if msg.RevisionNumber == current.Number &&
				strings.Contains(msg.Message, reportHeader) {
				canTest = false
				break
			}
		}
	}

	c.pending = canTest
	c.latest = git.ParseHash(change.CurrentRevision)
	c.parent = git.ParseHash(current.Commit.Parents[0].Commit)
	c.commitMessage = current.Commit.Message

	return nil
}

func (r *regres) newTest(commit git.Hash) *test {
	srcDir := filepath.Join(r.cacheRoot, "src", commit.String())
	resDir := filepath.Join(r.cacheRoot, "res", commit.String())
	return &test{
		r:        r,
		commit:   commit,
		srcDir:   srcDir,
		resDir:   resDir,
		outDir:   filepath.Join(srcDir, "out"),
		buildDir: filepath.Join(srcDir, "build"),
	}
}

type test struct {
	r             *regres
	commit        git.Hash // hash of the commit to test
	srcDir        string   // directory for the SwiftShader checkout
	resDir        string   // directory for the test results
	outDir        string   // directory for SwiftShader output
	buildDir      string   // directory for SwiftShader build
	keepCheckouts bool     // don't delete source & build checkouts after testing
}

// cleanup removes any temporary files used by the test.
func (t *test) cleanup() {
	if t.srcDir != "" && !t.keepCheckouts {
		os.RemoveAll(t.srcDir)
	}
}

// checkout clones the test's source commit into t.src.
func (t *test) checkout() error {
	if isDir(t.srcDir) && t.keepCheckouts {
		log.Printf("Reusing source cache for commit '%s'\n", t.commit)
		return nil
	}
	log.Printf("Checking out '%s'\n", t.commit)
	os.RemoveAll(t.srcDir)
	if err := git.Checkout(t.srcDir, gitURL, t.commit); err != nil {
		return cause.Wrap(err, "Checking out commit '%s'", t.commit)
	}
	log.Printf("Checked out commit '%s'\n", t.commit)
	return nil
}

// build builds the SwiftShader source into t.buildDir.
func (t *test) build() error {
	log.Printf("Building '%s'\n", t.commit)

	if err := os.MkdirAll(t.buildDir, 0777); err != nil {
		return cause.Wrap(err, "Failed to create build directory")
	}

	if err := shell.Shell(buildTimeout, t.r.cmake, t.buildDir,
		"-DCMAKE_BUILD_TYPE=Release",
		"-DDCHECK_ALWAYS_ON=1",
		".."); err != nil {
		return err
	}

	if err := shell.Shell(buildTimeout, t.r.make, t.buildDir, fmt.Sprintf("-j%d", runtime.NumCPU())); err != nil {
		return err
	}

	return nil
}

// run runs all the tests.
func (t *test) run(testLists testlist.Lists) (*CommitTestResults, error) {
	log.Printf("Running tests for '%s'\n", t.commit)
	start := time.Now()

	// Wait group that completes once all the tests have finished.
	wg := sync.WaitGroup{}
	results := make(chan TestResult, 256)

	numTests := 0

	// For each API that we are testing
	for _, list := range testLists {
		// Resolve the test runner
		var exe string
		switch list.API {
		case testlist.EGL:
			exe = filepath.Join(t.r.deqpBuild, "modules", "egl", "deqp-egl")
		case testlist.GLES2:
			exe = filepath.Join(t.r.deqpBuild, "modules", "gles2", "deqp-gles2")
		case testlist.GLES3:
			exe = filepath.Join(t.r.deqpBuild, "modules", "gles3", "deqp-gles3")
		case testlist.Vulkan:
			exe = filepath.Join(t.r.deqpBuild, "external", "vulkancts", "modules", "vulkan", "deqp-vk")
		default:
			return nil, fmt.Errorf("Unknown API '%v'", list.API)
		}
		if !isFile(exe) {
			return nil, fmt.Errorf("Couldn't find dEQP executable at '%s'", exe)
		}

		// Build a chan for the test names to be run.
		tests := make(chan string, len(list.Tests))

		// Start a number of go routines to run the tests.
		wg.Add(numParallelTests)
		for i := 0; i < numParallelTests; i++ {
			go func() {
				t.deqpTestRoutine(exe, tests, results)
				wg.Done()
			}()
		}

		// Hand the tests to the deqpTestRoutines.
		for _, t := range list.Tests {
			tests <- t
		}

		// Close the tests chan to indicate that there are no more tests to run.
		// The deqpTestRoutine functions will return once all tests have been
		// run.
		close(tests)

		numTests += len(list.Tests)
	}

	out := CommitTestResults{
		Version: dataVersion,
		Built:   true,
		Tests:   map[string]TestResult{},
	}

	// Collect the results.
	finished := make(chan struct{})
	lastUpdate := time.Now()
	go func() {
		start, i := time.Now(), 0
		for r := range results {
			i++
			out.Tests[r.Test] = r
			if time.Since(lastUpdate) > time.Minute {
				lastUpdate = time.Now()
				remaining := numTests - i
				log.Printf("Ran %d/%d tests (%v%%). Estimated completion in %v.\n",
					i, numTests, percent(i, numTests),
					(time.Since(start)/time.Duration(i))*time.Duration(remaining))
			}
		}
		close(finished)
	}()

	wg.Wait()      // Block until all the deqpTestRoutines have finished.
	close(results) // Signal no more results.
	<-finished     // And wait for the result collecting go-routine to finish.

	out.Duration = time.Since(start)

	return &out, nil
}

func (t *test) writeTestListsByStatus(testLists testlist.Lists, results *CommitTestResults) ([]string, error) {
	out := []string{}

	for _, list := range testLists {
		files := map[Status]*os.File{}
		ext := filepath.Ext(list.File)
		name := list.File[:len(list.File)-len(ext)]
		for _, status := range Statuses {
			path := filepath.Join(t.srcDir, name+"-"+string(status)+ext)
			dir := filepath.Dir(path)
			os.MkdirAll(dir, 0777)
			f, err := os.Create(path)
			if err != nil {
				return nil, cause.Wrap(err, "Couldn't create file '%v'", path)
			}
			defer f.Close()
			files[status] = f

			out = append(out, path)
		}

		for _, testName := range list.Tests {
			if r, found := results.Tests[testName]; found {
				fmt.Fprintln(files[r.Status], testName)
			}
		}
	}

	return out, nil
}

// resultsCachePath returns the path to the cache results file for the given
// test and testlists.
func (t *test) resultsCachePath(testLists testlist.Lists) string {
	return filepath.Join(t.resDir, testLists.Hash())
}

// Status is an enumerator of test results.
type Status string

const (
	// Pass is the status of a successful test.
	Pass = Status("PASS")
	// Fail is the status of a failed test.
	Fail = Status("FAIL")
	// Timeout is the status of a test that failed to complete in the alloted
	// time.
	Timeout = Status("TIMEOUT")
	// Crash is the status of a test that crashed.
	Crash = Status("CRASH")
	// NotSupported is the status of a test feature not supported by the driver.
	NotSupported = Status("NOT_SUPPORTED")
	// CompatibilityWarning is the status passing test with a warning.
	CompatibilityWarning = Status("COMPATIBILITY_WARNING")
	// QualityWarning is the status passing test with a warning.
	QualityWarning = Status("QUALITY_WARNING")
)

// Statuses is the full list of status types
var Statuses = []Status{Pass, Fail, Timeout, Crash, NotSupported, CompatibilityWarning, QualityWarning}

// Failing returns true if the task status requires fixing.
func (s Status) Failing() bool {
	switch s {
	case Fail, Timeout, Crash:
		return true
	default:
		return false
	}
}

// Passing returns true if the task status is considered a pass.
func (s Status) Passing() bool {
	switch s {
	case Pass, CompatibilityWarning, QualityWarning:
		return true
	default:
		return false
	}
}

// CommitTestResults holds the results the tests across all APIs for a given
// commit. The CommitTestResults structure may be serialized to cache the
// results.
type CommitTestResults struct {
	Version  int
	Built    bool
	Tests    map[string]TestResult
	Duration time.Duration
}

func loadCommitTestResults(path string) (*CommitTestResults, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, cause.Wrap(err, "Couldn't open '%s' for loading test results", path)
	}
	defer f.Close()

	var out CommitTestResults
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return nil, err
	}
	if out.Version != dataVersion {
		return nil, errors.New("Data is from an old version")
	}
	return &out, nil
}

func (r *CommitTestResults) save(path string) error {
	os.MkdirAll(filepath.Dir(path), 0777)

	f, err := os.Create(path)
	if err != nil {
		return cause.Wrap(err, "Couldn't open '%s' for saving test results", path)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return cause.Wrap(err, "Couldn't encode test results")
	}

	return nil
}

// compare returns a string describing all differences between two
// CommitTestResults. This string is used as the report message posted to the
// gerrit code review.
func compare(old, new *CommitTestResults) string {
	switch {
	case !old.Built && !new.Built:
		return "Build continues to be broken."
	case old.Built && !new.Built:
		return "Build broken."
	case !old.Built && !new.Built:
		return "Build now fixed. Cannot compare against broken parent."
	}

	oldStatusCounts, newStatusCounts := map[Status]int{}, map[Status]int{}
	totalTests := 0

	broken, fixed, failing, removed, changed := []string{}, []string{}, []string{}, []string{}, []string{}

	for test, new := range new.Tests {
		old, found := old.Tests[test]
		switch {
		case found && old.Status.Passing() && new.Status.Failing():
			broken = append(broken, test)
		case found && old.Status.Failing() && new.Status.Passing():
			fixed = append(fixed, test)
		case found && old.Status != new.Status:
			changed = append(changed, test)
		case found && old.Status.Failing() && new.Status.Failing():
			failing = append(failing, test) // Still broken
		}
		totalTests++
		if found {
			oldStatusCounts[old.Status] = oldStatusCounts[old.Status] + 1
		}
		newStatusCounts[new.Status] = newStatusCounts[new.Status] + 1
	}

	for test := range old.Tests {
		if _, found := new.Tests[test]; !found {
			removed = append(removed, test)
		}
	}

	sb := strings.Builder{}

	// list prints the list l to sb, truncating after a limit.
	list := func(l []string) {
		const max = 10
		for i, s := range l {
			sb.WriteString("  ")
			if i == max {
				sb.WriteString(fmt.Sprintf("> %d more\n", len(l)-i))
				break
			}
			sb.WriteString(fmt.Sprintf("> %s", s))
			if n, ok := new.Tests[s]; ok {
				if o, ok := old.Tests[s]; ok && n != o {
					sb.WriteString(fmt.Sprintf(" - [%s -> %s]", o.Status, n.Status))
				} else {
					sb.WriteString(fmt.Sprintf(" - [%s]", n.Status))
				}
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(fmt.Sprintf("          Total tests: %d\n", totalTests))
	for _, s := range []struct {
		label  string
		status Status
	}{
		{"                 Pass", Pass},
		{"                 Fail", Fail},
		{"              Timeout", Timeout},
		{"                Crash", Crash},
		{"        Not Supported", NotSupported},
		{"Compatibility Warning", CompatibilityWarning},
		{"      Quality Warning", QualityWarning},
	} {
		old, new := oldStatusCounts[s.status], newStatusCounts[s.status]
		if old == 0 && new == 0 {
			continue
		}
		change := percent64(int64(new-old), int64(old))
		switch {
		case old == new:
			sb.WriteString(fmt.Sprintf("%s: %v\n", s.label, new))
		case change == 0:
			sb.WriteString(fmt.Sprintf("%s: %v -> %v (%+d)\n", s.label, old, new, new-old))
		default:
			sb.WriteString(fmt.Sprintf("%s: %v -> %v (%+d %+d%%)\n", s.label, old, new, new-old, change))
		}
	}

	if old, new := old.Duration, new.Duration; old != 0 && new != 0 {
		label := "           Time taken"
		change := percent64(int64(new-old), int64(old))
		switch {
		case old == new:
			sb.WriteString(fmt.Sprintf("%s: %v\n", label, new))
		case change == 0:
			sb.WriteString(fmt.Sprintf("%s: %v -> %v\n", label, old, new))
		default:
			sb.WriteString(fmt.Sprintf("%s: %v -> %v (%+d%%)\n", label, old, new, change))
		}
	}

	if n := len(broken); n > 0 {
		sort.Strings(broken)
		sb.WriteString(fmt.Sprintf("\n--- This change breaks %d tests: ---\n", n))
		list(broken)
	}
	if n := len(fixed); n > 0 {
		sort.Strings(fixed)
		sb.WriteString(fmt.Sprintf("\n--- This change fixes %d tests: ---\n", n))
		list(fixed)
	}
	if n := len(removed); n > 0 {
		sort.Strings(removed)
		sb.WriteString(fmt.Sprintf("\n--- This change removes %d tests: ---\n", n))
		list(removed)
	}
	if n := len(changed); n > 0 {
		sort.Strings(changed)
		sb.WriteString(fmt.Sprintf("\n--- This change alters %d tests: ---\n", n))
		list(changed)
	}

	if len(broken) == 0 && len(fixed) == 0 && len(removed) == 0 && len(changed) == 0 {
		sb.WriteString(fmt.Sprintf("\n--- No change in test results ---\n"))
	}

	return sb.String()
}

// TestResult holds the results of a single API test.
type TestResult struct {
	Test   string
	Status Status
	Err    string `json:",omitempty"`
}

func (r TestResult) String() string {
	if r.Err != "" {
		return fmt.Sprintf("%s: %s (%s)", r.Test, r.Status, r.Err)
	}
	return fmt.Sprintf("%s: %s", r.Test, r.Status)
}

// Regular expression to parse the output of a dEQP test.
var parseRE = regexp.MustCompile(`(Fail|Pass|NotSupported|CompatibilityWarning|QualityWarning) \(([\s\S]*)\)`)

// deqpTestRoutine repeatedly runs the dEQP test executable exe with the tests
// taken from tests. The output of the dEQP test is parsed, and the test result
// is written to results.
// deqpTestRoutine only returns once the tests chan has been closed.
// deqpTestRoutine does not close the results chan.
func (t *test) deqpTestRoutine(exe string, tests <-chan string, results chan<- TestResult) {
	for name := range tests {
		// log.Printf("Running test '%s'\n", name)
		env := []string{
			"LD_LIBRARY_PATH=" + t.buildDir + ":" + os.Getenv("LD_LIBRARY_PATH"),
			"VK_ICD_FILENAMES=" + filepath.Join(t.outDir, "Linux", "vk_swiftshader_icd.json"),
			"DISPLAY=" + os.Getenv("DISPLAY"),
			"LIBC_FATAL_STDERR_=1", // Put libc explosions into logs.
		}

		out, err := shell.Exec(testTimeout, exe, filepath.Dir(exe), env, "--deqp-surface-type=pbuffer", "-n="+name)
		switch err.(type) {
		default:
			results <- TestResult{
				Test:   name,
				Status: Crash,
				Err:    cause.Wrap(err, string(out)).Error(),
			}
		case shell.ErrTimeout:
			results <- TestResult{
				Test:   name,
				Status: Timeout,
				Err:    cause.Wrap(err, string(out)).Error(),
			}
		case nil:
			toks := parseRE.FindStringSubmatch(string(out))
			if len(toks) < 3 {
				err := fmt.Sprintf("Couldn't parse test '%v' output:\n%s", name, string(out))
				log.Println("Warning: ", err)
				results <- TestResult{Test: name, Status: Fail, Err: err}
				continue
			}
			switch toks[1] {
			case "Pass":
				results <- TestResult{Test: name, Status: Pass}
			case "NotSupported":
				results <- TestResult{Test: name, Status: NotSupported}
			case "CompatibilityWarning":
				results <- TestResult{Test: name, Status: CompatibilityWarning}
			case "QualityWarning":
				results <- TestResult{Test: name, Status: QualityWarning}
			case "Fail":
				var err string
				if toks[2] != "Fail" {
					err = toks[2]
				}
				results <- TestResult{Test: name, Status: Fail, Err: err}
			default:
				err := fmt.Sprintf("Couldn't parse test output:\n%s", string(out))
				log.Println("Warning: ", err)
				results <- TestResult{Test: name, Status: Fail, Err: err}
			}
		}
	}
}

// loadTestLists loads the full test lists from the json file.
// The file is first searched at {t.srcDir}/{relPath}
// If this cannot be found, then the file is searched at the fallback path
// {CWD}/{relPath}
// This allows CLs to alter the list of tests to be run, as well as providing
// a default set.
func (t *test) loadTestLists(relPath string) (testlist.Lists, error) {
	// Seach for the test.json file in the checked out source directory.
	if path := filepath.Join(t.srcDir, relPath); isFile(path) {
		log.Printf("Loading test list '%v' from commit\n", relPath)
		return testlist.Load(t.srcDir, path)
	}

	// Not found there. Search locally.
	wd, err := os.Getwd()
	if err != nil {
		return testlist.Lists{}, cause.Wrap(err, "Couldn't get current working directory")
	}
	if path := filepath.Join(wd, relPath); isFile(path) {
		log.Printf("Loading test list '%v' from regres\n", relPath)
		return testlist.Load(wd, relPath)
	}

	return nil, errors.New("Couldn't find a test list file")
}

// isDir returns true if path is a file.
func isFile(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !s.IsDir()
}

// isDir returns true if path is a directory.
func isDir(path string) bool {
	s, err := os.Stat(path)
	if err != nil {
		return false
	}
	return s.IsDir()
}

// percent returns the percentage completion of i items out of n.
func percent(i, n int) int {
	return int(percent64(int64(i), int64(n)))
}

// percent64 returns the percentage completion of i items out of n.
func percent64(i, n int64) int64 {
	if n == 0 {
		return 0
	}
	return (100 * i) / n
}

type date struct {
	year  int
	month time.Month
	day   int
}

func toDate(t time.Time) date {
	d := date{}
	d.year, d.month, d.day = t.Date()
	return d
}