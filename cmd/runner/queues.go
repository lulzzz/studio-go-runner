package main

// This file contains the implementation of queue query functions along with
// tracking for queues to detect new arrivals and also to detect the
// disappearance of queues
//
// As queues come and go subscriptions are automatically created/accessed so that
// messages have a chance to be noticed

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SentientTechnologies/studio-go-runner"

	"github.com/davecgh/go-spew/spew"
	"github.com/dustin/go-humanize"

	"github.com/karlmutch/go-cache"

	"github.com/go-stack/stack"
	"github.com/karlmutch/errors"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// backoffs are a set of subscriptions to queues that when they are still alive
	// in the cache the server will not attempt to retrieve work from.  When the
	// cache entries that represent the subscriptions expire then they are
	// deemed to be ready for retrieving more work from.
	//
	// The TTL cache represents the signal to not do something, think of it as a
	// negative signal that has an expiry time.
	//
	// Create a cache with a default expiration time of 1 minute, and which
	// purges expired items every 10 seconds
	//
	backoffs = cache.New(10*time.Second, time.Minute)

	// busyQs is used to indicate when a worker is active for a named project:subscription so
	// that only one worker is activate at a time
	//
	busyQs = SubsBusy{subs: map[string]bool{}}

	refreshSuccesses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "queue_refresh_success",
			Help: "Number of successful queue inventory checks.",
		},
		[]string{"host", "project"},
	)
	refreshFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "queue_refresh_fail",
			Help: "Number of failed queue inventory checks.",
		},
		[]string{"host", "project"},
	)

	host = ""
)

func init() {
	host, _ = os.Hostname()

	if errGo := prometheus.Register(refreshSuccesses); errGo != nil {
		fmt.Fprintln(os.Stderr, errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()))
	}
	if errGo := prometheus.Register(refreshFailures); errGo != nil {
		fmt.Fprintln(os.Stderr, errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()))
	}
}

// Projects is used across several queuing modules for example the google pubsub and the rabbitMQ modules
//
type Projects struct {
	projects map[string]chan bool
	sync.Mutex
}

func (live *Projects) Lifecycle(found map[string]string) {

	if len(found) == 0 {
		return
	}

	// Place useful messages into the slack monitoring channel if available
	host := runner.GetHostName()

	// If projects have disappeared from the credentials then kill then from the
	// running set of projects if they are still running
	live.Lock()
	for proj, quiter := range live.projects {
		if _, isPresent := found[proj]; !isPresent {
			close(quiter)
			delete(live.projects, proj)
			logger.Info(fmt.Sprintf("%s no longer available [%s]", proj, stack.Trace().TrimRuntime()))
		}
	}
	live.Unlock()

	// Having checked for projects that have been dropped look for new projects
	for proj, cred := range found {
		live.Lock()
		if _, isPresent := live.projects[proj]; !isPresent {

			// Now start processing the queues that exist within the project in the background
			qr, err := NewQueuer(proj, cred)
			if err != nil {
				logger.Warn(err.Error())
				live.Unlock()
				continue
			}
			quiter := make(chan bool)
			live.projects[proj] = quiter

			// Start the projects runner and let it go off and do its thing until it dies
			// for no longer has a matching credentials file
			go func() {
				msg := fmt.Sprintf("started project %s on %s", proj, host)
				logger.Info(msg)

				runner.InfoSlack("", msg, []string{})
				if err := qr.run(5*time.Minute, quiter); err != nil {
					runner.WarningSlack("", fmt.Sprintf("terminating AWS project %s on %s due to %v", proj, host, err), []string{})
				} else {
					runner.WarningSlack("", fmt.Sprintf("stopping AWS project %s on %s", proj, host), []string{})
				}

				live.Lock()
				delete(live.projects, proj)
				live.Unlock()
			}()
		}
		live.Unlock()
	}
}

type SubsBusy struct {
	subs map[string]bool // The catalog of all known queues (subscriptions) within the project this server is handling
	sync.Mutex
}

type Subscription struct {
	name string           // The subscription name that represents a queue of potential for our purposes
	rsc  *runner.Resource // If known the resources that experiments asked for in this subscription
	cnt  uint             // The number of instances that are running for this queue
}

