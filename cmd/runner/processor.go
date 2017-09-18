package main

// This file contains the implementation of a main processing loop
// for handling pubsub messages and dispatching then after extracting data
// from firebase

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode"

	"cloud.google.com/go/pubsub"

	"github.com/dgryski/go-farm"
	"github.com/karlmutch/go-shortid"

	"github.com/SentientTechnologies/studio-go-runner"
	"github.com/davecgh/go-spew/spew"

	"github.com/dustin/go-humanize"
)

type processor struct {
	Group      string            `json:"group"` // A caller specific grouping for work that can share sensitive resources
	RootDir    string            `json:"root_dir"`
	ExprDir    string            `json:"expr_dir"`
	ExprSubDir string            `json:"expr_sub_dir"`
	ExprEnvs   map[string]string `json:"expr_envs"`
	Request    *runner.Request   `json:"request"` // merge these two fields, to avoid split data in a DB and some in JSON
	ready      chan bool         // Used by the processor to indicate it has released resources or state has changed
}

type TempSafe struct {
	dir string
	sync.Mutex
}

var (
	// This is a safety valve for when work should not be scheduled due to allocation
	// failing to get resources.  In these case we wait for another job to complete however
	// this might no occur for sometime and we might want to come back around and see
	// if a smaller job is available.  But we only do this after a backoff period to not
	// hammer queues relentlessly
	//
	errBackoff = time.Duration(5 * time.Minute)

	// Used to store machine resource prfile
	resources = &runner.Resources{}

	// tempRoot is used to store information about the root directory uses by the
	// runner
	tempRoot = TempSafe{}
)

func init() {
	res, err := runner.NewResources(*tempOpt)
	if err != nil {
		logger.Fatal(fmt.Sprintf("could not initialize disk space tracking due to %s", err.Error()))
	}
	resources = res

	// A cache exists on linux for cuda lets remove it as it
	// can cause issues
	err = os.RemoveAll("$HOME/.nv")
	if err != nil {
		logger.Fatal(fmt.Sprintf("could not clear the $HOME/.nv cache due to %s", err.Error()))
	}
}

// newProcessor will create a new working directory
//
func newProcessor(group string, msg *pubsub.Message) (p *processor, err error) {

	// Singleton style initialization to instantiate and overridding directory
	// for the entire server working area
	//
	temp := ""
	tempRoot.Lock()
	if tempRoot.dir == "" {
		if id, errId := shortid.Generate(); err != nil {
			err = errId
		} else {
			tempRoot.dir, err = ioutil.TempDir(*tempOpt, "gorun_"+id)
		}
	}
	if err != nil {
		err = fmt.Errorf("generating a signature dir failed due to %v", err)
	} else {
		temp = tempRoot.dir
	}
	tempRoot.Unlock()
	if err != nil {
		return nil, err
	}

	p = &processor{
		Group: group,
		ready: make(chan bool),
	}

	id, err := shortid.Generate()
	if err != nil {
		return nil, err
	}

	// Get a location for running the test.  shortid will generate 9 character
	// unique identifiers that will remain unique until 2050, and are unique
	// to ms granularity
	//
	p.RootDir, err = ioutil.TempDir(temp, id)
	if err != nil {
		return nil, err
	}

	p.Request, err = runner.UnmarshalRequest(msg.Data)
	if err != nil {
		return nil, err
	}

	return p, nil
}

// Close will release all resources and clean up the work directory that
// was used by the studioml work
//
func (p *processor) Close() (err error) {
	if *debugOpt {
		return nil
	}

	return os.RemoveAll(p.RootDir)
}

