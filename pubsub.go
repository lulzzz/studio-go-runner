package runner

// This file contains the implementation of googles PubSub message queues
// as they are used by studioML

import (
	"flag"
	"regexp"
	"sync/atomic"
	"time"

	"cloud.google.com/go/pubsub"
	"golang.org/x/net/context"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/go-stack/stack"
	"github.com/karlmutch/errors"
)

var (
	pubsubTimeoutOpt = flag.Duration("pubsub-timeout", time.Duration(5*time.Second), "the period of time discrete pubsub operations use for timeouts")
)

type PubSub struct {
	project string
	creds   string
}

func NewPubSub(project string, creds string) (ps *PubSub, err errors.Error) {
	return &PubSub{
		project: project,
		creds:   creds,
	}, nil
}

func (ps *PubSub) Refresh(qNameMatch *regexp.Regexp, timeout time.Duration) (known map[string]interface{}, err errors.Error) {

	known = map[string]interface{}{}

	ctx, cancel := context.WithTimeout(context.Background(), *pubsubTimeoutOpt)
	defer cancel()

	client, errGo := pubsub.NewClient(ctx, ps.project, option.WithCredentialsFile(ps.creds))
	if errGo != nil {
		return nil, errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime())
	}
	defer client.Close()

	// Get all of the known subscriptions in the project and make a record of them
	subs := client.Subscriptions(ctx)
	for {
		sub, errGo := subs.Next()
		if errGo == iterator.Done {
			break
		}
		if errGo != nil {
			return nil, errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime())
		}
		known[sub.ID()] = true
	}

	return known, nil
}

func (ps *PubSub) Exists(ctx context.Context, subscription string) (exists bool, err errors.Error) {
	client, errGo := pubsub.NewClient(ctx, ps.project, option.WithCredentialsFile(ps.creds))
	if errGo != nil {
		return true, errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()).With("project", ps.project)
	}
	defer client.Close()

	exists, errGo = client.Subscription(subscription).Exists(ctx)
	if errGo != nil {
		return true, errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()).With("project", ps.project)
	}
	return exists, nil
}

func (ps *PubSub) Work(ctx context.Context, qTimeout time.Duration, subscription string, handler MsgHandler) (msgs uint64, resource *Resource, err errors.Error) {

	client, errGo := pubsub.NewClient(ctx, ps.project, option.WithCredentialsFile(ps.creds))
	if errGo != nil {
		return 0, nil, errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime()).With("project", ps.project)
	}
	defer client.Close()

	sub := client.Subscription(subscription)
	sub.ReceiveSettings.MaxExtension = time.Duration(12 * time.Hour)

	errGo = sub.Receive(ctx,
		func(ctx context.Context, msg *pubsub.Message) {

			if rsc, ack := handler(ctx, ps.project, subscription, ps.creds, msg.Data); ack {
				msg.Ack()
				resource = rsc
			} else {
				msg.Nack()
			}
			atomic.AddUint64(&msgs, 1)
		})

	if errGo != nil {
		return msgs, nil, errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime())
	}

	return msgs, resource, nil
}