type Subscriptions struct {
	subs map[string]*Subscription // The catalog of all known queues (subscriptions) within the project this server is handling
	sync.Mutex
}

type Queuer struct {
	project string        // The project that is being used to access available work queues
	cred    string        // The credentials file associated with this project
	subs    Subscriptions // The subscriptions that exist within this project
	timeout time.Duration
	tasker  runner.TaskQueue
}

type SubRequest struct {
	project      string
	subscription string
	creds        string
}

func NewQueuer(projectID string, creds string) (qr *Queuer, err errors.Error) {
	qr = &Queuer{
		project: projectID,
		cred:    creds,
		subs:    Subscriptions{subs: map[string]*Subscription{}},
		timeout: 15 * time.Second,
	}
	qr.tasker, err = runner.NewTaskQueue(projectID, creds)
	if err != nil {
		return nil, err
	}
	return qr, nil

}

// refresh is used to update the queuer with a list of available queues
// accessible to the project specified by the queuer
//
func (qr *Queuer) refresh() (err errors.Error) {

	matcher, _ := regexp.Compile(*queueMatch)
	known, err := qr.tasker.Refresh(matcher, qr.timeout)
	if err != nil {
		refreshFailures.With(prometheus.Labels{"host": host, "project": qr.project}).Inc()
		return err
	}
	refreshSuccesses.With(prometheus.Labels{"host": host, "project": qr.project}).Inc()

	logger.Debug(fmt.Sprintf("on refresh got %#v", known))

	// Bring the queues collection uptodate with what the system has in terms
	// of functioning queues
	//
	added, removed := qr.subs.align(known)
	msg := ""
	if 0 != len(added) {
		msg += fmt.Sprintf("added queues %s", strings.Join(added, ", "))
	}
	if 0 != len(removed) {
		msg = strings.Join([]string{msg, fmt.Sprintf("removed queues %s", strings.Join(removed, ", "))}, ", and ")
	}
	if 0 != len(msg) {
		msg = fmt.Sprintf("project %s %s", qr.project, msg)
		logger.Info(msg)
		runner.InfoSlack("", msg, []string{})
	}
	return nil
}

// align allows the caller to take the extant subscriptions and add or remove them from the list of subscriptions
// we currently have cached
//
func (subs *Subscriptions) align(expected map[string]interface{}) (added []string, removed []string) {

	added = []string{}
	removed = []string{}

	subs.Lock()
	defer subs.Unlock()

	for sub := range expected {
		if _, isPresent := subs.subs[sub]; !isPresent {

			subs.subs[sub] = &Subscription{name: sub}
			added = append(added, sub)
		}
	}

	for sub := range subs.subs {
		if _, isPresent := expected[sub]; !isPresent {

			delete(subs.subs, sub)
			removed = append(removed, sub)
		}
	}

	return added, removed
}

// setResources is used to update the resources a queue will generally need for
// its individual work items
//
func (subs *Subscriptions) setResources(name string, rsc *runner.Resource) (err errors.Error) {
	if rsc == nil {
		return errors.New(fmt.Sprintf("clearing the resource spec for the subscription %s is not supported", name)).With("stack", stack.Trace().TrimRuntime())
	}

	subs.Lock()
	defer subs.Unlock()

	q, isPresent := subs.subs[name]
	if !isPresent {
		return errors.New(fmt.Sprintf("%s was not present", name)).With("stack", stack.Trace().TrimRuntime())
	}

	q.rsc = rsc

	return nil
}