// makeScript is used to write a script file that is generated for the specific TF tasks studioml has sent
// to retrieve any python packages etc then to run the task
//
func (p *processor) makeScript(fn string) (err error) {

	// Create a shell script that will do everything needed to run
	// the python environment in a virtual env
	tmpl, err := template.New("pythonRunner").Parse(
		`#!/bin/bash -x
date
{{range $key, $value := .Request.Config.Env}}
export {{$key}}="{{$value}}"
{{end}}
{{range $key, $value := .ExprEnvs}}
export {{$key}}="{{$value}}"
{{end}}
export LD_LIBRARY_PATH=$LD_LIBRARY_PATH:/usr/local/cuda/lib64/:/usr/lib/x86_64-linux-gnu:/lib/x86_64-linux-gnu/
mkdir {{.RootDir}}/blob-cache
mkdir {{.RootDir}}/queue
mkdir {{.RootDir}}/artifact-mappings
mkdir {{.RootDir}}/artifact-mappings/{{.Request.Experiment.Key}}
cd {{.ExprDir}}/workspace
virtualenv --system-site-packages -p /usr/bin/python2.7 .
source bin/activate
pip install {{range .Request.Config.Pip}}{{.}} {{end}}
{{range .Request.Experiment.Pythonenv}}
{{if ne . "studioml=="}}pip install {{.}}{{end}}{{end}}
if [ "` + "`" + `echo dist/studioml-*.tar.gz` + "`" + `" != "dist/studioml-*.tar.gz" ]; then
    pip install dist/studioml-*.tar.gz
else
    pip install studioml
fi
export STUDIOML_EXPERIMENT={{.ExprSubDir}}
export STUDIOML_HOME={{.RootDir}}
python {{.Request.Experiment.Filename}} {{range .Request.Experiment.Args}}{{.}} {{end}}
deactivate
date
`)

	if err != nil {
		return err
	}

	content := new(bytes.Buffer)
	err = tmpl.Execute(content, p)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(fn, content.Bytes(), 0744)
}

// runScript will use a generated script file and will run it to completion while marshalling
// results and files from the computation
//
func (p *processor) runScript(ctx context.Context, fn string, refresh map[string]runner.Modeldir) (err error) {

	cmd := exec.Command("/bin/bash", "-c", fn)
	cmd.Dir = path.Dir(fn)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	input := make(chan string)
	defer close(input)

	outputFN := filepath.Join(p.ExprDir, "output", "output")
	f, err := os.Create(outputFN)
	if err != nil {
		logger.Info(err.Error())
		return err
	}

	stopCP := make(chan bool)

	go func(f *os.File, input chan string, stopWriter chan bool) {
		defer f.Close()
		for {
			select {
			case <-stopWriter:
				return
			case line := <-input:
				f.WriteString(line)
				f.WriteString("\n")
			}
		}
	}(f, input, stopCP)

	logger.Debug(fmt.Sprintf("logging %s to %s", fn, outputFN))

	if err = cmd.Start(); err != nil {
		logger.Debug(fmt.Sprintf("start failed leaving Process with code %s", err.Error()))
		return err
	}

	done := sync.WaitGroup{}
	done.Add(2)

	go func() {
		defer done.Done()
		time.Sleep(time.Second)
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			input <- s.Text()
		}
	}()

	go func() {
		defer done.Done()
		time.Sleep(time.Second)
		s := bufio.NewScanner(stderr)
		for s.Scan() {
			input <- s.Text()
		}
	}()

	// checkpointing the output will be disabled if the time period is crazy large and overflows our wait
	disableCP := true

	// On a regular basis we will flush the log and compress it for uploading to
	// AWS or Google Cloud Storage etc, use the interval specified in the meta data for the job
	//
	saveDuration := time.Duration(600 * time.Minute)
	if p.Request.Config.SaveWorkspaceFrequency >= 1 && p.Request.Config.SaveWorkspaceFrequency < 43800 {
		saveDuration = time.Duration(time.Duration(p.Request.Config.SaveWorkspaceFrequency) * time.Minute)
		disableCP = false
	}

	checkpoint := time.NewTicker(saveDuration)
	defer checkpoint.Stop()

	go func() {
		for {
			select {
			case <-stopCP:
				return
			case <-checkpoint.C:

				if disableCP {
					continue
				}

				for group, artifact := range refresh {
					p.returnOne(group, artifact)
				}

			}
		}
	}()

	done.Wait()
	close(stopCP)

	err = cmd.Wait()
	if err != nil {
		logger.Info(fmt.Sprintf("leaving Process with code %s", err.Error()))
	}
	return err
}

