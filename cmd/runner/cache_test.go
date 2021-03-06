package main

// This file contains an number of explicit unit tests design to
// validate the caching layer that is difficult to do in a black box
// functional test.

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SentientTechnologies/studio-go-runner"

	"github.com/go-stack/stack"
	"github.com/karlmutch/ccache"
	"github.com/karlmutch/errors"

	humanize "github.com/dustin/go-humanize"

	"github.com/rs/xid" // MIT
	// Apache 2.0
)

func outputMetrics(metricsURL string) (err errors.Error) {

	resp, errGo := http.Get(metricsURL)
	if errGo != nil {
		return errors.Wrap(errGo).With("URL", metricsURL).With("stack", stack.Trace().TrimRuntime())
	}
	defer resp.Body.Close()

	body, errGo := ioutil.ReadAll(resp.Body)
	if errGo != nil {
		return errors.Wrap(errGo).With("URL", metricsURL).With("stack", stack.Trace().TrimRuntime())
	}

	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "runner_cache_") {
			logger.Info(line)
		}
	}
	return nil
}

func getHitsMisses(metricsURL string, hash string) (hits int, misses int, err errors.Error) {
	hits = 0
	misses = 0

	resp, errGo := http.Get(metricsURL)
	if errGo != nil {
		return -1, -1, errors.Wrap(errGo).With("URL", metricsURL).With("stack", stack.Trace().TrimRuntime())
	}
	defer resp.Body.Close()

	body, errGo := ioutil.ReadAll(resp.Body)
	if errGo != nil {
		return -1, -1, errors.Wrap(errGo).With("URL", metricsURL).With("stack", stack.Trace().TrimRuntime())
	}

	hashData := "hash=\"" + hash + "\""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, hashData) && strings.HasPrefix(line, "runner_cache") {
			values := strings.Split(line, " ")
			switch {
			case strings.HasPrefix(line, "runner_cache_hits{"):
				hits, _ = strconv.Atoi(values[len(values)-1])
			case strings.HasPrefix(line, "runner_cache_misses{"):
				misses, _ = strconv.Atoi(values[len(values)-1])
			}
		}
	}
	return hits, misses, nil
}

func tmpDirFile(size int64) (dir string, fn string, err errors.Error) {

	tmpDir, errGo := ioutil.TempDir("", xid.New().String())
	if errGo != nil {
		return "", "", errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime())
	}

	fn = path.Join(tmpDir, xid.New().String())
	f, errGo := os.Create(fn)
	if errGo != nil {
		return "", "", errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime())
	}
	defer f.Close()

	if errGo = f.Truncate(size); errGo != nil {
		return "", "", errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime())
	}

	return tmpDir, fn, nil
}

func uploadTestFile(bucket string, key string, size int64) (err errors.Error) {
	tmpDir, fn, err := tmpDirFile(size)
	if err != nil {
		return err
	}
	defer func() {
		if errGo := os.RemoveAll(tmpDir); errGo != nil {
			fmt.Printf("%s %#v", tmpDir, errGo)
		}
	}()

	// Get the Minio Test Server instance and sent it some random data while generating
	// a hash
	return runner.MinioTest.Upload(bucket, key, fn)
}

func TestCacheBase(t *testing.T) {
	logger = runner.NewLogger("cache_base_test")
	cache := ccache.New(ccache.Configure().GetsPerPromote(1).MaxSize(5).ItemsToPrune(1))
	for i := 0; i < 7; i++ {
		cache.Set(strconv.Itoa(i), i, time.Minute)
	}
	time.Sleep(time.Millisecond * 10)
	if cache.Get("0") != nil {
		t.Fatal(errors.New("unexpected entry in cache").With("stack", stack.Trace().TrimRuntime()))
	}
	if cache.Get("1") != nil {
		t.Fatal(errors.New("unexpected entry in cache").With("stack", stack.Trace().TrimRuntime()))
	}
	if cache.Get("2").Value() != 2 {
		t.Fatal(errors.New("expected entry NOT in cache").With("stack", stack.Trace().TrimRuntime()))
	}
	logger.Info("TestCacheBase completed")
}