// producer is used to examine the subscriptions that are available and determine if
// capacity is available to service any of the work that might be waiting
//
func (qr *Queuer) producer(rqst chan *SubRequest, quitC chan bool) {

	logger.Debug("started the queue checking producer")
	defer logger.Debug("stopped the queue checking producer")

	check := time.NewTicker(time.Duration(5 * time.Second))
	defer check.Stop()

	nextQDbg := time.Now()
	lastQs := 0

	lastReady := time.Now()
	lastReadyAbs := time.Now()

	for {
		select {
		case <-check.C:

			ranked := qr.rank()

			// Some monitoring logging used to tracking traffic on queues
			if logger.IsTrace() {
				if len(ranked) != 0 {
					logger.Trace(fmt.Sprintf("processing %s %d ranked subscriptions %s", qr.project, len(ranked), spew.Sdump(ranked)))
				} else {
					logger.Trace(fmt.Sprintf("no %s subscriptions found", qr.project))
				}
			} else {
				if logger.IsDebug() {
					// If either the queue length has changed, or sometime has passed since
					// the last debug log, one minute, print the queue checking state
					if nextQDbg.Before(time.Now()) || lastQs != len(ranked) {
						lastQs = len(ranked)
						nextQDbg = time.Now().Add(10 * time.Minute)
						if len(ranked) != 0 {
							logger.Debug(fmt.Sprintf("processing %d ranked subscriptions %v", len(ranked), ranked))
						} else {
							logger.Debug(fmt.Sprintf("no %s subscriptions found", qr.project))
						}
					}
				}
			}

			// track the first queue that has not been checked for the longest period of time that
			// also has no traffic on this node.  This queue will be check but it wont be until the next
			// pass that a new empty or idle queue will be checked.
			idle := []Subscription{}

			for _, sub := range ranked {
				// IDLE queue processing, that is queues that have no work running
				// against this runner
				if sub.cnt == 0 {
					if _, isPresent := backoffs.Get(qr.project + ":" + sub.name); isPresent {
						logger.Trace(fmt.Sprintf("backed off %s:%s", qr.project, sub.name))
						continue
					}
					// Save the queue that has been waiting the longest into the
					// idle slot that we will be processing on this pass
					idle = append(idle, sub)
				}
			}

			if len(idle) != 0 {

				// Shuffle the queues to pick one at random, fisher yates shuffle introduced in
				// go 1.10, c.f. https://golang.org/pkg/math/rand/#Shuffle
				rand.Shuffle(len(idle), func(i, j int) {
					idle[i], idle[j] = idle[j], idle[i]
				})

				if err := qr.check(idle[0].name, rqst, quitC); err != nil {

					backoffs.Set(qr.project+":"+idle[0].name, true, time.Duration(time.Minute))

					logger.Warn(fmt.Sprintf("checking %s for work failed due to %s, backoff 1 minute", qr.project+":"+idle[0].name, err.Error()))
					break
				}
				lastReady = time.Now()
				lastReadyAbs = time.Now()
			}

			// Check to see if we were last ready for work more than one hour ago as
			// this could be a resource problem
			if lastReady.Before(time.Now().Add(-1 * time.Hour)) {
				// If we have been unavailable for work alter slack once every 10 minutes and then
				// bump the ready timer for wait for another 10 before resending the advisory
				lastReady = lastReady.Add(10 * time.Minute)
				msg := fmt.Sprintf("no work has been requested by this system for %v, please check for disk space etc resource availability",
					time.Now().Sub(lastReadyAbs))
				runner.WarningSlack("", msg, []string{})
				logger.Warn(msg)
			}
		case <-quitC:
			return
		}
	}
}

func (qr *Queuer) getResources(name string) (rsc *runner.Resource) {
	qr.subs.Lock()
	defer qr.subs.Unlock()

	item, isPresent := qr.subs.subs[name]
	if !isPresent {
		return nil
	}
	return item.rsc.Clone()
}

// Retrieve the queues and count their occupancy, then sort ascending into
// an array
func (qr *Queuer) rank() (ranked []Subscription) {
	qr.subs.Lock()
	defer qr.subs.Unlock()

	ranked = make([]Subscription, 0, len(qr.subs.subs))
	for _, sub := range qr.subs.subs {
		ranked = append(ranked, *sub)
	}

	// sort the queues by their frequency of work, not their occupany of resources
	// so this is approximate but good enough for now
	//
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].cnt < ranked[j].cnt })

	return ranked
}

// getMachineResources extracts the current system state in terms of memory etc
// and coverts this into the resource specification used by jobs.  Because resources
// specified by users are not exact quantities the resource is used for the machines
// resources even in the face of some loss of precision
//
func getMachineResources() (rsc *runner.Resource) {

	rsc = &runner.Resource{}

	// For specified queue look for any free slots on existing GPUs is
	// applicable and fill them, or find empty GPUs and groups to fill
	// in with work

	cpus, v := runner.CPUFree()
	rsc.Cpus = uint(cpus)
	rsc.Ram = humanize.Bytes(v)

	rsc.Hdd = humanize.Bytes(runner.GetDiskFree())

	// go runner allows GPU resources at the board level so obtain the largest single
	// board form factor and use that as our max
	//
	rsc.Gpus = runner.LargestFreeGPUSlots()
	rsc.GpuMem = humanize.Bytes(runner.LargestFreeGPUMem())

	return rsc
}