// fetchAll is used to retrieve from the storage system employed by studioml any and all available
// artifacts and to unpack them into the experiement directory
//
func (p *processor) fetchAll() (err error) {
	// Extract all available artifacts into subdirectories of the main experiment directory.
	//
	// The current convention is that the archives include the directory name under which
	// the files are unpacked in their table of contents
	//
	for group, artifact := range p.Request.Experiment.Artifacts {
		// Process the qualified URI and use just the path for now
		uri, err := url.ParseRequestURI(artifact.Qualified)
		if err != nil {
			return err
		}
		path := strings.TrimLeft(uri.EscapedPath(), "/")
		dest := filepath.Join(p.ExprDir, group)
		if err = os.MkdirAll(dest, 0777); err != nil {
			return err
		}

		var storage runner.Storage
		switch uri.Scheme {
		case "gs":
			storage, err = runner.NewGSstorage(p.Request.Config.Database.ProjectId, p.ExprEnvs, artifact.Bucket, true, 15*time.Second)
		case "s3":
			storage, err = runner.NewS3storage(p.Request.Config.Database.ProjectId, p.ExprEnvs, uri.Host, artifact.Bucket, true, 15*time.Second)
		default:
			return fmt.Errorf("unknown URI scheme %s passed in studioml request", uri.Scheme)
		}
		if err != nil {
			return err
		}
		if err = storage.Fetch(artifact.Key, true, dest, 5*time.Second); err != nil {
			logger.Info(fmt.Sprintf("data not found for artifact %s using %s due to %s", group, path, err.Error()))
		} else {
			logger.Debug(fmt.Sprintf("extracted %s to %s", path, dest))
		}
		storage.Close()
	}
	return nil
}

// returnOne is used to upload a single artifact to the data store specified by the experimenter
//
func (p *processor) returnOne(group string, artifact runner.Modeldir) (err error) {
	uri, err := url.ParseRequestURI(artifact.Qualified)
	if err != nil {
		return err
	}
	path := strings.TrimLeft(uri.EscapedPath(), "/")

	var storage runner.Storage
	switch uri.Scheme {
	case "gs":
		storage, err = runner.NewGSstorage(p.Request.Config.Database.ProjectId, p.ExprEnvs, artifact.Bucket, true, 15*time.Second)
	case "s3":
		storage, err = runner.NewS3storage(p.Request.Config.Database.ProjectId, p.ExprEnvs, uri.Host, artifact.Bucket, true, 15*time.Second)
	default:
		return fmt.Errorf("unknown URI scheme %s passed in studioml request", uri.Scheme)
	}

	source := filepath.Join(p.ExprDir, group)
	logger.Debug(fmt.Sprintf("returning %s to %s", source, path))
	if err = storage.Deposit(source, artifact.Key, 5*time.Minute); err != nil {
		logger.Warn(fmt.Sprintf("%s data not uploaded due to %s", group, err.Error()))
	}
	storage.Close()

	return nil
}

// returnAll creates tar archives of the experiments artifacts and then puts them
// back to the studioml shared storage
//
func (p *processor) returnAll() (err error) {

	returned := make([]string, 0, len(p.Request.Experiment.Artifacts))

	for group, artifact := range p.Request.Experiment.Artifacts {
		if artifact.Mutable {
			if err = p.returnOne(group, artifact); err != nil {
				return fmt.Errorf("%s could not be returned due to %v", artifact, err)
			}
		}
	}

	if len(returned) != 0 {
		logger.Info(fmt.Sprintf("project %s returning %s", p.Request.Config.Database.ProjectId, strings.Join(returned, ", ")))
	}

	return nil
}

// allocate is used to reserve the resources on the local host needed to handle the entire job as
// a highwater mark.
//
// The returned alloc structure should be used with the deallocate function otherwise resource
// leaks will occur.
//
func (p *processor) allocate() (alloc *runner.Allocated, err error) {

	rqst := runner.AllocRequest{
		Group: p.Group,
	}

	// Before continuing locate GPU resources for the task that has been received
	//
	if rqst.MaxGPUMem, err = runner.ParseBytes(p.Request.Config.Resource.GpuMem); err != nil {
		logger.Debug(fmt.Sprintf("could not handle the gpuMemory value %s due to %v", p.Request.Config.Resource.GpuMem, err))
		// TODO Add an output function here for Issues #4, https://github.com/SentientTechnologies/studio-go-runner/issues/4
		return nil, err
	}

	rqst.MaxGPU = uint(p.Request.Config.Resource.Gpus)

	rqst.MaxCPU = uint(p.Request.Config.Resource.Cpus)
	if rqst.MaxMem, err = humanize.ParseBytes(p.Request.Config.Resource.Ram); err != nil {
		return nil, err
	}
	if rqst.MaxDisk, err = humanize.ParseBytes(p.Request.Config.Resource.Hdd); err != nil {
		return nil, err
	}

	if alloc, err = resources.AllocResources(rqst); err != nil {
		logger.Info(fmt.Sprintf("alloc %s failed due to %v", spew.Sdump(p.Request.Config.Resource), err))
		return nil, err
	}

	logger.Debug(fmt.Sprintf("alloc %s, gave %s", spew.Sdump(rqst), spew.Sdump(*alloc)))

	return alloc, err
}

