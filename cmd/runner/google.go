package main

// The file contains code for handling google certificates and
// refreshing a directory containing these certificates and using
// these to process work sent to pubsub queues that get forwarded
// to subscriptions made by the runner

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/net/context"

	"cloud.google.com/go/pubsub"
	"google.golang.org/api/option"

	"github.com/go-stack/stack"
	"github.com/karlmutch/errors"
)

var (
	jsonMatch = regexp.MustCompile(`\.json$`)
)

type googleCred struct {
	CredType string `json:"type"`
	Project  string `json:"project_id"`
}

func (*googleCred) validateCred(ctx context.Context, filename string, scopes []string) (project string, err errors.Error) {

	b, errGo := ioutil.ReadFile(filename)
	if errGo != nil {
		return "", errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()).With("file", filename)
	}

	cred := &googleCred{}
	if errGo = json.Unmarshal(b, cred); errGo != nil {
		return "", errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()).With("file", filename)
	}

	if len(cred.Project) == 0 {
		return "", errors.New("bad file format for credentials").With("stack", stack.Trace().TrimRuntime()).With("file", filename)
	}

	client, errGo := pubsub.NewClient(ctx, cred.Project, option.WithCredentialsFile(filename))
	if errGo != nil {
		return "", errors.Wrap(errGo, "could not verify credentials").With("stack", stack.Trace().TrimRuntime()).With("file", filename)
	}
	client.Close()

	return cred.Project, nil
}

func refreshGoogleCerts(dir string, timeout time.Duration) (found map[string]string) {

	found = map[string]string{}

	// it is possible that google certificates are not being used currently so simply return
	// if we are not going to find any
	if _, err := os.Stat(dir); err != nil {
		logger.Trace(fmt.Sprintf("%v", err.Error()))
		return found
	}

	gCred := &googleCred{}

	filepath.Walk(dir, func(path string, f os.FileInfo, _ error) error {
		if !f.IsDir() {
			if jsonMatch.MatchString(f.Name()) {
				// Check if this is a genuine credential
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()

				project, err := gCred.validateCred(ctx, path, []string{})
				if err != nil {
					logger.Warn(err.Error())
					return nil
				}

				// If so include it
				found[project] = path
			} else {
				logger.Trace(fmt.Sprintf("did not match %s (%s)", f.Name(), path))
			}
		}
		return nil
	})

	if len(found) == 0 {
		logger.Info(fmt.Sprintf("no google certs found at %s", dir))
	}

	return found
}

func servicePubsub(ctx context.Context, connTimeout time.Duration) {

	live := &Projects{projects: map[string]chan bool{}}

	// first time through make sure the credentials are checked immediately
	credCheck := time.Duration(time.Second)

	for {
		select {
		case <-ctx.Done():

			live.Lock()
			defer live.Unlock()

			// When shutting down stop all projects
			for _, quiter := range live.projects {
				close(quiter)
			}
			return

		case <-time.After(credCheck):
			credCheck = time.Duration(15 * time.Second)

			dir, errGo := filepath.Abs(*googleCertsDirOpt)
			if errGo != nil {
				logger.Warn(fmt.Sprintf("%#v", errGo))
				continue
			}

			found := refreshGoogleCerts(dir, connTimeout)

			if len(found) != 0 {
				logger.Trace(fmt.Sprintf("checking google certs in %s returned %v", dir, found))
				credCheck = time.Duration(time.Minute)
				continue
			}

			live.Lifecycle(found)
		}
	}
}