// check will first validate a subscription and will add it to the list of subscriptions
// to be processed, which is in turn used by the scheduler later.
//
func (qr *Queuer) check(name string, rQ chan *SubRequest, quitC chan bool) (err errors.Error) {

	// fqName is the fully qualified name for the subscription
	fqName := qr.project + ":" + name

	// Check to see if anyone is listening for a queue to check by sending a dummy request, and then
	// send the real request if the check message is consumed
	select {
	case rQ <- &SubRequest{}:
	default:
		return errors.New("busy checking consumer, at the 1ˢᵗ stage").With("stack", stack.Trace().TrimRuntime())
	}

	sub, isPresent := qr.subs.subs[name]
	if !isPresent {
		return errors.New(fmt.Sprintf("subscription %s could not be found", fqName)).With("stack", stack.Trace().TrimRuntime())
	}

	if sub.rsc != nil {
		if fit, err := sub.rsc.Fit(getMachineResources()); !fit {
			if err != nil {
				return err
			}

			return errors.New(fmt.Sprintf("%s could not be accommodated %#v -> headroom was %#v", fqName, sub.rsc, getMachineResources())).With("stack", stack.Trace().TrimRuntime())
		} else {
			if logger.IsTrace() {
				logger.Trace(fmt.Sprintf("%s passed capacity check", fqName))
			}
		}
	} else {
		if logger.IsTrace() {
			logger.Trace(fmt.Sprintf("%s skipped capacity check", fqName))
		}
	}

	select {
	case rQ <- &SubRequest{project: qr.project, subscription: name, creds: qr.cred}:
	case <-time.After(2 * time.Second):
		return errors.New("busy checking consumer, at the 2ⁿᵈ stage").With("stack", stack.Trace().TrimRuntime())
	}

	// Check resource allocation availability to guide fetching work from queues
	// based upon the project ID we have been given
	/**
	gpus := map[string]runner.GPUTrack{}

	// First if we have gpuSlots and mem then look for free gpus slots for
	// the project and if we dont find project specific slots check if
	// we should be using an unassigned device
	if slots != 0 && gpuMem != 0 {
		// Look at GPU devices to see if we can identify bound queues to
		// cards with capacity and fill those, 1 at a time
		gpus = runner.FindGPUs(queue, slots, mem)
		if len(gpus) == 0 {
			gpus = runner.FindGPUs("", slots, mem)
			if len(gpus) == 0 {
				return nil
			}
		}
	}
	**/
	return nil
}

// run will execute maintenance operations in the back ground for the server looking for new
// or old subscriptions and adding them or removing them as needed
//
// This function will block except in the case a fatal issue occurs that prevents it
// from being able to perform the function that it is intended to do
//
func (qr *Queuer) run(refreshInterval time.Duration, quitC chan bool) (err errors.Error) {

	// Start a single unbuffered worker that we have for now to trigger for work
	sendWork := make(chan *SubRequest)
	go qr.consumer(sendWork, quitC)

	// start work producer that looks at subscriptions and then checks the
	// sendWork listener to ensure there is capacity

	go qr.producer(sendWork, quitC)

	refresh := time.Duration(time.Second)

	for {
		select {
		case <-time.After(refresh):
			if err := qr.refresh(); err != nil {
				return err
			}
			// Check for new queues or deleted queues once every few minutes
			refresh = time.Duration(refreshInterval)
		case <-quitC:
			return nil
		}
	}
}

func (qr *Queuer) consumer(readyC chan *SubRequest, quitC chan bool) {

	logger.Debug(fmt.Sprintf("started %s checking consumer", qr.project))
	defer logger.Debug(fmt.Sprintf("stopped %s checking consumer", qr.project))

	for {
		select {
		case request := <-readyC:
			// The channel looks to have been closed so stop handling work
			if request == nil {
				return
			}
			// An empty structure will be sent when the sender want to check if
			// the worker is ready for a scheduling request for a queue
			if len(request.subscription) == 0 {
				continue
			}
			go qr.filterWork(request, quitC)
		case <-quitC:
			return
		}
	}
}

