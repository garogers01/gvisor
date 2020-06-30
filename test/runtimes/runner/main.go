// Copyright 2019 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Binary runner runs the runtime tests in a Docker container.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/test/dockerutil"
	"gvisor.dev/gvisor/pkg/test/testutil"
)

var (
	lang        = flag.String("lang", "", "language runtime to test")
	image       = flag.String("image", "", "docker image with runtime tests")
	excludeFile = flag.String("exclude_file", "", "file containing list of tests to exclude, in CSV format with fields: test name, bug id, comment")
)

// Wait time for each test to run.
const timeout = 5 * time.Minute

func main() {
	flag.Parse()
	if *lang == "" || *image == "" {
		fmt.Fprintf(os.Stderr, "lang and image flags must not be empty\n")
		os.Exit(1)
	}
	os.Exit(runTests())
}

// runTests is a helper that is called by main. It exists so that we can run
// defered functions before exiting. It returns an exit code that should be
// passed to os.Exit.
func runTests() int {
	// Get tests to exclude..
	excludes, err := getExcludes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting exclude list: %s\n", err.Error())
		return 1
	}

	// Construct the shared docker instance.
	d := dockerutil.MakeContainer(testutil.DefaultLogger(*lang))
	defer d.CleanUp()

	// Get a slice of tests to run. This will also start a single Docker
	// container that will be used to run each test. The final test will
	// stop the Docker container.
	tests, err := getTests(d, excludes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		return 1
	}

	m := testing.MainStart(testDeps{}, tests, nil, nil)
	return m.Run()
}

// getTests executes all tests as table tests.
func getTests(d *dockerutil.Container, excludes map[string]struct{}) ([]testing.InternalTest, error) {
	// Start the container.
	opts := dockerutil.RunOpts{
		Image: fmt.Sprintf("runtimes/%s", *image),
	}
	d.CopyFiles(&opts, "/proctor", "test/runtimes/proctor/proctor")
	if err := d.Spawn(opts, "/proctor/proctor", "--pause"); err != nil {
		return nil, fmt.Errorf("docker run failed: %v", err)
	}

	// Get a list of all tests in the image.
	list, err := d.Exec(dockerutil.ExecOpts{}, "/proctor/proctor", "--runtime", *lang, "--list")
	if err != nil {
		return nil, fmt.Errorf("docker exec failed: %v", err)
	}

	// Calculate a subset of tests to run corresponding to the current
	// shard.
	tests := strings.Fields(list)
	sort.Strings(tests)
	indices, err := testutil.TestIndicesForShard(len(tests))
	if err != nil {
		return nil, fmt.Errorf("TestsForShard() failed: %v", err)
	}

	var itests []testing.InternalTest
	for _, tci := range indices {
		// Capture tc in this scope.
		tc := tests[tci]
		itests = append(itests, testing.InternalTest{
			Name: tc,
			F: func(t *testing.T) {
				// Is the test excluded?
				if _, ok := excludes[tc]; ok {
					t.Skipf("SKIP: excluded test %q", tc)
				}

				var (
					now    = time.Now()
					done   = make(chan struct{})
					output string
					err    error
				)

				go func() {
					fmt.Printf("RUNNING %s...\n", tc)
					output, err = d.Exec(dockerutil.ExecOpts{}, "/proctor/proctor", "--runtime", *lang, "--test", tc)
					close(done)
				}()

				select {
				case <-done:
					if err == nil {
						fmt.Printf("PASS: %s (%v)\n\n", tc, time.Since(now))
						return
					}
					t.Errorf("FAIL: %s (%v):\n%s\n", tc, time.Since(now), output)
				case <-time.After(timeout):
					t.Errorf("TIMEOUT: %s (%v):\n%s\n", tc, time.Since(now), output)
				}
			},
		})
	}

	return itests, nil
}

// getBlacklist reads the exclude file and returns a set of test names to
// exclude.
func getExcludes() (map[string]struct{}, error) {
	excludes := make(map[string]struct{})
	if *excludeFile == "" {
		return excludes, nil
	}
	f, err := os.Open(*excludeFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)

	// First line is header. Skip it.
	if _, err := r.Read(); err != nil {
		return nil, err
	}

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		excludes[record[0]] = struct{}{}
	}
	return excludes, nil
}

// testDeps implements testing.testDeps (an unexported interface), and is
// required to use testing.MainStart.
type testDeps struct{}

func (f testDeps) MatchString(a, b string) (bool, error)       { return a == b, nil }
func (f testDeps) StartCPUProfile(io.Writer) error             { return nil }
func (f testDeps) StopCPUProfile()                             {}
func (f testDeps) WriteProfileTo(string, io.Writer, int) error { return nil }
func (f testDeps) ImportPath() string                          { return "" }
func (f testDeps) StartTestLog(io.Writer)                      {}
func (f testDeps) StopTestLog() error                          { return nil }