// TestCacheLoad will validate that fetching a file with an empty cache will
// trigger a fetch and then immediately followed by the same fetch will
// trigger a cache hit
//
func TestCacheLoad(t *testing.T) {

	prometheusURL := fmt.Sprintf("http://localhost:%d/metrics", PrometheusPort)

	if !CacheActive {
		t.Skip("cache not activate")
	}

	// This will erase any files from the artifact cache so that the test can
	// run unobstructed
	runner.ClearObjStore()
	defer runner.ClearObjStore()

	logger = runner.NewLogger("cache_load_test")

	bucket := "testcacheload"
	fn := "file-1"

	if err := uploadTestFile(bucket, fn, humanize.MiByte); err != nil {
		t.Fatal(err)
	}

	defer func() {
		for _, err := range runner.MinioTest.RemoveBucketAll(bucket) {
			logger.Warn(err.Error())
		}
	}()

	tmpDir, errGo := ioutil.TempDir("", xid.New().String())
	if errGo != nil {
		t.Fatal(errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()))
	}
	defer os.RemoveAll(tmpDir)

	// Build an artifact cache in the same manner as is used by the main studioml
	// runner implementation
	artifactCache = runner.NewArtifactCache()

	art := runner.Artifact{
		Bucket:    bucket,
		Key:       fn,
		Mutable:   false,
		Unpack:    false,
		Qualified: fmt.Sprintf("s3://%s/%s/%s", runner.MinioTest.Address, bucket, fn),
	}
	env := map[string]string{
		"AWS_ACCESS_KEY_ID":     runner.MinioTest.AccessKeyId,
		"AWS_SECRET_ACCESS_KEY": runner.MinioTest.SecretAccessKeyId,
		"AWS_DEFAULT_REGION":    "us-west-2",
	}

	hash, err := artifactCache.Hash(&art, "project", tmpDir, "", env, "")
	if err != nil {
		t.Fatal(err)
	}

	// Extract the starting metrics for the server under going this test
	hits, misses, err := getHitsMisses(prometheusURL, hash)
	if err != nil {
		t.Fatal(err)
	}

	// In production the files would be downloaded to an experiment dir,
	// in the testing case we use a temporary directory as your artifact
	// group then wipe it when the test is done
	//
	warns, err := artifactCache.Fetch(&art, "project", tmpDir, "", env, "")
	if err != nil {
		for _, w := range warns {
			logger.Warn(w.Error())
		}
		t.Fatal(err)
	}

	// Run a fetch and ensure we have a miss and no change to the hits
	//
	newHits, newMisses, err := getHitsMisses(prometheusURL, hash)
	if err != nil {
		t.Fatal(err)
	}

	// Run a fetch and ensure we have a miss and no change to the hits
	if misses+1 != newMisses {
		t.Fatal(errors.New("new file did not result in a miss").With("hash", hash).With("stack", stack.Trace().TrimRuntime()))
	}
	if hits != newHits {
		t.Fatal(errors.New("new file unexpectedly resulted in a hit").With("hash", hash).With("stack", stack.Trace().TrimRuntime()))
	}

	// Refetch the file
	logger.Info("fetching file from warm cache")
	if warns, err = artifactCache.Fetch(&art, "project", tmpDir, "", env, ""); err != nil {
		for _, w := range warns {
			logger.Warn(w.Error())
		}
		t.Fatal(err)
	}

	newHits, newMisses, err = getHitsMisses(prometheusURL, hash)
	if err != nil {
		t.Fatal(err)
	}
	if hits+1 != newHits {
		t.Fatal(errors.New("existing file did not result in a hit when cache active").With("hash", hash).With("hits", newHits).With("misses", newMisses).With("stack", stack.Trace().TrimRuntime()))
	}
	if misses+1 != newMisses {
		t.Fatal(errors.New("existing file resulted in a miss when cache active").With("hash", hash).With("stack", stack.Trace().TrimRuntime()))
	}

	logger.Info("TestCacheLoad completed")
}