// filterWork handles requests to check queues for work.  Before doing the work
// it will however also check to ensure that a backoff time is not in play
// for the queue, if it is then it will simply return
//
func (qr *Queuer) filterWork(request *SubRequest, quitC chan bool) {

	if _, isPresent := backoffs.Get(request.project + ":" + request.subscription); isPresent {
		logger.Trace(fmt.Sprintf("backoff on for %v", request))
		return
	}

	defer func() {
		if r := recover(); r != nil {
			logger.Warn(fmt.Sprintf("panic in filterWork %#v, %s", r, string(debug.Stack())))
		}
	}()

	busyQs.Lock()
	_, busy := busyQs.subs[request.project+":"+request.subscription]
	if !busy {
		busyQs.subs[request.project+":"+request.subscription] = true
	}
	busyQs.Unlock()

	if busy {
		logger.Trace(fmt.Sprintf("busy %v", request))
		return
	} else {
		logger.Trace(fmt.Sprintf("mark as busy %v", request))
	}

	defer func() {
		busyQs.Lock()
		delete(busyQs.subs, request.project+":"+request.subscription)
		busyQs.Unlock()

		logger.Trace(fmt.Sprintf("mark as free %v", request))
	}()

	qr.doWork(request, quitC)
}

func handleMsg(ctx context.Context, project string, subscription string, credentials string, msg []byte) (rsc *runner.Resource, consume bool) {

	rsc = nil

	defer func() {
		if r := recover(); r != nil {
			logger.Warn(fmt.Sprintf("%#v", r))
		}
	}()

	// Check for the back off and self destruct if one is seen for this subscription, leave the message for
	// redelivery upto the framework
	//
	// TODO Ack for PubSub Nack for SQS due to SQS supporting dead letter queues
	//
	if _, isPresent := backoffs.Get(project + ":" + subscription); isPresent {
		logger.Debug(fmt.Sprintf("stopping checking %s:%s backing off", project, subscription))
		return rsc, false
	}

	logger.Trace(fmt.Sprintf("msg processing started on %s:%s", project, subscription))
	defer logger.Trace(fmt.Sprintf("msg processing completed on %s:%s", project, subscription))

	// allocate the processor and sub the subscription as
	// the group mechanism for work coming down the
	// pipe that is sent to the resource allocation
	// module
	proc, err := newProcessor(subscription, msg, credentials, ctx.Done())
	if err != nil {
		logger.Warn(fmt.Sprintf("unable to process msg from %s:%s due to %s", project, subscription, err.Error()))

		backoffs.Set(project+":"+subscription, true, time.Duration(10*time.Second))
		return rsc, true
	}
	defer proc.Close()

	rsc = proc.Request.Experiment.Resource.Clone()

	header := fmt.Sprintf("%s:%s project %s experiment %s", project, subscription, proc.Request.Config.Database.ProjectId, proc.Request.Experiment.Key)
	logger.Info("started " + header)
	runner.InfoSlack(proc.Request.Config.Runner.SlackDest, "started "+header, []string{})

	// Used to cancel subsequent interactions if the context used by the queue system is cancelled.
	// Timeouts within the processor are not controlled by the queuing system
	prcCtx, prcCancel := context.WithCancel(context.Background())
	// Always cancel the operation, however we should ignore errors as these could
	// be already cancelled so we need to ignore errors at this point
	defer func() {
		defer func() {
			recover()
		}()
		prcCancel()
	}()
	// If the outer context gets cancelled cancel our inner context
	go func() {
		select {
		case <-ctx.Done():
			msg := fmt.Sprintf("%s:%s caller cancelled %s", project, subscription, proc.Request.Experiment.Key)
			logger.Info(msg)
			prcCancel()
		}
	}()

	// Blocking call to run the entire task and only return on termination due to error or success
	backoff, ack, err := proc.Process(prcCtx)
	if err != nil {

		// Do at least a minimal backoff
		if backoff == time.Duration(0) {
			backoff = time.Second
		}

		response := fmt.Sprintf(", backing off for %s, ", backoff)
		backoffs.Set(project+":"+subscription, true, backoff)

		if !ack {
			txt := fmt.Sprintf("%s retry%s due to %s", header, response, err.Error())

			runner.InfoSlack(proc.Request.Config.Runner.SlackDest, txt, []string{})
			logger.Info(txt)
		} else {
			txt := fmt.Sprintf("%s dumped%s due to %s", header, response, err.Error())

			runner.WarningSlack(proc.Request.Config.Runner.SlackDest, txt, []string{})
			logger.Warn(txt)
		}
		logger.Warn(err.Error())

		return rsc, ack
	}

	runner.InfoSlack(proc.Request.Config.Runner.SlackDest, header+" stopped", []string{})

	// At this point we could look for a backoff for this queue and set it to a small value as we are about to release resources
	if _, isPresent := backoffs.Get(project + ":" + subscription); isPresent {
		backoffs.Set(project+":"+subscription, true, time.Second)
	}
	return rsc, ack
}

