package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/SentientTechnologies/studio-go-runner"

	"github.com/karlmutch/envflag"

	"github.com/go-stack/stack"
	"github.com/karlmutch/errors"

	"github.com/dustin/go-humanize"
)

var (
	TestMode = false

	buildTime string
	gitHash   string

	logger = runner.NewLogger("runner")

	amqpURL    = flag.String("amqp-url", "", "The URI for an amqp message exchange through which StudioML is being sent")
	queueMatch = flag.String("queue-match", "^(rmq|sqs)_.*$", "User supplied regular expression that needs to match a queues name to be considered for work")

	googleCertsDirOpt = flag.String("google-certs", "/opt/studioml/google-certs", "Directory containing certificate files used to access studio projects [Mandatory]. Does not descend.")
	tempOpt           = flag.String("working-dir", setTemp(), "the local working directory being used for runner storage, defaults to env var %TMPDIR, or /tmp")
	debugOpt          = flag.Bool("debug", false, "leave debugging artifacts in place, can take a large amount of disk space (intended for developers only)")
	cpuOnlyOpt        = flag.Bool("cpu-only", false, "in the event no gpus are found continue with only CPU support")

	maxCoresOpt = flag.Uint("max-cores", 0, "maximum number of cores to be used (default 0, all cores available will be used)")
	maxMemOpt   = flag.String("max-mem", "0gb", "maximum amount of memory to be allocated to tasks using SI, ICE units, for example 512gb, 16gib, 1024mb, 64mib etc' (default 0, is all available RAM)")
	maxDiskOpt  = flag.String("max-disk", "0gb", "maximum amount of local disk storage to be allocated to tasks using SI, ICE units, for example 512gb, 16gib, 1024mb, 64mib etc' (default 0, is 85% of available Disk)")
)

func setTemp() (dir string) {
	if dir = os.Getenv("TMPDIR"); len(dir) != 0 {
		return dir
	}
	if _, err := os.Stat("/tmp"); err == nil {
		dir = "/tmp"
	}
	return dir
}

type callInfo struct {
	packageName string
	fileName    string
	funcName    string
	line        int
}

func retrieveCallInfo() (info *callInfo) {
	info = &callInfo{}

	pc, file, line, _ := runtime.Caller(2)

	_, info.fileName = path.Split(file)
	info.line = line

	parts := strings.Split(runtime.FuncForPC(pc).Name(), ".")
	pl := len(parts)
	info.funcName = parts[pl-1]

	if parts[pl-2][0] == '(' {
		info.funcName = parts[pl-2] + "." + info.funcName
		info.packageName = strings.Join(parts[0:pl-2], ".")
	} else {
		info.packageName = strings.Join(parts[0:pl-1], ".")
	}

	return info
}