// TestCacheXhaust will fill the cache to capacity with 11 files each of 10% the size
// of the cache and will then make sure that the first file was groomed out by
// the subsequent loads
//
func TestCacheXhaust(t *testing.T) {

	prometheusURL := fmt.Sprintf("http://localhost:%d/metrics", PrometheusPort)

	if !CacheActive {
		t.Skip("cache not activate")
	}

	// This will erase any files from the artifact cache so that the test can
	// run unobstructed
	runner.ClearObjStore()
	defer runner.ClearObjStore()

	logger = runner.NewLogger("cache_xhaust_test")

	// Determine how the files should look in order to overflow the cache and loose the first
	// one
	bucket := "testcachexhaust"

	filesInCache := 10
	cacheMax := runner.ObjStoreFootPrint()
	fileSize := cacheMax / int64(filesInCache)

	// Create a single copy of a test file that will be uploaded multiple times
	tmpDir, fn, err := tmpDirFile(fileSize)
	if err != nil {
		t.Fatal(err.Error())
	}
	defer os.RemoveAll(tmpDir)

	// Recycle the same input file multiple times and upload, changing 1 byte
	// to get a different checksum in the cache for each one
	srcFn := fn
	for i := 1; i != filesInCache+2; i++ {

		key := fmt.Sprintf("%s-%02d", filepath.Base(fn), i)

		// Modify a single byte to force a change to the file hash
		f, errGo := os.OpenFile(srcFn, os.O_CREATE|os.O_WRONLY, 0644)
		if errGo != nil {
			t.Fatal(errors.Wrap(errGo).With("file", srcFn).With("stack", stack.Trace().TrimRuntime()))
		}
		if _, errGo = f.WriteAt([]byte{(byte)(i & 0xFF)}, 0); errGo != nil {
			t.Fatal(errors.Wrap(errGo).With("file", srcFn).With("stack", stack.Trace().TrimRuntime()))
		}
		if errGo = f.Close(); errGo != nil {
			t.Fatal(errors.Wrap(errGo).With("file", srcFn).With("stack", stack.Trace().TrimRuntime()))
		}

		// Upload
		if err := runner.MinioTest.Upload(bucket, key, srcFn); err != nil {
			t.Fatal(err.Error())
		}
		logger.Info(key, stack.Trace().TrimRuntime())
	}

	// Build an artifact cache in the same manner as is used by the main studioml
	// runner implementation
	artifactCache = runner.NewArtifactCache()

	art := runner.Artifact{
		Bucket:  bucket,
		Mutable: false,
		Unpack:  false,
	}
	env := map[string]string{
		"AWS_ACCESS_KEY_ID":     runner.MinioTest.AccessKeyId,
		"AWS_SECRET_ACCESS_KEY": runner.MinioTest.SecretAccessKeyId,
		"AWS_DEFAULT_REGION":    "us-west-2",
	}

	// Now begin downloading checking the misses do occur, the highest numbers file being
	// the least recently used
	for i := filesInCache + 1; i != 0; i-- {
		key := fmt.Sprintf("%s-%02d", filepath.Base(fn), i)

		art.Key = key
		art.Qualified = fmt.Sprintf("s3://%s/%s/%s", runner.MinioTest.Address, bucket, key)

		hash, err := artifactCache.Hash(&art, "project", tmpDir, "", env, "")
		if err != nil {
			t.Fatal(err)
		}

		// Extract the starting metrics for the server under going this test
		hits, misses, err := getHitsMisses(prometheusURL, hash)
		if err != nil {
			t.Fatal(err)
		}
		logger.Info(key, hash, stack.Trace().TrimRuntime())

		// In production the files would be downloaded to an experiment dir,
		// in the testing case we use a temporary directory as your artifact
		// group then wipe it when the test is done
		//
		warns, err := artifactCache.Fetch(&art, "project", tmpDir, "", env, "")
		if err != nil {
			for _, w := range warns {
				logger.Warn(w.Error())
			}
			t.Fatal(err)
		}
		newHits, newMisses, err := getHitsMisses(prometheusURL, hash)
		if err != nil {
			t.Fatal(err)
		}
		if hits != newHits {
			t.Fatal(errors.New("new file resulted in a hit when cache active").With("hash", hash).
				With("hits", hits).With("misses", misses).
				With("newHits", newHits).With("newMisses", newMisses).
				With("stack", stack.Trace().TrimRuntime()))
		}
		if misses+1 != newMisses {
			t.Fatal(errors.New("new file did not result in a miss when cache active").With("hash", hash).
				With("hits", hits).With("misses", misses).
				With("newHits", newHits).With("newMisses", newMisses).
				With("stack", stack.Trace().TrimRuntime()))
		}
	}

	// Now go back in reverse order downloading making sure we get
	// hits until the pen-ultimate file.  This means we have exercised
	// all files except for the highest numbered of all files
	for i := 2; i != filesInCache+1; i++ {
		key := fmt.Sprintf("%s-%02d", filepath.Base(fn), i)

		art.Key = key
		art.Qualified = fmt.Sprintf("s3://%s/%s/%s", runner.MinioTest.Address, bucket, key)

		hash, err := artifactCache.Hash(&art, "project", tmpDir, "", env, "")
		if err != nil {
			t.Fatal(err)
		}

		// Extract the starting metrics for the server under going this test
		hits, misses, err := getHitsMisses(prometheusURL, hash)
		if err != nil {
			t.Fatal(err)
		}

		logger.Info(key, hash, stack.Trace().TrimRuntime())

		// In production the files would be downloaded to an experiment dir,
		// in the testing case we use a temporary directory as your artifact
		// group then wipe it when the test is done
		//
		warns, err := artifactCache.Fetch(&art, "project", tmpDir, "", env, "")
		if err != nil {
			for _, w := range warns {
				logger.Warn(w.Error())
			}
			t.Fatal(err)
		}
		newHits, newMisses, err := getHitsMisses(prometheusURL, hash)
		if err != nil {
			t.Fatal(err)
		}
		if hits+1 != newHits {
			t.Fatal(errors.New("existing file did not result in a hit when cache active").With("hash", hash).
				With("hits", hits).With("misses", misses).
				With("newHits", newHits).With("newMisses", newMisses).
				With("stack", stack.Trace().TrimRuntime()))
		}
		if misses != newMisses {
			t.Fatal(errors.New("existing file resulted in a miss when cache active").With("hash", hash).
				With("hits", hits).With("misses", misses).
				With("newHits", newHits).With("newMisses", newMisses).
				With("stack", stack.Trace().TrimRuntime()))
		}
	}

	logger.Info("allowing the gc to kick in for the caching", stack.Trace().TrimRuntime())
	time.Sleep(10 * time.Second)

	// Check for a miss on the very last file that has been ignored for the longest
	i := filesInCache + 1
	key := fmt.Sprintf("%s-%02d", filepath.Base(fn), i)

	art.Key = key
	art.Qualified = fmt.Sprintf("s3://%s/%s/%s", runner.MinioTest.Address, bucket, key)

	hash, err := artifactCache.Hash(&art, "project", tmpDir, "", env, "")
	if err != nil {
		t.Fatal(err)
	}

	if runner.CacheProbe(hash) {
		t.Fatal(errors.New("cache still contained old key").With("hash", hash).
			With("stack", stack.Trace().TrimRuntime()))
	}

	logger.Info(key, hash, stack.Trace().TrimRuntime())

	// Extract the starting metrics for the server under going this test
	hits, misses, err := getHitsMisses(prometheusURL, hash)
	if err != nil {
		t.Fatal(err)
	}

	// In production the files would be downloaded to an experiment dir,
	// in the testing case we use a temporary directory as your artifact
	// group then wipe it when the test is done
	//
	warns, err := artifactCache.Fetch(&art, "project", tmpDir, "", env, "")
	if err != nil {
		for _, w := range warns {
			logger.Warn(w.Error())
		}
		t.Fatal(err)
	}
	newHits, newMisses, err := getHitsMisses(prometheusURL, hash)
	if err != nil {
		t.Fatal(err)
	}
	if hits != newHits {
		t.Fatal(errors.New("flushed file resulted in a hit when cache active").With("hash", hash).
			With("hits", hits).With("misses", misses).
			With("newHits", newHits).With("newMisses", newMisses).
			With("stack", stack.Trace().TrimRuntime()))
	}
	if misses+1 != newMisses {
		t.Fatal(errors.New("flushed file did not result in a miss when cache active").With("hash", hash).
			With("hits", hits).With("misses", misses).
			With("newHits", newHits).With("newMisses", newMisses).
			With("stack", stack.Trace().TrimRuntime()))
	}

	logger.Info("TestCacheXhaust completed")
}