func (qr *Queuer) doWork(request *SubRequest, quitC chan bool) {

	if _, isPresent := backoffs.Get(request.project + ":" + request.subscription); isPresent {
		logger.Trace(fmt.Sprintf("%v, backed off", request))
		return
	}

	logger.Trace(fmt.Sprintf("started checking %#v", *request))
	defer logger.Trace(fmt.Sprintf("stopped checking for %#v", *request))

	defer func() {
		if r := recover(); r != nil {
			logger.Warn(fmt.Sprintf("panic running studioml script %#v, %s", r, string(debug.Stack())))
		}
	}()

	// cCTX could be used with a timeout later to have a global limit on runtimes
	cCtx, cCancel := context.WithCancel(context.Background())
	// The cancel is called explicitly below due to GC and defers being delayed

	go func() {
		logger.Trace(fmt.Sprintf("started queue check %#v", *request))
		defer logger.Trace(fmt.Sprintf("completed queue check for %#v", *request))

		// Spins out a go routine to handle messages
		cnt, rsc, err := qr.tasker.Work(cCtx, qr.timeout, request.subscription, handleMsg)

		cCancel()

		if err != nil {
			backoffTime := time.Duration(2 * time.Minute)
			logger.Warn(fmt.Sprintf("backing off %v, %v msg receive failed due to %s", backoffTime,
				request, strings.Replace(fmt.Sprint(err), "\n", "", 0)))
			backoffs.Set(request.project+":"+request.subscription, true, backoffTime)
			return
		}

		// Set the default resource requirements for the next message fetch to that of the most recently
		// seen resource request
		//
		if rsc == nil {
			if cnt > 0 {
				logger.Warn(fmt.Sprintf("%#v handled msg that lacked a resource spec", *request))

				backoffTime := time.Duration(2 * time.Minute)
				logger.Warn(fmt.Sprintf("backing off %v, %v msg resource empty", backoffTime,
					request))
				backoffs.Set(request.project+":"+request.subscription, true, backoffTime)
			}
			return
		}
		if err = qr.subs.setResources(request.subscription, rsc); err != nil {
			logger.Info(fmt.Sprintf("%s:%s resources not updated due to %s", request.project, request.subscription, err))
		}

	}()

	// While waiting for this check periodically that the queue that
	// was used to send the message still exists, if it does not cancel
	// everything as this is an indication that the work is intended to
	// be stopped in a minute or so
	func() {
		check := time.NewTicker(5 * time.Minute)
		defer check.Stop()

		for {
			select {
			case <-check.C:
				eCtx, eCancel := context.WithTimeout(context.Background(), qr.timeout)
				// Is the queue still there that the job came in on, TODO the state information
				// can be obtainer from the queue refresher in the refresh() function
				exists, err := qr.tasker.Exists(eCtx, request.subscription)
				eCancel()

				if err != nil {
					logger.Info(fmt.Sprintf("%s:%s could not be validated due to %s", request.project, request.subscription, err))
					continue
				}
				if !exists {
					logger.Warn(fmt.Sprintf("%s:%s no longer found cancelling running tasks", request.project, request.subscription))
					// If not simply return which will cancel the context being used to manage the
					// lifecycle of task processing
					return
				}

			case <-cCtx.Done():
				return
			case <-quitC:
				return
			}
		}
	}()

	cCancel()
}