// deallocate first releases resources and then triggers a ready channel to notify any listener that the
func (p *processor) deallocate(alloc *runner.Allocated) {

	if errs := alloc.Release(); len(errs) != 0 {
		for _, err := range errs {
			logger.Warn(fmt.Sprintf("dealloc %s rejected due to %v", spew.Sdump(*alloc), err))
		}
	} else {
		logger.Debug(fmt.Sprintf("released %s", spew.Sdump(*alloc)))
	}

	// Only wait a second to alter others that the resources have been released
	//
	select {
	case <-time.After(time.Second):
	case p.ready <- true:
	}
}

// ProcessMsg is the main function where experiment processing occurs.
//
// This function blocks.
//
func (p *processor) Process(msg *pubsub.Message) (wait time.Duration, err error) {

	// Call the allocation function to get access to resources and get back
	// the allocation we recieved
	alloc, err := p.allocate()
	if err != nil {
		err = fmt.Errorf("allocation fail backing off due to %v", err)
		logger.Debug(err.Error())
		return errBackoff, err
	}

	// Setup a function to release resources that have been allocated
	defer p.deallocate(alloc)

	// Use a panic handler to catch issues related to, or unrelated to the runner
	//
	defer func() {
		if r := recover(); r != nil {
			logger.Warn(fmt.Sprintf("panic running studioml script %#v, %s", r, string(debug.Stack())))
		}
	}()

	// The allocation details are passed in to the runner to allow the
	// resource reservations to become known to the running applications
	if err = p.run(alloc); err != nil {
		return time.Duration(0), err
	}

	return time.Duration(0), nil
}

// getHash produces a very simple and short hash for use in generating directory names from
// the experiment IDs assign by users to shorten the names and defang them
//
func getHash(text string) string {
	//	hasher := md5.New()
	//	hasher.Write([]byte(text))
	//	return hex.EncodeToString(hasher.Sum(nil))
	//
	// The stadtx hash could improve on this, see https://github.com/dgryski/go-stadtx.  However
	// it appears the impl was never set in stone and the author has disappeared from github
	//
	return fmt.Sprintf("%x", farm.Hash64([]byte(text)))
}

// mkUniqDir will create a working directory for an experiment
// using the file system calls appropriately so as to make sure
// no other instance of the same experiement is using it.  It is
// only being used by the caller and for which no race conditions
// during creation would have occurred.
//
// A new UUID could have been used to do this but that makes
// diagnosis of failed experiements very difficult so we keep a meaningful
// name for the new directory and use an index on the end of the experiment
// id so that during diagnosis we know exactly which attempts came first.
//
// There are lots of easier methods to create unique directories of course,
// but most involve using long unique identifies.
//
// This function will fill in the name being used into the structure being
// used for the method scope on success.
//
func (p *processor) mkUniqDir() (err error) {

	self, err := shortid.Generate()
	if err != nil {
		return fmt.Errorf("generating a signature dir failed due to %v", err)
	}

	// Shorten any excessively massively long names supplied by users
	expDir := getHash(p.Request.Experiment.Key)

	inst := 0
	for {
		// Loop until we fail to find a directory with the prefix
		for {
			p.ExprDir = filepath.Join(p.RootDir, "experiments", expDir+"."+strconv.Itoa(inst))
			if _, err = os.Stat(p.ExprDir); err == nil {
				logger.Trace(fmt.Sprintf("found collision %s for %d", p.ExprDir, inst))
				inst++
				continue
			}
			break
		}

		// Create the next directory in sequence with another directory containing our signature
		if err = os.MkdirAll(filepath.Join(p.ExprDir, self), 0777); err != nil {
			p.ExprDir = ""
			return err
		}

		logger.Trace(fmt.Sprintf("check for collision in %s", p.ExprDir))
		// After creation check to make sure our signature is the only file there, meaning no other entity
		// used the same experiment and instance
		files, err := ioutil.ReadDir(p.ExprDir)
		if err != nil {
			return err
		}
		if len(files) != 1 {
			logger.Debug(fmt.Sprintf("looking in what should be a single file inside our experiment and find %s", spew.Sdump(files)))
			// Increment the instance for the next pass
			inst++

			// Backoff for a small amount of time, less than a second then attempt again
			<-time.After(time.Duration(rand.Intn(1000)) * time.Millisecond)
			logger.Debug(fmt.Sprintf("collision during creation of %s with %d files", p.ExprDir, len(files)))
			continue
		}
		p.ExprSubDir = expDir + "." + strconv.Itoa(inst)
		return nil
	}
}