func usage() {
	fmt.Fprintln(os.Stderr, path.Base(os.Args[0]))
	fmt.Fprintln(os.Stderr, "usage: ", os.Args[0], "[arguments]      studioml runner      ", gitHash, "    ", buildTime)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Arguments:")
	fmt.Fprintln(os.Stderr, "")
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Environment Variables:")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "runner options can be read for environment variables by changing dashes '-' to underscores")
	fmt.Fprintln(os.Stderr, "and using upper case letters.  The certs-dir option is a mandatory option.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "To control log levels the LOGXI env variables can be used, these are documented at https://github.com/mgutz/logxi")
}

func resourceLimits() (cores uint, mem uint64, storage uint64, err error) {
	cores = *maxCoresOpt
	if mem, err = humanize.ParseBytes(*maxMemOpt); err != nil {
		return 0, 0, 0, err
	}
	if storage, err = humanize.ParseBytes(*maxDiskOpt); err != nil {
		return 0, 0, 0, err
	}
	return cores, mem, storage, err
}

// Go runtime entry point for production builds.  This function acts as an alias
// for the main.Main function.  This allows testing and code coverage features of
// go to invoke the logic within the server main without skipping important
// runtime initialization steps.  The coverage tools can then run this server as if it
// was a production binary.
//
// main will be called by the go runtime when the master is run in production mode
// avoiding this alias.
//
func main() {

	quitC := make(chan struct{})
	defer close(quitC)

	// This is the one check that does not get tested when the server is under test
	//
	if _, err := runner.NewExclusive("studio-go-runner", quitC); err != nil {
		logger.Error(fmt.Sprintf("An instance of this process is already running %s", err.Error()))
		os.Exit(-1)
	}

	Main()
}

// Production style main that will invoke the server as a go routine to allow
// a very simple supervisor and a test wrapper to coexist in terms of our logic.
//
// When using test mode 'go test ...' this function will not, normally, be run and
// instead the EntryPoint function will be called avoiding some initialization
// logic that is not applicable when testing.  There is one exception to this
// and that is when the go unit test framework is linked to the master binary,
// using a TestRunMain build flag which allows a binary with coverage
// instrumentation to be compiled with only a single unit test which is,
// infact an alias to this main.
//
func Main() {

	fmt.Printf("%s built at %s, against commit id %s\n", os.Args[0], buildTime, gitHash)

	flag.Usage = usage

	// Use the go options parser to load command line options that have been set, and look
	// for these options inside the env variable table
	//
	envflag.Parse()

	doneC := make(chan struct{})
	quitCtx, cancel := context.WithCancel(context.Background())

	if errs := EntryPoint(quitCtx, cancel, doneC); len(errs) != 0 {
		for _, err := range errs {
			logger.Error(err.Error())
		}
		os.Exit(-1)
	}

	// After starting the application message handling loops
	// wait until the system has shutdown
	//
	select {
	case <-quitCtx.Done():
	}

	// Allow the quitC to be sent across the server for a short period of time before exiting
	time.Sleep(time.Second)
}

// EntryPoint enables both test and standard production infrastructure to
// invoke this server.
//
// quitC can be used by the invoking functions to stop the processing
// inside the server and exit from the EntryPoint function
//
// doneC is used by the EntryPoint function to indicate when it has terminated
// its processing
//
func EntryPoint(quitCtx context.Context, cancel context.CancelFunc, doneC chan struct{}) (errs []errors.Error) {

	defer close(doneC)

	errs = []errors.Error{}

	logger.Trace(fmt.Sprintf("%#v", retrieveCallInfo()))

	// First gather any and as many errors as we can before stopping to allow one pass at the user
	// fixing things than than having them retrying multiple times

	if _, free := runner.GPUSlots(); free == 0 {
		if runner.HasCUDA() && !*cpuOnlyOpt && *runner.UseGPU {
			msg := "no available GPUs could be detected using the nvidia management library"
			errs = append(errs, errors.New(msg))
		}
	}

	if len(*tempOpt) == 0 {
		msg := "the working-dir command line option must be supplied with a valid working directory location, or the TEMP, or TMP env vars need to be set"
		errs = append(errs, errors.New(msg))
	}

	if _, _, err := getCacheOptions(); err != nil {
		errs = append(errs, errors.Wrap(err).With("stack", stack.Trace().TrimRuntime()))
	}

	// Attempt to deal with user specified hard limits on the CPU, this is a validation step for options
	// from the CLI
	//
	limitCores, limitMem, limitDisk, err := resourceLimits()
	if err != nil {
		errs = append(errs, errors.Wrap(err).With("stack", stack.Trace().TrimRuntime()))
	}

	if err = runner.SetCPULimits(limitCores, limitMem); err != nil {
		errs = append(errs, errors.Wrap(err, "the cores, or memory limits on command line option were invalid").With("stack", stack.Trace().TrimRuntime()))
	}
	avail, err := runner.SetDiskLimits(*tempOpt, limitDisk)
	if err != nil {
		errs = append(errs, errors.Wrap(err, "the disk storage limits on command line option were invalid").With("stack", stack.Trace().TrimRuntime()))
	} else {
		if 0 == avail {
			msg := fmt.Sprintf("insufficient disk storage available %s", humanize.Bytes(avail))
			errs = append(errs, errors.New(msg))
		} else {
			logger.Debug(fmt.Sprintf("%s available diskspace", humanize.Bytes(avail)))
		}
	}

	// Setup a channel to allow a CTRL-C to terminate all processing.  When the CTRL-C
	// occurs we cancel the background msg pump processing pubsub mesages from
	// google, and this will also cause the main thread to unblock and return
	//
	stopC := make(chan os.Signal)
	go func() {
		select {
		case <-quitCtx.Done():
			return
		case <-stopC:
			logger.Warn("CTRL-C Seen")
			cancel()
			return
		}
	}()

	signal.Notify(stopC, os.Interrupt, syscall.SIGTERM)

	// initialize the disk based artifact cache, after the signal handlers are in place
	//
	if err = runObjCache(quitCtx); err != nil {
		errs = append(errs, errors.Wrap(err))
	}

	// Make at least one of the credentials directories is valid, as long as this is not a test
	if TestMode {
		logger.Warn("running in test mode, queue validation not performed")
	} else {
		if len(*googleCertsDirOpt) == 0 && len(*sqsCertsDirOpt) == 0 && len(*amqpURL) == 0 {
			errs = append(errs, errors.New("One of the amqp-url, sqs-certs, or google-certs options must be set for the runner to work"))
		} else {
			stat, err := os.Stat(*googleCertsDirOpt)
			if err != nil || !stat.Mode().IsDir() {
				stat, err = os.Stat(*sqsCertsDirOpt)
				if err != nil || !stat.Mode().IsDir() {
					if len(*amqpURL) == 0 {
						msg := fmt.Sprintf(
							"One of the sqs-certs, or google-certs options must be set to an existing directory, or amqp-url is specified, for the runner to perform any useful work (%s,%s)",
							*googleCertsDirOpt, *sqsCertsDirOpt)
						errs = append(errs, errors.New(msg))
					}
				}
			}
		}
	}

	if len(*amqpURL) != 0 {
		if _, errGo := regexp.Compile(*queueMatch); errGo != nil {
			errs = append(errs, errors.Wrap(errGo))
		}
	}

	// Now check for any fatal errors before allowing the system to continue.  This allows
	// all errors that could have ocuured as a result of incorrect options to be flushed
	// out rather than having a frustrating single failure at a time loop for users
	// to fix things
	//
	if len(errs) != 0 {
		return errs
	}

	msg := fmt.Sprintf("git hash version %s", gitHash)
	logger.Info(msg)
	runner.InfoSlack("", msg, []string{})

	// loops printing out resource consumption statistics on a regular basis
	go showResources(quitCtx)

	// start the prometheus http server for metrics
	go func() {
		if err := runPrometheus(quitCtx); err != nil {
			logger.Warn(err.Error())
		}
	}()

	// Create a component that listens to a credentials directory
	// and starts and stops run methods as needed based on the credentials
	// it has for the Google cloud infrastructure
	//
	go servicePubsub(quitCtx, 15*time.Second)

	// Create a component that listens to AWS credentials directories
	// and starts and stops run methods as needed based on the credentials
	// it has for the AWS infrastructure
	//
	go serviceSQS(quitCtx, 15*time.Second)

	// Create a component that listens to an amqp (rabbitMQ) exchange for work
	// queues
	//
	go serviceRMQ(quitCtx, time.Minute, 15*time.Second)

	return nil
}
