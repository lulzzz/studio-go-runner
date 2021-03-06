package main

import (
	"context"
	"fmt"
	"regexp"
	"time"

	runner "github.com/SentientTechnologies/studio-go-runner"
)

// This file contains the implementation of a RabbitMQ service for
// retriving and handling StudioML workloads within a self hosted
// queue context

func serviceRMQ(ctx context.Context, checkInterval time.Duration, connTimeout time.Duration) {

	if len(*amqpURL) == 0 {
		logger.Info("rabbitMQ services disabled")
		return
	}

	live := &Projects{projects: map[string]chan bool{}}

	rmq, err := runner.NewRabbitMQ(*amqpURL, "")
	if err != nil {
		logger.Error(err.Error())
	}

	// The regular expression is validated in the main.go file
	matcher, _ := regexp.Compile(*queueMatch)

	// first time through make sure the credentials are checked immediately
	qCheck := time.Duration(time.Second)

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
		case <-time.After(qCheck):
			qCheck = checkInterval

			found, err := rmq.GetKnown(matcher, connTimeout)
			if err != nil {
				logger.Warn(fmt.Sprintf("unable to refresh RMQ queue manifest due to %v", err))
				qCheck = qCheck * 2
			}

			live.Lifecycle(found)
		}
	}
}