// applyEnv is used to apply the contents of the env block specified by the studioml client into the
// runners environment table.
//
// this function is also used to examine the contents of the processor request environment variables and
// to resolve locally any environment variables that are present indicated by the %...% pairs.
// If the enclosed value is not an environment variable within the context of the runner then the
// text will be left untouched.
//
// This behavior is specific to the go runner at this time.
//
func (p *processor) applyEnv(alloc *runner.Allocated) (err error) {

	p.ExprEnvs = map[string]string{}
	for _, v := range os.Environ() {
		// After the first equal keep everything else together
		kv := strings.SplitN(v, "=", 2)
		// Extract the first unicode rune and test that it is a valid character for an env name
		envName := []rune(kv[0])
		if len(kv) == 2 && (unicode.IsLetter(envName[0]) || unicode.IsDigit(envName[0])) {
			kv[1] = strings.Replace(kv[1], "\"", "\\\"", -1)
			p.ExprEnvs[kv[0]] = kv[1]
		} else {
			// The underscore is always present and represents the CWD so dont print messages about it
			if envName[0] != '_' {
				logger.Debug(fmt.Sprintf("env var %s (%c) (%d) dropped due to conformance", kv[0], envName[0], len(kv)))
			}
		}
	}

	// Expand %...% pairs by iterating the env table for the process and explicitly replacing on each line
	re := regexp.MustCompile(`(?U)(?:\%(.*)*\%)+`)

	// Environment variables need to be applied here to assist in unpacking S3 files etc
	for k, v := range p.Request.Config.Env {

		for _, match := range re.FindAllString(v, -1) {
			if envV := os.Getenv(match[1 : len(match)-1]); len(envV) != 0 {
				v = strings.Replace(envV, match, envV, -1)
			}
		}
		// Update the processor env table with the resolved value
		p.Request.Config.Env[k] = v

		p.ExprEnvs[k] = v
	}
	// create the map into which customer environment variables will be added to
	// the experiment script
	//
	p.ExprEnvs["AWS_SDK_LOAD_CONFIG"] = "1"

	// Although we copy the env values to the runners env table through they done get
	// automatically included into the script this is done via the makeScript being given
	// a set of env variables as an array that will be written into the script using the receiever
	// contents.
	//
	if alloc.GPU != nil && len(alloc.GPU.Env) != 0 {
		for k, v := range alloc.GPU.Env {
			p.ExprEnvs[k] = v
		}
	}
	return nil
}

// run is called to execute the work unit
//
func (p *processor) run(alloc *runner.Allocated) (err error) {

	// Generates a working directory if successful and puts the name into the structure for this
	// method
	//
	if err = p.mkUniqDir(); err != nil {
		return err
	}

	if !*debugOpt {
		defer os.RemoveAll(p.ExprDir)
	}

	// Update and apply environment variables for the experiment
	if err = p.applyEnv(alloc); err != nil {
		return err
	}

	if *debugOpt {
		// The following log can expose passwords etc.  As a result we do not allow it unless the debug
		// non production flag is explicitly set
		logger.Trace(fmt.Sprintf("experiment → %s → %s →  %s", p.Request.Experiment, p.ExprDir, spew.Sdump(p.Request)))
	}

	// fetchAll when called will have access to the environment variables used by the experiment in order that
	// credentials can be used
	if err = p.fetchAll(); err != nil {
		return err
	}

	id, err := shortid.Generate()
	if err != nil {
		return fmt.Errorf("generating a script file name failed due to %v", err)
	}

	script := filepath.Join(p.ExprDir, "workspace", id+".sh")

	// Now we have the files locally stored we can begin the work
	if err = p.makeScript(script); err != nil {
		// TODO: We could push work back onto the queue at this point if needed
		return err
	}

	refresh := make(map[string]runner.Modeldir, len(p.Request.Experiment.Artifacts))
	for k, v := range p.Request.Experiment.Artifacts {
		if v.Mutable {
			refresh[k] = v
		}
	}

	if err = p.runScript(context.Background(), script, refresh); err != nil {
		// TODO: We could push work back onto the queue at this point if needed
		return err
	}

	if err = p.returnAll(); err != nil {
		logger.Info(fmt.Sprintf("Failed to return all artifacts due to %s", err))
		return err
	}

	return nil
}
